package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeSecret(t *testing.T, dir, name string, mode os.FileMode, body string, links int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < links; i++ {
		if err := os.Link(p, filepath.Join(dir, name+string(rune('a'+i-1))+".link")); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestReadPrivateFileAcceptsPrivateFile(t *testing.T) {
	dir := t.TempDir()
	p := writeSecret(t, dir, "token", 0o600, "secret-value", 1)
	raw, err := readPrivateFile(p)
	if err != nil {
		t.Fatalf("readPrivateFile: %v", err)
	}
	if string(raw) != "secret-value" {
		t.Fatalf("unexpected content %q", raw)
	}
}

func TestReadPrivateFileRejects(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		path func() string
	}{
		{"missing", func() string { return filepath.Join(dir, "absent") }},
		{"wrong-mode", func() string { return writeSecret(t, dir, "m0644", 0o644, "x", 1) }},
		{"group-readable", func() string { return writeSecret(t, dir, "m0640", 0o640, "x", 1) }},
		{"hardlink", func() string { return writeSecret(t, dir, "hl", 0o600, "x", 2) }},
		{"directory", func() string { return dir }},
		{"fifo", func() string {
			path := filepath.Join(dir, "fifo")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readPrivateFile(tc.path()); err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
		})
	}
	// Symlink must be rejected even when it points at a valid file.
	target := writeSecret(t, dir, "real", 0o600, "x", 1)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(link); err == nil {
		t.Fatal("symlink must be rejected")
	}
	// Oversize file rejected.
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", (64<<10)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(big); err == nil {
		t.Fatal("oversize file must be rejected")
	}
}
