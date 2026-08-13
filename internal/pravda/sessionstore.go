package pravda

import (
	"context"
	"io"
	"time"
)

type SessionMeta struct {
	Path    string
	Size    int64
	ModTime time.Time
	Title   string
}

type SessionStore interface {
	ListSessions(ctx context.Context) ([]SessionMeta, error)

	OpenSession(ctx context.Context, path string) (io.ReadCloser, error)

	Subscribe(ctx context.Context) <-chan string
}
