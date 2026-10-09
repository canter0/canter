package sshsite

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestInputBudgetReaderBurstAndRefill(t *testing.T) {
	now := time.Unix(100, 0)
	violations := 0
	r := newInputBudgetReader(bytes.NewReader(bytes.Repeat([]byte{'x'}, maxSessionInputBurst+maxSessionInputBytesPerSecond+1)), func() time.Time { return now }, func() { violations++ })
	buf := make([]byte, maxSessionInputBurst+1)
	n, err := r.Read(buf)
	if err != nil || n != maxSessionInputBurst {
		t.Fatalf("initial read = %d, %v; want burst %d", n, err, maxSessionInputBurst)
	}
	if n, err = r.Read(buf[:1]); n != 0 || !errors.Is(err, errSessionInputBudget) {
		t.Fatalf("over-budget read = %d, %v; want immediate budget error", n, err)
	}
	now = now.Add(time.Second)
	buf = make([]byte, maxSessionInputBytesPerSecond+1)
	if n, err = r.Read(buf); err != nil || n != maxSessionInputBytesPerSecond {
		t.Fatalf("refilled read = %d, %v; want %d bytes", n, err, maxSessionInputBytesPerSecond)
	}
	if n, err = r.Read(buf[:1]); n != 0 || !errors.Is(err, errSessionInputBudget) {
		t.Fatalf("post-refill read = %d, %v; want immediate budget error", n, err)
	}
	if violations != 1 {
		t.Fatalf("limit callback ran %d times, want exactly once", violations)
	}
}
