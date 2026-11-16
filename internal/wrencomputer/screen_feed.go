package wrencomputer

import (
	"context"
	"fmt"
	"sync"
)

type screenFeed struct {
	mu      sync.Mutex
	viewers map[chan []byte]struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
}

// ScreenStream fans out one capture per computer transport. Each viewer gets
// the latest frame, not an accumulating backlog. Slow viewers cannot delay
// other people or the computer's input channel.
func (c *Client) ScreenStream(ctx context.Context, receive func([]byte) error) error {
	c.screenMu.Lock()
	feed := c.screen
	if feed != nil {
		select {
		case <-feed.done:
			feed = nil
		default:
		}
	}
	if feed == nil {
		captureCtx, cancel := context.WithCancel(context.Background())
		feed = &screenFeed{viewers: make(map[chan []byte]struct{}), cancel: cancel, done: make(chan struct{})}
		c.screen = feed
		go func() {
			err := c.captureScreenStream(captureCtx, feed.publish)
			feed.mu.Lock()
			feed.err = err
			close(feed.done)
			feed.mu.Unlock()
		}()
	}
	viewer := make(chan []byte, 1)
	feed.mu.Lock()
	if len(feed.viewers) >= 32 {
		feed.mu.Unlock()
		c.screenMu.Unlock()
		return fmt.Errorf("computer has too many screen viewers")
	}
	feed.viewers[viewer] = struct{}{}
	feed.mu.Unlock()
	c.screenMu.Unlock()
	defer func() {
		c.screenMu.Lock()
		defer c.screenMu.Unlock()
		feed.mu.Lock()
		defer feed.mu.Unlock()
		delete(feed.viewers, viewer)
		if len(feed.viewers) == 0 {
			feed.cancel()
			if c.screen == feed {
				c.screen = nil
			}
		}
	}()
	return feed.watch(ctx, viewer, receive)
}
func (f *screenFeed) publish(frame []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for viewer := range f.viewers {
		select {
		case <-viewer:
		default:
		}
		viewer <- frame
	}
	return nil
}
func (f *screenFeed) watch(ctx context.Context, viewer <-chan []byte, receive func([]byte) error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.done:
			f.mu.Lock()
			err := f.err
			f.mu.Unlock()
			return err
		case frame := <-viewer:
			if err := receive(frame); err != nil {
				return err
			}
		}
	}
}
