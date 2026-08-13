package main

import (
	"cmp"
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/eordano/cccp/internal/pravda"
)

func runExplore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("explore", flag.ExitOnError)
	port := fs.String("port", cmp.Or(os.Getenv("PORT"), "9123"), "http port")
	fs.StringVar(port, "p", *port, "http port (shorthand)")
	mustParse(fs, args)

	watchDir := defaultWatchDir()
	if rest := fs.Args(); len(rest) > 0 {
		watchDir = rest[0]
	}

	store := pravda.NewFSStore(watchDir)
	srv := pravda.NewWatchServer(store)
	go srv.Run(ctx)

	slog.Info("explore listening", "addr", ":"+*port, "dir", store.Dir())
	return http.ListenAndServe(":"+*port, srv.Routes())
}

func defaultWatchDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(home, ".claude", "projects")
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return "."
}
