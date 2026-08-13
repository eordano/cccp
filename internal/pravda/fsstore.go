package pravda

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

type FSStore struct {
	dir string
}

func NewFSStore(dir string) *FSStore {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return &FSStore{dir: abs}
}

func (s *FSStore) Dir() string { return s.dir }

func (s *FSStore) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	var out []SessionMeta
	err := filepath.Walk(s.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		out = append(out, SessionMeta{
			Path:    rel,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
		return nil
	})
	return out, err
}

func (s *FSStore) OpenSession(_ context.Context, path string) (io.ReadCloser, error) {
	full := filepath.Join(s.dir, path)
	if !isInside(full, s.dir) {
		return nil, os.ErrPermission
	}
	return os.Open(full)
}

func (s *FSStore) Subscribe(ctx context.Context) <-chan string {
	ch := make(chan string, 64)
	go s.watchLoop(ctx, ch)
	return ch
}

func (s *FSStore) watchLoop(ctx context.Context, ch chan<- string) {
	defer close(ch)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("fsstore watcher: %v", err)
		return
	}
	defer w.Close()

	if err := addRecursive(w, s.dir); err != nil {
		log.Printf("fsstore watch setup: %v", err)
		return
	}

	lastPerPath := make(map[string]time.Time)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove) == 0 {
				continue
			}
			if now := time.Now(); now.Sub(lastPerPath[ev.Name]) >= 100*time.Millisecond {
				lastPerPath[ev.Name] = now
			} else {
				continue
			}
			if !strings.HasSuffix(ev.Name, ".jsonl") {

				if ev.Op&fsnotify.Create != 0 {
					if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
						_ = w.Add(ev.Name)
					}
				}
				continue
			}
			rel := strings.TrimPrefix(ev.Name, s.dir+string(os.PathSeparator))
			rel = filepath.ToSlash(rel)
			select {
			case ch <- rel:
			default:
			}

			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = w.Add(ev.Name)
				}
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Println("fsstore watch err:", err)
		}
	}
}

func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return w.Add(path)
		}
		return nil
	})
}
