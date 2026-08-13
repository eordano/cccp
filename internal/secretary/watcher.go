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

func runWatcher(ctx context.Context, root, ignorePrefix string, dirty chan<- struct{}, localDirty *atomic.Bool, log *slog.Logger) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Error("fsnotify new", "err", err)
		return
	}
	defer w.Close()

	addDir := func(p string) {
		if strings.HasPrefix(p, ignorePrefix) {
			return
		}
		if err := w.Add(p); err != nil {
			log.Debug("watch add", "path", p, "err", err)
		}
	}

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			addDir(p)
		}
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if strings.HasPrefix(ev.Name, ignorePrefix) {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {

				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
					addDir(ev.Name)
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
