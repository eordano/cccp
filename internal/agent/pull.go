package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type PullResult struct {
	TarFiles    int
	TarWritten  int
	TarBytes    int64
	TarRemoved  int
	LogFiles    int
	LogLinesNew int
	NewCursor   int64
}

func Pull(ctx context.Context, c *Config, full bool) (*PullResult, error) {
	res := &PullResult{}

	state, err := LoadState(c.Root)
	if err != nil {
		return nil, err
	}
	cursor := state.LogCursor
	if full {
		cursor = 0
	}

	seen, shas, err := pullCurrent(ctx, c, res)
	if err != nil {
		return nil, fmt.Errorf("pull current: %w", err)
	}
	if res.TarFiles == 0 && len(state.KnownPulledBlobPaths) > 0 {
		slog.Warn("pull: server snapshot is empty; refusing to remove previously pulled files",
			"known", len(state.KnownPulledBlobPaths))
	} else {
		for _, p := range state.KnownPulledBlobPaths {
			if _, ok := seen[p]; ok {
				continue
			}
			abs := filepath.Join(c.Root, p)
			if !safeToRemove(abs, state.PulledSHAs[p], state.BlobCache[p].SHA) {
				slog.Warn("pull: keeping locally modified file despite server tombstone", "path", p)
				continue
			}
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove tombstoned %s: %w", p, err)
			}
			res.TarRemoved++
		}
		state.KnownPulledBlobPaths = state.KnownPulledBlobPaths[:0]
		for p := range seen {
			state.KnownPulledBlobPaths = append(state.KnownPulledBlobPaths, p)
		}
		state.PulledSHAs = shas
	}

	newCursor, fullOffsets, err := pullLogs(ctx, c, res, cursor, full)
	if err != nil {
		return nil, fmt.Errorf("pull logs: %w", err)
	}
	if len(fullOffsets) > 0 {
		if state.LogOffsets == nil {
			state.LogOffsets = map[string]int64{}
		}
		for p, off := range fullOffsets {
			state.LogOffsets[p] = off
		}
	}
	if newCursor > state.LogCursor {
		state.LogCursor = newCursor
	}
	if err := SaveState(c.Root, state); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}
	res.NewCursor = state.LogCursor
	return res, nil
}

func pullCurrent(ctx context.Context, c *Config, res *PullResult) (map[string]struct{}, map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.ServerURL+"/current", nil)
	if err != nil {
		return nil, nil, err
	}
	c.AuthReq(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("/current %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	seen := map[string]struct{}{}
	shas := map[string]string{}
	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		res.TarFiles++
		seen[hdr.Name] = struct{}{}
		buf, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, err
		}
		sum := sha256.Sum256(buf)
		shas[hdr.Name] = hex.EncodeToString(sum[:])
		changed, err := writeIfDifferent(c.Root, hdr.Name, hdr.Mode, hdr.ModTime, buf)
		if err != nil {
			return nil, nil, err
		}
		if changed {
			res.TarWritten++
			res.TarBytes += int64(len(buf))
		}
	}
	return seen, shas, nil
}

func safeToRemove(abs string, knownSHAs ...string) bool {
	b, err := os.ReadFile(abs)
	if err != nil {
		return os.IsNotExist(err)
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	for _, k := range knownSHAs {
		if k != "" && k == sha {
			return true
		}
	}
	return false
}

func writeIfDifferent(root, rel string, mode int64, mtime time.Time, content []byte) (bool, error) {
	if err := safeRel(rel); err != nil {
		return false, err
	}
	abs := filepath.Join(root, rel)
	if existing, err := os.ReadFile(abs); err == nil && bytes.Equal(existing, content) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".ccd-tmp-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(os.FileMode(mode) & 0o7777); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(tmpName, mtime, mtime); err != nil {
			slog.Warn("chtimes", "path", rel, "err", err)
		}
	}
	return true, os.Rename(tmpName, abs)
}

func safeRel(rel string) error {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return fmt.Errorf("unsafe path: %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return fmt.Errorf("unsafe path: %q", rel)
		}
	}
	return nil
}

type logSinceLine struct {
	Path          string `json:"path"`
	Line          string `json:"line"`
	SuggestedTime int64  `json:"suggested_time"`
	FirstSeen     int64  `json:"first_seen"`
}

func pullLogs(ctx context.Context, c *Config, res *PullResult, since int64, full bool) (int64, map[string]int64, error) {
	url := c.ServerURL + "/log/since?since=" + strconv.FormatInt(since, 10)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	c.AuthReq(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, nil, fmt.Errorf("/log/since %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	byPath := map[string][]logSinceLine{}
	maxFirstSeen := since
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, ScanBufInit), MaxScanLine)
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var l logSinceLine
		if err := json.Unmarshal(raw, &l); err != nil {
			return 0, nil, err
		}
		byPath[l.Path] = append(byPath[l.Path], l)
		if l.FirstSeen > maxFirstSeen {
			maxFirstSeen = l.FirstSeen
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, nil, err
	}

	fullOffsets := map[string]int64{}
	for path, lines := range byPath {
		sort.SliceStable(lines, func(i, j int) bool {
			if lines[i].SuggestedTime != lines[j].SuggestedTime {
				return lines[i].SuggestedTime < lines[j].SuggestedTime
			}
			return lines[i].FirstSeen < lines[j].FirstSeen
		})
		var written int
		if full {
			var serverBytes int64
			written, serverBytes, err = rewriteLogFile(c.Root, path, lines)
			if err == nil {
				fullOffsets[path] = serverBytes
			}
		} else {
			written, err = mergeLogFile(c.Root, path, lines)
		}
		if err != nil {
			return 0, nil, err
		}
		res.LogFiles++
		res.LogLinesNew += written
	}
	return maxFirstSeen, fullOffsets, nil
}

func logFileAbs(root, rel string) (string, error) {
	if err := safeRel(rel); err != nil {
		return "", err
	}
	abs := filepath.Join(root, rel)
	return abs, os.MkdirAll(filepath.Dir(abs), 0o755)
}

func mergeLogFile(root, rel string, lines []logSinceLine) (int, error) {
	abs, err := logFileAbs(root, rel)
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(abs)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	existing := map[string]struct{}{}
	for _, raw := range bytes.Split(data, []byte{'\n'}) {
		if len(raw) > 0 {
			existing[hashLine(string(raw))] = struct{}{}
		}
	}

	f, err := os.OpenFile(abs, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	added := 0
	for _, l := range lines {
		if _, dup := existing[hashLine(l.Line)]; dup {
			continue
		}
		if _, err := f.WriteString(l.Line + "\n"); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

func rewriteLogFile(root, rel string, lines []logSinceLine) (int, int64, error) {
	abs, err := logFileAbs(root, rel)
	if err != nil {
		return 0, 0, err
	}

	serverSet := make(map[string]struct{}, len(lines))
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(l.Line)
		buf.WriteByte('\n')
		serverSet[hashLine(l.Line)] = struct{}{}
	}
	serverBytes := int64(buf.Len())

	data, err := os.ReadFile(abs)
	if err != nil && !os.IsNotExist(err) {
		return 0, 0, err
	}
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		for _, raw := range bytes.Split(data[:i], []byte{'\n'}) {
			if len(raw) == 0 {
				continue
			}
			if _, dup := serverSet[hashLine(string(raw))]; dup {
				continue
			}
			buf.Write(raw)
			buf.WriteByte('\n')
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(abs), ".ccd-tmp-*")
	if err != nil {
		return 0, 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return 0, 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, 0, err
	}
	return len(lines), serverBytes, os.Rename(tmpName, abs)
}

func hashLine(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
