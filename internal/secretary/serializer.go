package secretary

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/eordano/cccp/internal/agent"
)

type kind int

const (
	kindPush kind = iota
	kindPull
)

const finalPushGrace = 250 * time.Millisecond

func finalPush(localDirty *atomic.Bool, pushedOnce bool, opt Options) {
	time.Sleep(finalPushGrace)
	if pushedOnce && !localDirty.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	localDirty.Store(false)
	res, err := agent.Push(ctx, opt.Agent, opt.Allowlist, opt.ChekaAllow, opt.ChekaRedact)
	if err != nil {
		opt.Log.Warn("final push", "err", err)
		return
	}
	opt.Log.Info("final push", "log_new", res.LogLinesNew, "committed", res.BlobsCommitted)
}

func runSerializer(ctx context.Context, triggers <-chan kind, localDirty *atomic.Bool, opt Options) {
	interval := opt.ReconcileInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	pushFailing := false
	pushedOnce := false
	for {
		var pending [2]bool
		select {
		case <-ctx.Done():
			finalPush(localDirty, pushedOnce, opt)
			return
		case <-ticker.C:
			pending[kindPush] = true
			pending[kindPull] = !opt.PushOnly
		case k := <-triggers:
			pending[k] = true
		}

	drain:
		for {
			select {
			case k := <-triggers:
				pending[k] = true
			default:
				break drain
			}
		}

		if pending[kindPull] && (pushFailing || localDirty.Load()) {
			pending[kindPush] = true
		}

		if pending[kindPush] {
			localDirty.Store(false)
			res, err := agent.Push(ctx, opt.Agent, opt.Allowlist, opt.ChekaAllow, opt.ChekaRedact)
			pushFailing = err != nil
			pushedOnce = pushedOnce || err == nil
			if err != nil {
				localDirty.Store(true)
				if ctx.Err() == nil {
					opt.Log.Warn("push", "err", err)
				}
			} else {
				opt.Log.Info("push",
					"committed", res.BlobsCommitted, "skipped", res.BlobsSkipped,
					"uploaded", res.BlobsUploaded, "deleted", res.BlobsDeleted,
					"log_new", res.LogLinesNew, "log_dup", res.LogLinesDup)
			}
		}
		if pending[kindPull] && !opt.PushOnly {
			if pushFailing || localDirty.Load() {
				if ctx.Err() == nil {
					opt.Log.Warn("pull skipped: push is failing, refusing to overwrite unpushed local changes")
				}
				continue
			}
			if res, err := agent.Pull(ctx, opt.Agent, false); err != nil {
				if ctx.Err() == nil {
					opt.Log.Warn("pull", "err", err)
				}
			} else {
				opt.Log.Info("pull",
					"written", res.TarWritten, "removed", res.TarRemoved,
					"log_new", res.LogLinesNew, "cursor", res.NewCursor)
			}
		}
	}
}
