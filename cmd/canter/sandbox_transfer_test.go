package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestSandboxTransferManifestGitAndSensitivePaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git unavailable")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		result, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, result)
		}
	}
	git("init", "-q")
	write(".gitignore", "*.ignored\n", 0600)
	write("tracked.ignored", "tracked despite ignore rule", 0600)
	write("drop.ignored", "leave behind", 0600)
	write(".env", "test marker", 0600)
	write("é file.sh", "echo test", 0755)
	git("add", "-f", "--", "tracked.ignored", ".env")
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("é file.sh", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	root, err := openTransferRoot(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	limits := sdk.SandboxTransferLimits{Files: 100, Bytes: 4096, MetadataBytes: 65536}
	manifest, err := localTransferManifest(context.Background(), root, limits, false)
	if err != nil {
		t.Fatal(err)
	}
	selected := map[string]sdk.SandboxTransferEntry{}
	for _, entry := range manifest.Entries {
		selected[entry.Path] = entry
	}
	if len(selected) != 4 || !selected["empty"].Directory || !selected["é file.sh"].Executable || selected["tracked.ignored"].SHA256 == "" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	explicit, err := localTransferManifest(context.Background(), root, limits, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit.Entries) != 5 || explicit.Entries[0].Path != ".env" {
		t.Fatalf("sensitive override did not include tracked .env: %+v", explicit)
	}
}
