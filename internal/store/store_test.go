package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestAppendLogLine_FirstSeenNeverBehindReadCursor(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	const writers = 4
	const perWriter = 100
	var wg sync.WaitGroup
	done := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				line := fmt.Sprintf("writer %d line %d", w, i)
				sum := sha256.Sum256([]byte(line))
				if _, err := s.AppendLogLine(ctx, LogLine{
					Path:      "p.jsonl",
					LineHash:  hex.EncodeToString(sum[:]),
					Contents:  []byte(line),
					FirstHost: "h",
				}); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	go func() { wg.Wait(); close(done) }()

	got := map[string]struct{}{}
	var cursor int64
	poll := func() {
		err := s.LogLinesSince(ctx, cursor, func(l LogLine) bool {
			got[l.LineHash] = struct{}{}
			if l.FirstSeen <= cursor {
				t.Errorf("first_seen %d not greater than cursor %d", l.FirstSeen, cursor)
			}
			cursor = l.FirstSeen
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for {
		select {
		case <-done:
			poll()
			if len(got) != writers*perWriter {
				t.Fatalf("cursor-following reader saw %d lines, want %d (permanently skipped lines)",
					len(got), writers*perWriter)
			}
			return
		default:
			poll()
		}
	}
}

func TestOpen_SeedsFirstSeenHighWater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.sqlite")
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendLogLine(ctx, LogLine{Path: "p.jsonl", LineHash: "aa", Contents: []byte("x"), FirstHost: "h"}); err != nil {
		t.Fatal(err)
	}
	var first int64
	if err := s.LogLinesSince(ctx, 0, func(l LogLine) bool { first = l.FirstSeen; return true }); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.lastFirstSeen != first {
		t.Errorf("lastFirstSeen = %d, want %d after reopen", s2.lastFirstSeen, first)
	}
}
