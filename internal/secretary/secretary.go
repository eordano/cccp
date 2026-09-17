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
	Agent             *agent.Config
	Root              string
	Allowlist         []string
	Debounce          time.Duration
	ReconcileInterval time.Duration
	Log               *slog.Logger
	ChekaAllow        int
	ChekaRedact       bool
	PushOnly          bool
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

	allow := opt.Allowlist
	if allow == nil {
		allow = opt.Agent.DefaultAllowlist()
	}
	go runWatcher(ctx, opt.Root, ignorePrefix, allow, dirty, &localDirty, opt.Log)
	go debounce(ctx, dirty, opt.Debounce, func() {
		enqueue(triggers, kindPush)
	})
	if opt.PushOnly {
		opt.Log.Info("push-only: not subscribing to server events, pulls disabled")
	} else {
		go subscribeEvents(ctx, opt.Agent, opt.Log, func() {
			enqueue(triggers, kindPull)
		})
	}

	localDirty.Store(true)
	enqueue(triggers, kindPush)
	runSerializer(ctx, triggers, &localDirty, opt)
	return nil
}

func enqueue(ch chan<- kind, k kind) {
	select {
	case ch <- k:
	default:
	}
}
