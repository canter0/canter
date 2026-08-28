package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func archive(t *testing.T, name string, mode int64, content string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRouteSwitchDrainsPreviousTarget(t *testing.T) {
	n := &node{inflight: make(map[string]int)}
	n.target.Store("http://old")
	target, release := n.acquireTarget()
	if target != "http://old" {
		t.Fatalf("target=%q", target)
	}
	n.setTarget("http://new")
	var drained atomic.Bool
	go func() {
		n.waitForDrain(t.Context(), "http://old", time.Second)
		drained.Store(true)
	}()
	time.Sleep(30 * time.Millisecond)
	if drained.Load() {
		t.Fatal("old target drained while a request was still active")
	}
	release()
	deadline := time.Now().Add(time.Second)
	for !drained.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !drained.Load() {
		t.Fatal("old target did not drain")
	}
	newTarget, done := n.acquireTarget()
	done()
	if newTarget != "http://new" {
		t.Fatalf("new target=%q", newTarget)
	}
}

func TestExtractTarGzPreservesExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := extractTarGz(archive(t, "app", 0o750, "binary"), dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	if err := extractTarGz(archive(t, "../escape", 0o600, "bad"), t.TempDir()); err == nil {
		t.Fatal("path traversal archive was accepted")
	}
}
