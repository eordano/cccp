package secretary

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/eordano/cccp/internal/agent"
)

type Options struct {
	Agent       *agent.Config
	Root        string
	Allowlist   []string
	Debounce    time.Duration
	Log         *slog.Logger
	ChekaAllow  int
	ChekaRedact bool
}

func Run(ctx context.Context, opt Options) error {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Debounce <= 0 {
		opt.Debounce = 500 * time.Millisecond
	}
	if opt.Agent == nil {
		return errors.New("secretary: Agent is required")
	}
	if opt.Root == "" {
		return errors.New("secretary: Root is required")
	}

	ignorePrefix := filepath.Join(opt.Root, "cccp")

	dirty := make(chan struct{}, 64)
	triggers := make(chan kind, 64)
	var localDirty atomic.Bool

	opt.Log.Info("secretary starting", "root", opt.Root, "debounce", opt.Debounce)

	go runWatcher(ctx, opt.Root, ignorePrefix, dirty, &localDirty, opt.Log)
	go debounce(ctx, dirty, opt.Debounce, func() {
		enqueue(triggers, kindPush)
	})
	go subscribeEvents(ctx, opt.Agent, opt.Log, func() {
		enqueue(triggers, kindPull)
	})

	runSerializer(ctx, triggers, &localDirty, opt)
	return nil
}

func enqueue(ch chan<- kind, k kind) {
	select {
	case ch <- k:
	default:
	}
}
