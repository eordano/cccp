package pravda

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/api"
	"github.com/eordano/cccp/internal/store"
)

type SQLiteStore struct {
	store  *store.Store
	events *api.Broadcaster
}

func NewSQLiteStore(s *store.Store, b *api.Broadcaster) *SQLiteStore {
	return &SQLiteStore{store: s, events: b}
}

func (s *SQLiteStore) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	paths, err := s.store.LogPaths(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SessionMeta, 0, len(paths))
	for _, lp := range paths {
		if !strings.HasSuffix(lp.Path, ".jsonl") {
			continue
		}
		out = append(out, SessionMeta{
			Path:      lp.Path,
			Size:      lp.TotalSize,
			LineCount: lp.LineCount,
			ModTime:   time.Unix(0, lp.LastTime),
			Host:      lp.Host,
		})
	}
	return out, nil
}

func (s *SQLiteStore) SessionLines(ctx context.Context, path string, after int64) ([][]byte, int64, error) {
	if after == 0 {
		return s.store.LogContents(ctx, path)
	}
	return s.store.LogLinesAfter(ctx, path, after)
}

func (s *SQLiteStore) LoadFacts(ctx context.Context) (map[string]cachedFacts, error) {
	rows, err := s.store.AllPathFacts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]cachedFacts, len(rows))
	for p, r := range rows {
		f := &sessionFacts{}
		if err := json.Unmarshal(r.Facts, f); err != nil {
			continue
		}
		out[p] = cachedFacts{facts: f, lines: r.LineCount}
	}
	return out, nil
}

func (s *SQLiteStore) SaveFacts(ctx context.Context, path string, lineCount int, f *sessionFacts) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return s.store.PutPathFacts(ctx, path, lineCount, b)
}

func (s *SQLiteStore) OpenSession(ctx context.Context, path string) (io.ReadCloser, error) {
	lines, _, err := s.store.LogContents(ctx, path)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, &pathNotFoundError{path}
	}

	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return io.NopCloser(&buf), nil
}

func (s *SQLiteStore) Subscribe(ctx context.Context) <-chan string {
	ch := make(chan string, 64)
	go s.subscribeLoop(ctx, ch)
	return ch
}

func (s *SQLiteStore) subscribeLoop(ctx context.Context, out chan<- string) {
	defer close(out)
	sub := s.events.Subscribe()
	defer s.events.Unsubscribe(sub)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if ev.Kind != "log" {
				continue
			}
			for _, p := range ev.Paths {
				select {
				case out <- p:
				default:
				}
			}
		}
	}
}

func (s *SQLiteStore) OpenSessionHead(ctx context.Context, path string, limit int) (io.ReadCloser, error) {
	lines, err := s.store.LogHead(ctx, path, limit)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, &pathNotFoundError{path}
	}
	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return io.NopCloser(&buf), nil
}

type pathNotFoundError struct{ path string }

func (e *pathNotFoundError) Error() string { return "session not found: " + e.path }
