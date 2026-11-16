package wrencomputer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPinnedTransportReusedAcrossOperations(t *testing.T) {
	cfg, _, _ := helperSSH(t)
	var pool ClientPool
	c, err := pool.Get(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := c.connection
	same, err := pool.Get(cfg)
	if err != nil || same != c {
		t.Fatal("client was not reused")
	}
	if _, err = c.ListFiles(context.Background(), "/workspace"); err != nil {
		t.Fatal(err)
	}
	if c.connection != first || first == nil {
		t.Fatal("SSH transport was not reused")
	}
	cfg.HostKeySHA256 = "SHA256:different"
	different, err := pool.Get(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer different.Close()
	if different == c {
		t.Fatal("host identity crossed the transport cache")
	}
	if _, err = different.Probe(context.Background()); err == nil {
		t.Fatal("untrusted host accepted")
	}
}
func TestCancelledInputDoesNotDisconnectOtherOperations(t *testing.T) {
	cfg, _, _ := helperSSH(t)
	c, err := NewSSH(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bin := t.TempDir()
	if err = os.WriteFile(filepath.Join(bin, "xdotool"), []byte("#!/bin/sh\nsleep 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, err = c.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := c.connection
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err = c.Input(ctx, DesktopInput{Kind: "key", Key: "Return"}); err == nil {
		t.Fatal("cancelled input succeeded")
	}
	if _, err = c.ListFiles(context.Background(), "/workspace"); err != nil {
		t.Fatal(err)
	}
	if c.connection != first {
		t.Fatal("cancelling one command disconnected the shared transport")
	}
}
