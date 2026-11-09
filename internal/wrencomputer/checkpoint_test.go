package wrencomputer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func checkpointFixtureArchive(t *testing.T, name string, kind byte, size int64) []byte {
	t.Helper()
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	w := tar.NewWriter(z)
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0600, Typeflag: kind, Size: size, Linkname: "../../escape"}); err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		w.Write(bytes.Repeat([]byte{'x'}, int(size)))
	}
	w.Close()
	z.Close()
	return out.Bytes()
}
func TestCheckpointArchiveRejectsEscapesCredentialsAndSpecialFiles(t *testing.T) {
	for _, item := range []struct {
		name string
		kind byte
	}{{"../escape", tar.TypeReg}, {"/etc/passwd", tar.TypeReg}, {".canter/harness/session/launch.json", tar.TypeReg}, {".browser/Default/Cookies", tar.TypeReg}, {"safe-link", tar.TypeSymlink}, {"pipe", tar.TypeFifo}} {
		if err := ValidateCheckpoint(checkpointFixtureArchive(t, item.name, item.kind, 0), false); err == nil {
			t.Errorf("accepted unsafe checkpoint %q", item.name)
		}
	}
	if err := ValidateCheckpoint(checkpointFixtureArchive(t, "project/index.html", tar.TypeReg, 10), false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckpoint(checkpointFixtureArchive(t, ".browser/Default/Cookies", tar.TypeReg, 10), true); err != nil {
		t.Fatal(err)
	}
}
func TestCheckpointRealCompressionAndRestoreInIsolatedFilesystem(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	root := t.TempDir()
	source, err := filepath.Abs("checkpoint.py")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "project/empty"), 0700)
	os.MkdirAll(filepath.Join(root, ".canter/harness/private"), 0700)
	original := bytes.Repeat([]byte("Actual checkpoint round trip.\n"), 1000)
	os.WriteFile(filepath.Join(root, "project/report.txt"), original, 0600)
	noise := make([]byte, 9<<20)
	if _, err = rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "project/noise.bin"), noise, 0600)
	os.WriteFile(filepath.Join(root, ".canter/harness/private/token"), []byte("must not enter archive"), 0600)
	base := filepath.Join(root, ".canter/checkpoints/checkpoint_test_1234")
	os.MkdirAll(base, 0700)
	os.WriteFile(filepath.Join(base, "request.json"), []byte(`{"includeBrowser":false}`), 0600)
	run := func(action string) {
		command := exec.Command(python, source, action, base)
		command.Env = append(os.Environ(), "CANTER_COMPUTER_ROOT="+root, "CANTER_COMPUTER_STATE="+filepath.Join(root, ".canter/state"))
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("checkpoint helper: %v %s", e, out)
		}
	}
	run("create")
	data, err := os.ReadFile(filepath.Join(base, "workspace.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateCheckpoint(data, false); err != nil {
		t.Fatal(err)
	}
	if len(data) >= len(original)+len(noise) {
		t.Fatal("fixture did not compress")
	}
	var manifest CheckpointManifest
	raw, _ := os.ReadFile(filepath.Join(base, "manifest.json"))
	if json.Unmarshal(raw, &manifest) != nil {
		t.Fatal("invalid manifest")
	}
	for _, e := range manifest.Entries {
		if e.Path == ".canter/harness/private/token" {
			t.Fatal("runtime token archived")
		}
	}
	os.MkdirAll(filepath.Join(base, "parts"), 0700)
	parts := (len(data) + (8 << 20) - 1) / (8 << 20)
	if parts < 2 {
		t.Fatal("fixture did not exercise multipart restore")
	}
	for i := 0; i < parts; i++ {
		os.WriteFile(filepath.Join(base, "parts", fmt.Sprint(i)), data[i*(8<<20):min(len(data), (i+1)*(8<<20))], 0600)
	}
	req, _ := json.Marshal(map[string]any{"includeBrowser": false, "parts": parts, "archiveSha256": manifest.ArchiveSHA256})
	os.WriteFile(filepath.Join(base, "request.json"), req, 0600)
	os.RemoveAll(filepath.Join(root, "project"))
	run("restore")
	restored, _ := os.ReadFile(filepath.Join(root, "project/report.txt"))
	actualNoise, _ := os.ReadFile(filepath.Join(root, "project/noise.bin"))
	if !bytes.Equal(actualNoise, noise) {
		t.Fatal("multipart restore bytes differ")
	}
	if !bytes.Equal(restored, original) {
		t.Fatal("restored bytes differ")
	}
	if info, e := os.Stat(filepath.Join(root, "project/empty")); e != nil || !info.IsDir() {
		t.Fatal("empty directory lost")
	}
}

