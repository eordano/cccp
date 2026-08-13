package secretary

import (
	"context"
	"sync/atomic"

	"github.com/eordano/cccp/internal/agent"
)

type kind int

const (
	kindPush kind = iota
	kindPull
)

func runSerializer(ctx context.Context, triggers <-chan kind, localDirty *atomic.Bool, opt Options) {
	pushFailing := false
	for {
		var pending [2]bool
		select {
		case <-ctx.Done():
			return
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
			if err != nil {
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
		if pending[kindPull] {
			if pushFailing {
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
