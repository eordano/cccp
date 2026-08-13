package secretary

import (
	"context"
	"time"
)

func debounce(ctx context.Context, in <-chan struct{}, window time.Duration, fire func()) {
	var timerC <-chan time.Time
	var timer *time.Timer
	reset := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(window)
		timerC = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-in:
			reset()
		case <-timerC:
			timerC = nil
			fire()
		}
	}
}
