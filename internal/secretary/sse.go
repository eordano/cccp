package secretary

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/agent"
)

func subscribeEvents(ctx context.Context, c *agent.Config, log *slog.Logger, onTick func()) {
	backoff := 1 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := streamOnce(ctx, c, onTick)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Warn("sse", "err", err, "retry_in", backoff)
		} else {
			log.Info("sse disconnected", "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func streamOnce(ctx context.Context, c *agent.Config, onTick func()) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.ServerURL+"/events", nil)
	if err != nil {
		return err
	}

	c.AuthReq(req)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/events %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		onTick()
	}
	return scanner.Err()
}
