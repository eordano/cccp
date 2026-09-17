package agent

import (
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
	"strings"
	"time"

	"github.com/eordano/cccp/internal/cheka"
)

type manifestLine struct {
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	MtimeNS int64  `json:"mtime_ns"`
}

type ingestResp struct {
	Need      []string `json:"need"`
	Committed int      `json:"committed"`
	Skipped   int      `json:"skipped"`
	Pending   int      `json:"pending"`
}

type logAppendEntry struct {
	Path          string `json:"path"`
	Line          string `json:"line"`
	SuggestedTime int64  `json:"suggested_time"`
}

type logAppendResp struct {
	New int `json:"new"`
	Dup int `json:"dup"`
}

var LogAppendChunkBytes = 16 << 20

type PushResult struct {
	BlobFiles       int
	BlobsUploaded   int
	BlobsCommitted  int
	BlobsSkipped    int
	BlobsDeleted    int
	LogFiles        int
	LogLinesNew     int
	LogLinesDup     int
	SecretsFound    int
	SecretsRedacted int
}

func Push(ctx context.Context, c *Config, allowlist []string, chekaAllow int, chekaRedact bool) (*PushResult, error) {
	if allowlist == nil {
		allowlist = c.DefaultAllowlist()
	}
	entries, skipped, err := Walk(c.Root, c.Prefix, allowlist)
	if err != nil {
		return nil, err
	}
	state, err := LoadState(c.Root)
	if err != nil {
		return nil, err
	}

	var blobs, logs []FileEntry
	for _, e := range entries {
		if e.IsLog {
			logs = append(logs, e)
		} else {
			blobs = append(blobs, e)
		}
	}

	res := &PushResult{BlobFiles: len(blobs), LogFiles: len(logs)}

	currentBlobPaths := map[string]struct{}{}
	for _, e := range blobs {
		currentBlobPaths[e.RelPath] = struct{}{}
	}
	for _, p := range skipped {
		if !IsLogPath(p) {
			currentBlobPaths[p] = struct{}{}
		}
	}
	var deletions []manifestLine
	for _, p := range state.KnownPushedBlobPaths {
		if _, ok := currentBlobPaths[p]; !ok {
			deletions = append(deletions, manifestLine{Path: p, SHA: ""})
		}
	}

	manifest := make([]manifestLine, 0, len(blobs)+len(deletions))
	shaByPath := map[string]string{}
	newCache := make(map[string]BlobCacheEntry, len(blobs))
	for _, p := range skipped {
		if cached, ok := state.BlobCache[p]; ok && !IsLogPath(p) {
			newCache[p] = cached
		}
	}
	var allFindings []cheka.Finding
	for _, e := range blobs {
		if e.Size > MaxPushBlobBytes {
			slog.Warn("push: skip oversized blob", "path", e.RelPath, "bytes", e.Size, "max", MaxPushBlobBytes)
			if cached, ok := state.BlobCache[e.RelPath]; ok {
				newCache[e.RelPath] = cached
			}
			continue
		}
		if cached, ok := state.BlobCache[e.RelPath]; ok && cached.MtimeNS == e.MtimeNS && cached.Size == e.Size && cached.SHA != "" {
			shaByPath[e.RelPath] = cached.SHA
			manifest = append(manifest, manifestLine{
				Path:    e.RelPath,
				SHA:     cached.SHA,
				Size:    e.Size,
				Mode:    e.Mode,
				MtimeNS: e.MtimeNS,
			})
			newCache[e.RelPath] = cached
			continue
		}
		content, err := os.ReadFile(e.AbsPath)
		if err != nil {
			slog.Warn("push: skip unreadable blob", "path", e.RelPath, "err", err)
			if cached, ok := state.BlobCache[e.RelPath]; ok {
				newCache[e.RelPath] = cached
			}
			continue
		}
		sum := sha256.Sum256(content)
		sha := hex.EncodeToString(sum[:])
		shaByPath[e.RelPath] = sha
		manifest = append(manifest, manifestLine{
			Path:    e.RelPath,
			SHA:     sha,
			Size:    e.Size,
			Mode:    e.Mode,
			MtimeNS: e.MtimeNS,
		})
		if ff := cheka.Scan(e.RelPath, content); len(ff) > 0 {
			allFindings = append(allFindings, ff...)
		}
		newCache[e.RelPath] = BlobCacheEntry{MtimeNS: e.MtimeNS, Size: e.Size, SHA: sha}
	}
	var logFindings []cheka.Finding
	for _, e := range logs {
		if ff := scanLogFile(e, state.LogOffsets[e.RelPath]); len(ff) > 0 {
			logFindings = append(logFindings, ff...)
		}
	}
	allFindings = append(allFindings, logFindings...)
	if len(allFindings) > 0 {
		res.SecretsFound = len(allFindings)

		type pk struct{ path, kind string }
		counts := map[pk]int{}
		for _, f := range allFindings {
			counts[pk{f.Path, f.Kind}]++
		}
		paths := map[string]struct{}{}
		msg := "cheka: secret detected"
		if chekaRedact {
			msg = "cheka: secret will be redacted"
		}
		for k, n := range counts {
			paths[k.path] = struct{}{}
			slog.Warn(msg, "path", k.path, "kind", k.kind, "count", n)
		}
		slog.Warn("cheka: scan complete", "total", len(allFindings), "files", len(paths))

		blockCount := len(allFindings)
		if chekaRedact {
			blockCount = len(allFindings) - len(logFindings)
			res.SecretsRedacted = len(logFindings)
		}
		if chekaAllow >= 0 && blockCount > chekaAllow {
			return res, fmt.Errorf("cheka: %d secret(s) found in blobs, %d allowed", blockCount, chekaAllow)
		}
	}

	manifest = append(manifest, deletions...)

	if len(manifest) > 0 {
		ir, err := postIngest(ctx, c, manifest)
		if err != nil {
			return nil, err
		}

		res.BlobsCommitted = ir.Committed
		res.BlobsSkipped = ir.Skipped

		if len(ir.Need) > 0 {
			needSet := map[string]struct{}{}
			for _, sha := range ir.Need {
				needSet[sha] = struct{}{}
			}
			for _, e := range blobs {
				sha := shaByPath[e.RelPath]
				if _, ok := needSet[sha]; !ok {
					continue
				}
				if err := putBlob(ctx, c, e.AbsPath, sha); err != nil {
					return nil, fmt.Errorf("upload %s: %w", e.RelPath, err)
				}
				res.BlobsUploaded++
				delete(needSet, sha)
			}
			ir2, err := postIngest(ctx, c, manifest)
			if err != nil {
				return nil, err
			}
			res.BlobsCommitted = ir2.Committed
			res.BlobsSkipped = ir2.Skipped
		}
	}
	res.BlobsDeleted = len(deletions)

	if state.LogOffsets == nil {
		state.LogOffsets = map[string]int64{}
	}
	var pushErrors []error
	nextOffsets := map[string]int64{}
	for _, e := range logs {
		off := state.LogOffsets[e.RelPath]

		if off > e.Size {
			off = 0
		}
		nNew, nDup, newOff, err := pushLogFile(ctx, c, e, chekaRedact, off)
		res.LogLinesNew += nNew
		res.LogLinesDup += nDup
		nextOffsets[e.RelPath] = newOff
		if err != nil {
			slog.Warn("push: log file push incomplete", "path", e.RelPath, "err", err)
			pushErrors = append(pushErrors, fmt.Errorf("push log %s: %w", e.RelPath, err))
		}
	}
	for _, p := range skipped {
		if off, ok := state.LogOffsets[p]; ok && IsLogPath(p) {
			nextOffsets[p] = off
		}
	}
	state.LogOffsets = nextOffsets

	state.KnownPushedBlobPaths = make([]string, 0, len(currentBlobPaths))
	for p := range currentBlobPaths {
		state.KnownPushedBlobPaths = append(state.KnownPushedBlobPaths, p)
	}
	state.BlobCache = newCache
	if err := SaveState(c.Root, state); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	return res, errors.Join(pushErrors...)
}

