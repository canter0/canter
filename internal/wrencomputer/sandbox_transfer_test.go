package wrencomputer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestGuestSandboxTransferRoundTripAndVersionFence(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	probe, err := client.Probe(ctx)
	if err != nil || probe.TransferProtocol != 1 {
		t.Fatalf("probe: %+v %v", probe, err)
	}
	const file = "/workspace/project/é file.sh"
	const stage = "upl_transfer_test_001"
	data := []byte("#!/bin/sh\nprintf hello\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if _, err := client.BeginUpload(ctx, stage, file, int64(len(data)), digest); err != nil {
		t.Fatal(err)
	}
	for offset, part := range [][]byte{data[:5], data[5:]} {
		start := int64(offset * 5)
		if _, err := client.UploadChunk(ctx, stage, start, part); err != nil {
			t.Fatal(err)
		}
	}
	in := map[string]any{"id": stage, "path": file, "version": "missing", "size": len(data), "sha256": digest, "executable": true}
	var saved FileUpload
	for range 2 {
		if err := client.SandboxTransfer(ctx, "commit", in, &saved); err != nil || !saved.Complete {
			t.Fatalf("commit receipt: %+v %v", saved, err)
		}
	}
	project := filepath.Join(workspace, "project")
	if err := os.Mkdir(filepath.Join(project, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte("never by default"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("é file.sh", filepath.Join(project, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(project, ".env"), filepath.Join(project, "hardlink")); err != nil {
		t.Fatal(err)
	}
	var manifest sdk.SandboxTransferManifest
	if err := client.SandboxTransfer(ctx, "manifest", map[string]any{"path": "/workspace/project"}, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Exclusions) != 3 || len(manifest.Entries) != 2 || !manifest.Entries[0].Directory || !manifest.Entries[1].Executable {
		t.Fatalf("manifest: %+v", manifest)
	}
	entry := manifest.Entries[1]
	reading := map[string]any{"path": file, "version": entry.Version, "offset": 0, "limit": 1024}
	var chunk FileChunk
	if err := client.SandboxTransfer(ctx, "read", reading, &chunk); err != nil || string(chunk.Data) != string(data) || !chunk.EOF {
		t.Fatalf("read: %+v %v", chunk, err)
	}
	if err := os.WriteFile(filepath.Join(project, entry.Path), []byte("changed concurrently"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := client.SandboxTransfer(ctx, "read", reading, &chunk); err == nil {
		t.Fatal("guest accepted a stale source version")
	}
	// A writer created or changed the destination after planning. Both a
	// stale missing plan and a requested replacement must preserve its bytes.
	in["id"] = "upl_transfer_test_002"
	if _, err := client.BeginUpload(ctx, "upl_transfer_test_002", file, int64(len(data)), digest); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadChunk(ctx, "upl_transfer_test_002", 0, data); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"missing", entry.Version} {
		in["version"] = version
		if err := client.SandboxTransfer(ctx, "commit", in, &saved); err == nil {
			t.Fatalf("guest replaced a competing destination at version %q", version)
		}
	}
	if actual, err := os.ReadFile(filepath.Join(project, entry.Path)); err != nil || string(actual) != "changed concurrently" {
		t.Fatalf("competing file changed: %q %v", actual, err)
	}
	for _, target := range []string{"/workspace/project/../.canter", "/workspace/project/symlink/new", "/workspace/.git/new"} {
		if err := client.SandboxTransfer(ctx, "mkdir", map[string]string{"path": target}, &struct{}{}); err == nil {
			t.Fatalf("guest accepted %q", target)
		}
	}
}
