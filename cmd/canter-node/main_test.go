package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
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
