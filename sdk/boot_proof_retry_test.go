package sdk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type receiptSequence struct {
	reads    int
	failures int
	proof    BootProof
}

func (s *receiptSequence) GetOptional(_ context.Context, _ string, out any) (bool, error) {
	s.reads++
	if s.reads <= s.failures {
		return false, errors.New("temporary receipt read failure")
	}
	*out.(*BootProof) = s.proof
	return true, nil
}
func TestBootProofRecoversAfterReadFailures(t *testing.T) {
	s := &receiptSequence{failures: 2, proof: BootProof{Status: "booted", ExitCode: 0}}
	if err := waitForProofInterval(context.Background(), s, "receipt", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if s.reads != 3 {
		t.Fatalf("reads=%d", s.reads)
	}
}
func TestBootProofReadRetriesAreBounded(t *testing.T) {
	s := &receiptSequence{failures: 10}
	err := waitForProofInterval(context.Background(), s, "receipt", time.Millisecond)
	if err == nil || s.reads != 3 {
		t.Fatalf("reads=%d error=%v", s.reads, err)
	}
}
func TestBootProofDoesNotRetryExplicitGuestFailure(t *testing.T) {
	s := &receiptSequence{proof: BootProof{Status: "failed", ExitCode: 1}}
	err := waitForProofInterval(context.Background(), s, "receipt", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "bootstrap failed") || s.reads != 1 {
		t.Fatalf("reads=%d error=%v", s.reads, err)
	}
}
func TestBootProofRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &receiptSequence{failures: 10}
	if err := waitForProofInterval(ctx, s, "receipt", time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
