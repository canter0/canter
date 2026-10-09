package sshsite

import (
	"errors"
	"io"
	"sync"
	"time"
)

// A visitor only needs a handful of terminal key sequences to navigate. This
// allows brief paste bursts while bounding parser and redraw work over time.
const (
	maxSessionInputBurst          = 512
	maxSessionInputBytesPerSecond = 256
)

var errSessionInputBudget = errors.New("SSH terminal input budget exceeded")

type inputBudgetReader struct {
	r       io.Reader
	now     func() time.Time
	last    time.Time
	tokens  float64
	onLimit func()
	once    sync.Once
}

func newInputBudgetReader(r io.Reader, now func() time.Time, onLimit func()) *inputBudgetReader {
	if now == nil {
		now = time.Now
	}
	n := now()
	return &inputBudgetReader{r: r, now: now, last: n, tokens: maxSessionInputBurst, onLimit: onLimit}
}

func (r *inputBudgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	now := r.now()
	elapsed := now.Sub(r.last).Seconds()
	if elapsed > 0 {
		r.tokens = min(float64(maxSessionInputBurst), r.tokens+elapsed*maxSessionInputBytesPerSecond)
	}
	r.last = now
	available := int(r.tokens)
	if available < 1 {
		r.once.Do(func() {
			if r.onLimit != nil {
				r.onLimit()
			}
		})
		return 0, errSessionInputBudget
	}
	if len(p) > available {
		p = p[:available]
	}
	n, err := r.r.Read(p)
	r.tokens -= float64(n)
	return n, err
}