func TestCheckpointBrowserRestoreDoesNotMergeFreshSessionFiles(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	root := t.TempDir()
	source, _ := filepath.Abs("checkpoint.py")
	sessions := filepath.Join(root, ".browser/Default/Sessions")
	os.MkdirAll(sessions, 0700)
	os.WriteFile(filepath.Join(sessions, "saved-session"), []byte("saved public tab"), 0600)
	model := filepath.Join(root, ".browser/optimization_guide_model_store")
	os.MkdirAll(model, 0700)
	os.WriteFile(filepath.Join(model, "downloaded-model.bin"), []byte("rebuildable downloaded model"), 0600)
	base := filepath.Join(root, ".canter/checkpoints/browser_test_1234")
	os.MkdirAll(base, 0700)
	os.WriteFile(filepath.Join(base, "request.json"), []byte(`{"includeBrowser":true}`), 0600)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "google-chrome"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	run := func(action string) {
		command := exec.Command(python, source, action, base)
		command.Env = append(os.Environ(), "CANTER_COMPUTER_ROOT="+root, "CANTER_COMPUTER_PROC="+filepath.Join(root, "fake-proc"), "CANTER_COMPUTER_STATE="+filepath.Join(root, "state"), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("profile checkpoint: %v %s", e, out)
		}
	}
	run("create")
	data, _ := os.ReadFile(filepath.Join(base, "workspace.tar.gz"))
	if err = ValidateCheckpoint(data, true); err != nil {
		t.Fatal(err)
	}
	var manifest CheckpointManifest
	raw, _ := os.ReadFile(filepath.Join(base, "manifest.json"))
	json.Unmarshal(raw, &manifest)
	for _, entry := range manifest.Entries {
		if strings.Contains(entry.Path, "optimization_guide_model_store") {
			t.Fatal("downloaded browser model entered profile checkpoint")
		}
	}
	os.RemoveAll(filepath.Join(root, ".browser"))
	os.MkdirAll(sessions, 0700)
	os.WriteFile(filepath.Join(sessions, "newer-fresh-session"), []byte("must not win over restored tabs"), 0600)
	os.MkdirAll(filepath.Join(base, "parts"), 0700)
	os.WriteFile(filepath.Join(base, "parts/0"), data, 0600)
	request, _ := json.Marshal(map[string]any{"includeBrowser": true, "parts": 1, "archiveSha256": manifest.ArchiveSHA256})
	os.WriteFile(filepath.Join(base, "request.json"), request, 0600)
	run("restore")
	saved, _ := os.ReadFile(filepath.Join(sessions, "saved-session"))
	if string(saved) != "saved public tab" {
		t.Fatal("saved session was not restored")
	}
	if _, err = os.Stat(filepath.Join(sessions, "newer-fresh-session")); !os.IsNotExist(err) {
		t.Fatal("fresh VM session leaked into restored browser profile")
	}
}

func TestCheckpointBrowserQuiesceUsesNormalWindowManagerClose(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	source, _ := filepath.Abs("checkpoint.py")
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	pid := filepath.Join(proc, "42424242")
	os.MkdirAll(pid, 0700)
	os.WriteFile(filepath.Join(pid, "cmdline"), []byte("/opt/google/chrome/chrome\x00--user-data-dir=/workspace/.browser\x00"), 0600)
	bin := filepath.Join(root, "bin")
	os.Mkdir(bin, 0700)
	record := filepath.Join(root, "actions")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CANTER_TEST_WM_ACTIONS"
case "$1" in
search) printf '1001\n' ;;
getwindowpid) printf '42424242\n' ;;
key) rm "$CANTER_TEST_BROWSER_PID/cmdline"; rmdir "$CANTER_TEST_BROWSER_PID" ;;
esac
`
	os.WriteFile(filepath.Join(bin, "xdotool"), []byte(script), 0700)
	command := exec.Command(python, "-c", `import importlib.util,sys;spec=importlib.util.spec_from_file_location("checkpoint",sys.argv[1]);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);assert module.quiesce_browser()`, source)
	command.Env = append(os.Environ(), "CANTER_COMPUTER_PROC="+proc, "CANTER_TEST_WM_ACTIONS="+record, "CANTER_TEST_BROWSER_PID="+pid, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, e := command.CombinedOutput(); e != nil {
		t.Fatalf("graceful browser close: %v %s", e, out)
	}
	actions, _ := os.ReadFile(record)
	if !strings.Contains(string(actions), "windowactivate --sync 1001") || !strings.Contains(string(actions), "key --clearmodifiers alt+F4") {
		t.Fatalf("did not use normal WM close: %s", actions)
	}
}