func postIngest(ctx context.Context, c *Config, manifest []manifestLine) (*ingestResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, m := range manifest {
		if err := enc.Encode(m); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.ServerURL+"/ingest", &body)
	if err != nil {
		return nil, err
	}
	c.AuthReq(req)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ingest %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var ir ingestResp
	if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil {
		return nil, err
	}
	return &ir, nil
}

func putBlob(ctx context.Context, c *Config, absPath, sha string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, "PUT", c.ServerURL+"/blob/"+sha, f)
	if err != nil {
		return err
	}
	if fi, err := f.Stat(); err == nil {
		req.ContentLength = fi.Size()
	}
	c.AuthReq(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("put blob %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func scanLogFile(e FileEntry, offset int64) []cheka.Finding {
	f, err := os.Open(e.AbsPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	if offset > e.Size {
		offset = 0
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	var findings []cheka.Finding
	for i, raw := range bytes.Split(data, []byte{'\n'}) {
		if len(raw) == 0 || len(raw) >= MaxScanLine {
			continue
		}
		if ff := cheka.ScanLine(e.RelPath, string(raw), i+1); len(ff) > 0 {
			findings = append(findings, ff...)
		}
	}
	return findings
}

func pushLogFile(ctx context.Context, c *Config, e FileEntry, redact bool, offset int64) (newCount, dupCount int, nextOffset int64, err error) {
	f, err := os.Open(e.AbsPath)
	if err != nil {
		return 0, 0, offset, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, 0, offset, err
		}
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return 0, 0, offset, err
	}
	lastNL := bytes.LastIndexByte(tail, '\n')
	if lastNL < 0 {

		return 0, 0, offset, nil
	}
	complete := tail[:lastNL+1]

	nextOffset = offset
	var processed int64
	var body bytes.Buffer
	flush := func() error {
		if body.Len() > 0 {
			n, d, err := postLogAppend(ctx, c, &body)
			if err != nil {
				return err
			}
			newCount += n
			dupCount += d
			body.Reset()
		}
		nextOffset = offset + processed
		return nil
	}
	for _, raw := range bytes.Split(complete[:len(complete)-1], []byte{'\n'}) {
		processed += int64(len(raw)) + 1
		if len(raw) == 0 {
			continue
		}
		if len(raw) >= MaxScanLine {
			slog.Warn("push: skip oversized log line", "path", e.RelPath, "bytes", len(raw))
			continue
		}
		line := string(raw)
		toSend := line
		if redact {
			toSend = cheka.RedactLine(line)
		}
		ts := extractTimestamp(toSend)
		b, err := json.Marshal(logAppendEntry{Path: e.RelPath, Line: toSend, SuggestedTime: ts})
		if err != nil {
			return newCount, dupCount, nextOffset, err
		}
		if len(b) >= MaxScanLine {
			slog.Warn("push: skip oversized log line", "path", e.RelPath, "bytes", len(b))
			continue
		}
		body.Write(b)
		body.WriteByte('\n')
		if body.Len() >= LogAppendChunkBytes {
			if err := flush(); err != nil {
				return newCount, dupCount, nextOffset, err
			}
		}
	}
	if err := flush(); err != nil {
		return newCount, dupCount, nextOffset, err
	}
	return newCount, dupCount, nextOffset, nil
}

func postLogAppend(ctx context.Context, c *Config, body *bytes.Buffer) (int, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", c.ServerURL+"/log/append", bytes.NewReader(body.Bytes()))
	if err != nil {
		return 0, 0, err
	}
	c.AuthReq(req)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("log append %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var lr logAppendResp
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return 0, 0, err
	}
	return lr.New, lr.Dup, nil
}
