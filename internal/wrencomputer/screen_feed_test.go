package wrencomputer

import (
	"context"
	"errors"
	"testing"
)

func TestScreenFeedDropsStaleFramesWithoutBlockingOtherViewers(t *testing.T) {
	slow, fast := make(chan []byte, 1), make(chan []byte, 1)
	feed := &screenFeed{viewers: map[chan []byte]struct{}{slow: {}, fast: {}}, done: make(chan struct{})}
	for i := byte(1); i <= 30; i++ {
		feed.publish([]byte{i})
		if got := <-fast; got[0] != i {
			t.Fatalf("fast viewer lagged at %d", i)
		}
	}
	if got := <-slow; got[0] != 30 {
		t.Fatalf("slow viewer retained old frame: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := feed.watch(ctx, slow, func([]byte) error { t.Fatal("cancelled viewer consumed a frame"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Removing one viewer must leave the other feed intact.
	feed.mu.Lock()
	delete(feed.viewers, slow)
	feed.mu.Unlock()
	feed.publish([]byte{31})
	if got := <-fast; got[0] != 31 {
		t.Fatal("sibling viewer interrupted")
	}
}
