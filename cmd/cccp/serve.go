package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/eordano/cccp/internal/api"
	"github.com/eordano/cccp/internal/pravda"
	"github.com/eordano/cccp/internal/store"
)

func runServe(ctx context.Context, args []string) error {
	fs, cli := newFlagSet("serve")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := mustResolve(fs, cli)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if cfg.Token == "" {
		return fmt.Errorf("token is required (--token, $CCCP_TOKEN, or config.token)")
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DB), 0o755); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}

	st, err := store.Open(cfg.DB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	events := api.NewBroadcaster()
	sqlStore := pravda.NewSQLiteStore(st, events)
	watchSrv := pravda.NewWatchServer(sqlStore)
	go watchSrv.Run(ctx)

	srv := &api.Server{
		Store:  st,
		Token:  cfg.Token,
		Log:    logger,
		Events: events,
		Watch:  watchSrv.Routes(),
	}
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "db", cfg.DB)
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown", "err", err)
	}
	return nil
}
