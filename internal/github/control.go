package github

import (
	"context"
	"fmt"
	"time"

	"gh-mirror/internal/progress"
)

func (c *Client) acquire(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("cancel queued GitHub request: %w", err)
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return 0, fmt.Errorf("cancel queued GitHub request: %w", ctx.Err())
	}
	for {
		c.mu.Lock()
		if c.fatal != nil || ctx.Err() != nil {
			err := c.fatal
			if err == nil {
				err = fmt.Errorf("cancel queued GitHub request: %w", ctx.Err())
			}
			c.mu.Unlock()
			<-c.slots
			return 0, err
		}
		wait := time.Until(c.pausedUntil)
		if wait <= 0 {
			if c.remaining <= 0 {
				err := fmt.Errorf("GitHub request budget exhausted after %d attempts", c.used)
				c.fatal = err
				c.mu.Unlock()
				<-c.slots
				return 0, err
			}
			c.used++
			c.remaining--
			c.active++
			number := c.used
			c.mu.Unlock()
			c.Report(progress.Event{})
			return number, nil
		}
		c.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			<-c.slots
			return 0, fmt.Errorf("cancel GitHub retry wait: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *Client) release() {
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	<-c.slots
	c.Report(progress.Event{})
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatal == nil {
		c.fatal = err
	}
}

func (c *Client) pause(delay time.Duration) {
	c.mu.Lock()
	if until := time.Now().Add(delay); until.After(c.pausedUntil) {
		c.pausedUntil = until
	}
	c.mu.Unlock()
	c.Report(progress.Event{Wait: delay})
}

// Report serializes activity callbacks and attaches shared worker/request state.
func (c *Client) Report(event progress.Event) {
	c.reportMu.Lock()
	defer c.reportMu.Unlock()
	c.mu.Lock()
	event.Requests, event.Limit = c.used, c.used+c.remaining
	event.Resumed = c.resumed
	event.Active, event.Workers = c.active, cap(c.slots)
	event.WorkerKind = progress.GitHubWorkers
	event.WaitUntil = c.pausedUntil
	c.mu.Unlock()
	c.Progress.Send(event)
}

// Resumed reports saved responses reused without an HTTP request.
func (c *Client) Resumed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resumed
}
func (c *Client) load(ctx context.Context, key string) ([]byte, bool, error) {
	if c.Checkpoint == nil {
		return nil, false, nil
	}
	body, found, err := c.Checkpoint.Load(ctx, key)
	if err != nil {
		return nil, false, fmt.Errorf("resume GitHub fetch: %w", err)
	}
	if found {
		c.mu.Lock()
		c.resumed++
		c.mu.Unlock()
		c.Report(progress.Event{})
	}
	return body, found, nil
}
func (c *Client) save(ctx context.Context, key string, body []byte) error {
	if c.Checkpoint != nil {
		if err := c.Checkpoint.Save(ctx, key, body); err != nil {
			return fmt.Errorf("checkpoint GitHub fetch: %w", err)
		}
	}
	return nil
}
