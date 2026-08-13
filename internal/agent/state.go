package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type State struct {
	KnownPushedBlobPaths []string                  `json:"known_pushed_blob_paths"`
	KnownPulledBlobPaths []string                  `json:"known_pulled_blob_paths"`
	LogCursor            int64                     `json:"log_cursor"`
	LogOffsets           map[string]int64          `json:"log_offsets,omitempty"`
	BlobCache            map[string]BlobCacheEntry `json:"blob_cache,omitempty"`
	PulledSHAs           map[string]string         `json:"pulled_shas,omitempty"`
}

type BlobCacheEntry struct {
	MtimeNS int64  `json:"m"`
	Size    int64  `json:"s"`
	SHA     string `json:"h"`
}

func statePath(root string) string {
	return filepath.Join(root, "cccp", "state.json")
}

func LoadState(root string) (*State, error) {
	b, err := os.ReadFile(statePath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return &s, nil
}

func SaveState(root string, s *State) error {
	dir := filepath.Join(root, "cccp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	sort.Strings(s.KnownPushedBlobPaths)
	sort.Strings(s.KnownPulledBlobPaths)
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, statePath(root))
}
