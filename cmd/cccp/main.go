package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/eordano/cccp/internal/agent"
	"github.com/eordano/cccp/internal/config"
)

const usage = `cccp - cloud code config provider.

Usage:
  cccp serve [flags]    Run the storage server.
  cccp push  [flags]    Walk allowlist, ship blobs and log lines.
                        Tombstones paths previously pushed but now missing.
                        Fails if cheka finds secrets (--cheka-allow N to
                        tolerate up to N findings; -1 disables the check).
                        --cheka-redact: redact secrets in log files instead
                        of blocking (blobs with secrets still fail).
  cccp pull  [flags]    Incremental pull: only writes blob files whose
                        content differs from local; appends log lines newer
                        than the saved cursor (deduped against local).
  cccp pull --full      Bootstrap pull: rewrites every blob file from the
                        snapshot, replays all log lines from cursor=0.
  cccp watch [flags]    Watch ~/.claude and sync on change. Runs as a
                        blocking daemon: pushes on fsnotify, pulls on SSE.
  cccp watch hook-start   SessionStart hook (reads JSON on stdin).
  cccp watch hook-end     SessionEnd hook (reads JSON on stdin).
  cccp watch status       Print daemon status (PID, sessions).
  cccp watch stop         SIGTERM the daemon.
  cccp explore [dir]    Browse local sessions as rich timelines (standalone
                        viewer, reads ~/.claude/projects/ by default).
  cccp host-id          Print this host's persistent UUID.

Configuration (4 layers, later wins):
  defaults < config file < environment variables < CLI flags.
  An explicit empty string in a higher layer still overrides a lower one.

CLI flags (any subcommand):
  --dir PATH            config root directory (default $HOME/.claude)
  --url, --server-url U server URL, e.g. https://host:12110
  --token TOKEN         bearer token
  --addr, --address A   listen address (default :12110)
  --port N              listen port; overrides the port in --addr
  --db, --database,
  --db-path PATH        sqlite database path (default <dir>/cccp/store.sqlite)
  --config PATH         path to config file (default <dir>/cccp/config.json)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		if err := runServe(ctx, os.Args[2:]); err != nil {
			fail(err)
		}
	case "push":
		fs, cli := newFlagSet("push")
		mustParse(fs, os.Args[2:])
		cfg := mustResolve(fs, cli)
		ac := mustAgent(cfg)
		res, err := agent.Push(ctx, ac, cfg.Allowlist, cfg.ChekaAllow, cfg.ChekaRedact)
		if err != nil {
			fail(err)
		}
		fmt.Printf("blobs:   %d files, %d uploaded, %d committed, %d skipped, %d deleted\n",
			res.BlobFiles, res.BlobsUploaded, res.BlobsCommitted, res.BlobsSkipped, res.BlobsDeleted)
		fmt.Printf("logs:    %d files, %d new lines, %d duplicates\n",
			res.LogFiles, res.LogLinesNew, res.LogLinesDup)
		if res.SecretsFound > 0 {
			fmt.Printf("secrets: %d found", res.SecretsFound)
			if res.SecretsRedacted > 0 {
				fmt.Printf(" (%d redacted)", res.SecretsRedacted)
			}
			fmt.Println()
		}
	case "pull":
		fs, cli := newFlagSet("pull")
		full := fs.Bool("full", false, "bootstrap (rewrite everything)")
		mustParse(fs, os.Args[2:])
		cfg := mustResolve(fs, cli)
		ac := mustAgent(cfg)
		res, err := agent.Pull(ctx, ac, *full)
		if err != nil {
			fail(err)
		}
		fmt.Printf("tar:  %d files seen, %d written, %d removed, %d bytes\n",
			res.TarFiles, res.TarWritten, res.TarRemoved, res.TarBytes)
		fmt.Printf("logs: %d files, %d new lines, cursor=%d\n",
			res.LogFiles, res.LogLinesNew, res.NewCursor)
	case "watch":
		if err := runWatch(ctx, os.Args[2:]); err != nil {
			fail(err)
		}
	case "explore":
		if err := runExplore(ctx, os.Args[2:]); err != nil {
			fail(err)
		}
	case "host-id":
		fs, cli := newFlagSet("host-id")
		mustParse(fs, os.Args[2:])
		cfg := mustResolve(fs, cli)
		id, err := agent.LoadOrCreateHostID(cfg.Dir)
		if err != nil {
			fail(err)
		}
		fmt.Println(id)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func newFlagSet(name string) (*flag.FlagSet, *config.CLI) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return fs, config.RegisterFlags(fs)
}

func mustParse(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
}

func mustResolve(fs *flag.FlagSet, cli *config.CLI) *config.Resolved {
	cfg, err := config.Resolve(fs, cli)
	if err != nil {
		fail(err)
	}
	return cfg
}

func mustAgent(cfg *config.Resolved) *agent.Config {
	ac, err := agent.New(cfg.Dir, cfg.URL, cfg.Token)
	if err != nil {
		fail(err)
	}
	return ac
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
