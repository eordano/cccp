package pravda

import (
	"context"
	"io"
	"time"
)

type SessionMeta struct {
	Path      string
	Size      int64
	LineCount int
	ModTime   time.Time
	Title     string
	Host      string
	Facts     *sessionFacts
}

type sessionLineStore interface {
	SessionLines(ctx context.Context, path string, after int64) ([][]byte, int64, error)
}

type factsStore interface {
	LoadFacts(ctx context.Context) (map[string]cachedFacts, error)
	SaveFacts(ctx context.Context, path string, lineCount int, f *sessionFacts) error
}

type cachedFacts struct {
	facts *sessionFacts
	lines int
}

func innerStore(s SessionStore) SessionStore {
	for {
		switch t := s.(type) {
		case convertingHeadStore:
			s = t.SessionStore
		case convertingStore:
			s = t.SessionStore
		default:
			return s
		}
	}
}

type SessionStore interface {
	ListSessions(ctx context.Context) ([]SessionMeta, error)

	OpenSession(ctx context.Context, path string) (io.ReadCloser, error)

	Subscribe(ctx context.Context) <-chan string
}
