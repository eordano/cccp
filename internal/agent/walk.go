package agent

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	ScanBufInit = 64 * 1024
	MaxScanLine = 4 * 1024 * 1024
)

var DefaultAllowlist = []string{
	"agents/",
	"commands/",
	"file-history/",
	"hooks/",
	"memory/",
	"paste-cache/",
	"plans/",
	"projects/",
	"skills/",
	"tasks/",
	"CLAUDE.md",
	"history.jsonl",
	"settings.json",
}

type FileEntry struct {
	RelPath string
	AbsPath string
	Mode    uint32
	Size    int64
	MtimeNS int64
	IsLog   bool
}

func IsLogPath(rel string) bool {
	return strings.HasSuffix(strings.ToLower(rel), ".jsonl")
}

func Walk(root string, allowlist []string) ([]FileEntry, []string, error) {
	var out []FileEntry
	var skipped []string
	keep := func(rel string, fe *FileEntry, err error) {
		if err != nil {
			slog.Warn("walk: skip unresolvable file", "path", rel, "err", err)
			skipped = append(skipped, filepath.ToSlash(rel))
			return
		}
		if fe != nil {
			out = append(out, *fe)
		}
	}
	for _, entry := range allowlist {
		recurse := strings.HasSuffix(entry, "/")
		base := strings.TrimSuffix(entry, "/")
		full := filepath.Join(root, base)

		info, err := os.Lstat(full)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("lstat %s: %w", base, err)
		}

		if !recurse {
			fe, err := resolveAndStat(root, base, info)
			keep(base, fe, err)
			continue
		}

		walkErr := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			li, err := os.Lstat(p)
			if err != nil {
				return err
			}
			fe, err := resolveAndStat(root, rel, li)
			keep(rel, fe, err)
			return nil
		})
		if walkErr != nil {
			return nil, nil, fmt.Errorf("walk %s: %w", base, walkErr)
		}
	}
	return out, skipped, nil
}

func resolveAndStat(root, rel string, li os.FileInfo) (*FileEntry, error) {
	abs := filepath.Join(root, rel)
	mode := li.Mode()

	if mode&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("resolve symlink %s: %w", rel, err)
		}
		ti, err := os.Stat(target)
		if err != nil {
			return nil, fmt.Errorf("stat symlink target %s: %w", rel, err)
		}
		if !ti.Mode().IsRegular() {
			return nil, nil
		}
		return &FileEntry{
			RelPath: filepath.ToSlash(rel),
			AbsPath: target,
			Mode:    uint32(ti.Mode().Perm()),
			Size:    ti.Size(),
			MtimeNS: ti.ModTime().UnixNano(),
			IsLog:   IsLogPath(rel),
		}, nil
	}

	if !mode.IsRegular() {
		return nil, nil
	}
	return &FileEntry{
		RelPath: filepath.ToSlash(rel),
		AbsPath: abs,
		Mode:    uint32(mode.Perm()),
		Size:    li.Size(),
		MtimeNS: li.ModTime().UnixNano(),
		IsLog:   IsLogPath(rel),
	}, nil
}
