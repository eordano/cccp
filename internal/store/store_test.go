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

func TestLogPaths_SummaryFollowsInsertsAndBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.sqlite")
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertHost(ctx, "h1", "alpha"); err != nil {
		t.Fatal(err)
	}
	put := func(p, line, host string, when int64) {
		t.Helper()
		sum := sha256.Sum256([]byte(line))
		if _, err := s.AppendLogLine(ctx, LogLine{Path: p, LineHash: hex.EncodeToString(sum[:]), Contents: []byte(line), SuggestedTime: when, FirstHost: host}); err != nil {
			t.Fatal(err)
		}
	}
	put("a.jsonl", "one", "h1", 10)
	put("a.jsonl", "two-2", "h2", 30)
	put("a.jsonl", "three", "h2", 20)
	put("a.jsonl", "three", "h2", 20)
	put("b.jsonl", "b1", "h2", 5)

	check := func(st *Store) {
		t.Helper()
		got, err := st.LogPaths(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []LogPathInfo{
			{Path: "a.jsonl", LineCount: 3, LastTime: 30, TotalSize: 13, Host: "alpha"},
			{Path: "b.jsonl", LineCount: 1, LastTime: 5, TotalSize: 2, Host: "h2"},
		}
		if len(got) != len(want) {
			t.Fatalf("LogPaths = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("LogPaths[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	}
	check(s)

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM log_paths`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	check(s2)
	var summarized int
	if err := s2.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM log_paths`).Scan(&summarized); err != nil {
		t.Fatal(err)
	}
	if summarized != 2 {
		t.Errorf("log_paths rows after backfill = %d, want 2", summarized)
	}
}

func TestLogLinesAfter_CursorAndPathFacts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i, line := range []string{"a", "b", "c"} {
		if _, err := s.AppendLogLine(ctx, LogLine{Path: "p.jsonl", LineHash: fmt.Sprint(i), Contents: []byte(line), SuggestedTime: int64(i), FirstHost: "h"}); err != nil {
			t.Fatal(err)
		}
	}
	all, cursor, err := s.LogContents(ctx, "p.jsonl")
	if err != nil || len(all) != 3 || cursor == 0 {
		t.Fatalf("LogContents = %d lines, cursor %d, err %v", len(all), cursor, err)
	}
	more, c2, err := s.LogLinesAfter(ctx, "p.jsonl", cursor)
	if err != nil || len(more) != 0 || c2 != 0 {
		t.Fatalf("LogLinesAfter(cursor) = %d lines, cursor %d, err %v; want none", len(more), c2, err)
	}
	if _, err := s.AppendLogLine(ctx, LogLine{Path: "p.jsonl", LineHash: "3", Contents: []byte("d"), SuggestedTime: 3, FirstHost: "h"}); err != nil {
		t.Fatal(err)
	}
	more, c2, err = s.LogLinesAfter(ctx, "p.jsonl", cursor)
	if err != nil || len(more) != 1 || string(more[0]) != "d" || c2 <= cursor {
		t.Fatalf("LogLinesAfter after append = %q, cursor %d (prev %d), err %v", more, c2, cursor, err)
	}

	if err := s.PutPathFacts(ctx, "p.jsonl", 4, []byte(`{"Title":"t"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPathFacts(ctx, "p.jsonl", 5, []byte(`{"Title":"u"}`)); err != nil {
		t.Fatal(err)
	}
	facts, err := s.AllPathFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pf := facts["p.jsonl"]; pf.LineCount != 5 || string(pf.Facts) != `{"Title":"u"}` {
		t.Errorf("path_facts = %+v, want upserted row", pf)
	}
}
