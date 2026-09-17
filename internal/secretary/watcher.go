package secretary

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

type scope struct {
	root  string
	dirs  map[string]bool
	files map[string]bool
}

func newScope(root string, allowlist []string) scope {
	s := scope{root: root, dirs: map[string]bool{}, files: map[string]bool{}}
	for _, e := range allowlist {
		if strings.HasSuffix(e, "/") {
			s.dirs[strings.TrimSuffix(e, "/")] = true
		} else {
			s.files[e] = true
		}
	}
	return s
}

func (s scope) interesting(p string) bool {
	rel, err := filepath.Rel(s.root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	rel = filepath.ToSlash(rel)
	if s.files[rel] {
		return true
	}
	top, _, _ := strings.Cut(rel, "/")
	return s.dirs[top]
}

func runWatcher(ctx context.Context, root, ignorePrefix string, allowlist []string, dirty chan<- struct{}, localDirty *atomic.Bool, log *slog.Logger) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Error("fsnotify new", "err", err)
		return
	}
	defer w.Close()

	sc := newScope(root, allowlist)

	addDir := func(p string) {
		if strings.HasPrefix(p, ignorePrefix) {
			return
		}
		if err := w.Add(p); err != nil {
			log.Debug("watch add", "path", p, "err", err)
		}
	}
	addTree := func(top string) {
		_ = filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				addDir(p)
			}
			return nil
		})
	}

	addDir(root)
	for d := range sc.dirs {
		addTree(filepath.Join(root, d))
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if strings.HasPrefix(ev.Name, ignorePrefix) || !sc.interesting(ev.Name) {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
					addTree(ev.Name)
				}
			}
			localDirty.Store(true)
			select {
			case dirty <- struct{}{}:
			default:
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Warn("fsnotify", "err", err)
		}
	}
}
