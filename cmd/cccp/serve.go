package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/eordano/cccp/internal/sae"
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
	saeDir := fs.String("sae-dir", "", "directory of daily SAE pages served under /sae/daily/")
	saeCaptureURL := fs.String("sae-capture-url", "", "capture service base URL for asynchronous SAE scoring")
	saeModelsDir := fs.String("sae-models-dir", "", "directory of SAE weights and original scales, keyed by encoder SHA256")
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
	watchSrv.SaeDir = *saeDir
	if ix, err := sae.New(st.DB()); err == nil {
		watchSrv.Sae = ix
		if *saeCaptureURL != "" || *saeModelsDir != "" {
			scorer, err := sae.NewOnDemand(ix, sae.ScorerConfig{CaptureURL: *saeCaptureURL, ModelsDir: *saeModelsDir})
			if err != nil {
				return fmt.Errorf("configure SAE scoring: %w", err)
			}
			watchSrv.SaeScorer = scorer
			go scorer.Start(ctx)
		}
	} else {
		slog.Warn("sae index unavailable", "error", err)
	}
	go watchSrv.Run(ctx)

	srv := &api.Server{
		Store:     st,
		Token:     cfg.Token,
		PushToken: cfg.PushToken,
		Log:       logger,
		Events:    events,
		Watch:     watchSrv.Routes(),
		SaeDir:    *saeDir,
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
