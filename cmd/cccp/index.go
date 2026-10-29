package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/eordano/cccp/internal/pravda"
	"github.com/eordano/cccp/internal/store"
)

func runIndex(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cccp index rebuild|update|status [--db PATH]")
	}
	sub := args[0]
	fs, cli := newFlagSet("index " + sub)
	fresh := fs.Bool("fresh", false, "rebuild: start over even when a rebuild is in progress")
	mustParse(fs, args[1:])
	cfg := mustResolve(fs, cli)
	if _, err := os.Stat(cfg.DB); err != nil {
		return fmt.Errorf("store %s: %w", cfg.DB, err)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	ix := &pravda.Indexer{Store: st, Log: slog.Default()}
	start := time.Now()
	switch sub {
	case "rebuild":
		reset := true
		if m, ok, err := st.IndexMeta(ctx); err == nil && ok && m.RebuildActive && m.Version == pravda.IndexVersion && !*fresh {
			fmt.Fprintf(os.Stderr, "resuming the rebuild in progress (done through %q); --fresh starts over\n", m.RebuildPath)
			reset = false
		}
		pr, err := ix.Rebuild(ctx, reset, func(p pravda.RebuildProgress) {
			fmt.Fprintf(os.Stderr, "  %d sessions, %d lines, at %s (%s)\n", p.Paths, p.Lines, p.Done, p.Elapsed.Round(time.Second))
		})
		if err != nil {
			return err
		}
		n, err := ix.CatchUp(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("rebuilt: %d sessions, %d lines in %s; then %d newer lines\n", pr.Paths, pr.Lines, time.Since(start).Round(time.Millisecond), n)
	case "update":
		n, err := ix.CatchUp(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("folded %d lines in %s\n", n, time.Since(start).Round(time.Millisecond))
	case "status":
	default:
		return fmt.Errorf("unknown index command %q (rebuild, update, status)", sub)
	}
	m, ok, err := st.IndexMeta(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("index: never built")
		return nil
	}
	stats, err := st.IndexStats(ctx)
	if err != nil {
		return err
	}
	state := "following"
	if m.RebuildActive {
		state = "rebuilding, done through " + m.RebuildPath
	}
	fmt.Printf("index: v%d gen %d, %s; %d sessions (%d roots) over %d log paths, %d lines folded, %d file writes\n",
		m.Version, m.Gen, state, stats.Sessions, stats.Roots, stats.Paths, stats.Lines, stats.Writes)
	return nil
}
