package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/eordano/cccp/internal/agent"
	"github.com/eordano/cccp/internal/config"
	"github.com/eordano/cccp/internal/secretary"
)

func runWatch(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "hook-start":
			return watchHookStart(args[1:])
		case "hook-end":
			return watchHookEnd(args[1:])
		case "status":
			return watchStatus(args[1:])
		case "stop":
			return watchStop(args[1:])
		}
	}
	return watchDaemon(args)
}

type hookPayload struct {
	SessionID string `json:"session_id"`
}

func readHookPayload() hookPayload {
	var p hookPayload
	b, _ := io.ReadAll(os.Stdin)
	if len(b) > 0 {
		_ = json.Unmarshal(b, &p)
	}
	if p.SessionID == "" {
		p.SessionID = os.Getenv("CCCP_SESSION_ID")
	}
	if p.SessionID == "" {
		p.SessionID = os.Getenv("CLAUDE_SESSION_ID")
	}
	if p.SessionID == "" {
		p.SessionID = fmt.Sprintf("unknown-%d", os.Getpid())
	}
	return p
}

func watchResolveCfg(args []string) (*config.Resolved, error) {
	fs, cli := newFlagSet("watch")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.Resolve(fs, cli)
}

func watchHookStart(args []string) error {
	cfg, err := watchResolveCfg(args)
	if err != nil {
		return err
	}
	paths := secretary.NewPaths(cfg.Dir)
	p := readHookPayload()
	if _, err := paths.AddSession(p.SessionID); err != nil {
		return fmt.Errorf("add session: %w", err)
	}
	if paths.DaemonRunning() {
		return nil
	}
	return spawnWatchDaemon(cfg, paths)
}

func watchHookEnd(args []string) error {
	cfg, err := watchResolveCfg(args)
	if err != nil {
		return err
	}
	paths := secretary.NewPaths(cfg.Dir)
	p := readHookPayload()
	n, err := paths.RemoveSession(p.SessionID)
	if err != nil {
		return err
	}
	if n == 0 && paths.DaemonRunning() {
		paths.SignalDaemon(syscall.SIGTERM)
		deadline := time.Now().Add(5 * time.Second)
		for paths.DaemonRunning() && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

func watchStatus(args []string) error {
	cfg, err := watchResolveCfg(args)
	if err != nil {
		return err
	}
	paths := secretary.NewPaths(cfg.Dir)
	n, err := paths.SessionCount()
	if err != nil {
		return err
	}
	state, pid := paths.DaemonLiveness()
	var label string
	switch state {
	case secretary.DaemonRunning:
		label = "running"
	case secretary.DaemonUnknown:
		label = "unknown (pid present but cannot probe)"
	default:
		label = "not running"
	}
	if pid == 0 {
		fmt.Printf("status: %s  sessions: %d\n", label, n)
	} else {
		fmt.Printf("status: %s  pid: %d  sessions: %d\n", label, pid, n)
	}
	return nil
}

func watchStop(args []string) error {
	cfg, err := watchResolveCfg(args)
	if err != nil {
		return err
	}
	paths := secretary.NewPaths(cfg.Dir)
	if !paths.DaemonRunning() {
		return errors.New("daemon not running")
	}
	if !paths.SignalDaemon(syscall.SIGTERM) {
		return errors.New("signal failed")
	}
	return nil
}

func watchDaemon(args []string) error {
	fs, cli := newFlagSet("watch")
	mustParse(fs, args)
	cfg := mustResolve(fs, cli)

	paths := secretary.NewPaths(cfg.Dir)

	lock, err := paths.TryAcquireDaemon()
	if err != nil {
		return err
	}
	if lock == nil {
		return nil
	}
	defer lock.Close()

	if err := paths.WritePID(); err != nil {
		return err
	}
	defer paths.RemovePID()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ac, err := agent.New(cfg.Dir, cfg.URL, cfg.Token, cfg.Agent)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	allow := cfg.Allowlist
	if allow == nil {
		allow = ac.DefaultAllowlist()
	}

	redact := cfg.ChekaRedact
	if !cfg.ChekaRedactSet {
		redact = true
	}

	return secretary.Run(ctx, secretary.Options{
		Agent:       ac,
		Root:        cfg.Dir,
		Allowlist:   allow,
		Debounce:    time.Duration(cfg.DebounceMS) * time.Millisecond,
		Log:         logger,
		ChekaAllow:  cfg.ChekaAllow,
		ChekaRedact: redact,
		PushOnly:    cfg.PushOnly,
	})
}

func spawnWatchDaemon(cfg *config.Resolved, paths secretary.Paths) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := paths.Dir + "/secretary.log"
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return err
	}
	logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logF.Close()

	runArgs := []string{"watch", "--agent", cfg.Agent, "--dir", cfg.Dir}
	if cfg.URL != "" {
		runArgs = append(runArgs, "--url", cfg.URL)
	}
	if cfg.TokenFile != "" && cfg.TokenFromFile {
		runArgs = append(runArgs, "--token-file", cfg.TokenFile)
	} else if cfg.Token != "" {
		runArgs = append(runArgs, "--token", cfg.Token)
	}
	if cfg.PushOnly {
		runArgs = append(runArgs, "--push-only")
	}

	c := exec.Command(self, runArgs...)
	c.Stdout = logF
	c.Stderr = logF
	c.Stdin = nil
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	_ = c.Process.Release()
	return nil
}
