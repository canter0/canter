package wrencomputer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func snapshotArchiveProcess(t *testing.T, ctx context.Context, workspace, home string, input io.Reader) *exec.Cmd {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	source, err := os.ReadFile("snapshot_stream.py")
	if err != nil {
		t.Fatal(err)
	}
	code := string(source)
	for literal, root := range map[string]string{`WORKSPACE_ROOT = "/workspace"`: workspace, `HOME_ROOT = "/home/wren"`: home} {
		if strings.Count(code, literal) != 1 {
			t.Fatal("snapshot fixture root substitution is ambiguous")
		}
		quoted, _ := json.Marshal(root)
		code = strings.Replace(code, literal, strings.Split(literal, " = ")[0]+" = "+string(quoted), 1)
	}
	command := exec.CommandContext(ctx, python, "-I", "-c", code)
	command.Stdin = input
	return command
}

func snapshotArchiveRoots(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	workspace, home := filepath.Join(root, "workspace"), filepath.Join(root, "home")
	for _, path := range []string{workspace, home} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return workspace, home
}

func snapshotArchiveWrite(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func snapshotArchiveHeader(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func snapshotArchiveExport(t *testing.T, workspace, home string, options SandboxSnapshotOptions) (string, SandboxSnapshotStats) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	framed, err := os.CreateTemp(t.TempDir(), "framed-*")
	if err != nil {
		t.Fatal(err)
	}
	defer framed.Close()
	command := snapshotArchiveProcess(t, ctx, workspace, home, bytes.NewReader(snapshotArchiveHeader(t, map[string]any{"operation": "export", "options": options})))
	var stderr bytes.Buffer
	command.Stdout, command.Stderr = framed, &stderr
	if err = command.Run(); err != nil {
		t.Fatalf("export failed: %v: %s", err, stderr.String())
	}
	if _, err = framed.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	reader, err := newSnapshotFrameReader(framed, options)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	receipt, err := validateSnapshotStream(reader, archive)
	if err != nil {
		t.Fatal(err)
	}
	return archive.Name(), receipt
}

func snapshotArchiveRestore(t *testing.T, workspace, home string, options SandboxSnapshotOptions, expected SandboxSnapshotStats, data io.Reader) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	header := snapshotArchiveHeader(t, map[string]any{"operation": "restore", "options": options, "expected": expected})
	command := snapshotArchiveProcess(t, ctx, workspace, home, io.MultiReader(bytes.NewReader(header), data))
	var output, stderr bytes.Buffer
	command.Stdout, command.Stderr = &output, &stderr
	if err := command.Run(); err != nil {
		return err
	}
	return readSnapshotRestoreReceipt(&output, options, expected)
}

func snapshotArchiveFileHash(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestSandboxSnapshotArchivePreservesLargeProjects(t *testing.T) {
	workspace, home := snapshotArchiveRoots(t)
	snapshotArchiveWrite(t, filepath.Join(home, ".local/bin/tool"), "#!/bin/sh\necho tool\n", 0755)
	snapshotArchiveWrite(t, filepath.Join(home, ".local/lib/cache.bin"), "linked package contents", 0644)
	snapshotArchiveWrite(t, filepath.Join(home, ".local/lib/zero.bin"), strings.Repeat("\x00", 100<<10), 0600)
	snapshotArchiveWrite(t, filepath.Join(workspace, "node_modules/pkg/index.js"), "module.exports = 'kept';", 0644)
	snapshotArchiveWrite(t, filepath.Join(workspace, "venv/pyvenv.cfg"), "home = /usr/bin", 0644)
	snapshotArchiveWrite(t, filepath.Join(workspace, "unicode-é.txt"), "héllo ✓\x00\xff", 0444)
	snapshotArchiveWrite(t, filepath.Join(home, ".ssh/id_test"), "managed source key", 0600)
	snapshotArchiveWrite(t, filepath.Join(workspace, ".canter/request.json"), "managed source state", 0600)
	snapshotArchiveWrite(t, filepath.Join(workspace, ".browser/Cookies"), "source browser session", 0600)
	if err := os.Link(filepath.Join(home, ".local/lib/cache.bin"), filepath.Join(workspace, "node_modules/pkg/cache.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/python3", filepath.Join(workspace, "venv/python")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(workspace, "runtime.pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 123456789)
	tool := filepath.Join(home, ".local/bin/tool")
	if err := os.Chtimes(tool, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	largePath := filepath.Join(workspace, "large.bin")
	large, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	// Incompressible data proves the new stream exceeds the old archive cap,
	// without allocating the file's contents in the test process.
	if _, err = io.CopyN(large, rand.Reader, 34<<20); err != nil {
		t.Fatal(err)
	}
	large.Close()
	options := SandboxSnapshotOptions{MaxUnpackedBytes: 64 << 20, MaxEntries: 100}
	archivePath, receipt := snapshotArchiveExport(t, workspace, home, options)
	if receipt.CompressedBytes <= CheckpointMaxCompressed || receipt.RuntimeSkipped != 1 {
		t.Fatalf("archive cap/runtime omission receipt: %#v", receipt)
	}
	targetWorkspace, targetHome := snapshotArchiveRoots(t)
	snapshotArchiveWrite(t, filepath.Join(targetWorkspace, "stale.txt"), "stale allocation file", 0600)
	snapshotArchiveWrite(t, filepath.Join(targetWorkspace, ".canter/keep"), "destination machine state", 0600)
	snapshotArchiveWrite(t, filepath.Join(targetHome, ".ssh/authorized_keys"), "destination key", 0600)
	archive, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if err = snapshotArchiveRestore(t, targetWorkspace, targetHome, options, receipt, archive); err != nil {
		t.Fatal("restore", err)
	}
	for source, target := range map[string]string{
		largePath: filepath.Join(targetWorkspace, "large.bin"),
		tool:      filepath.Join(targetHome, ".local/bin/tool"),
		filepath.Join(home, ".local/lib/cache.bin"):           filepath.Join(targetHome, ".local/lib/cache.bin"),
		filepath.Join(home, ".local/lib/zero.bin"):            filepath.Join(targetHome, ".local/lib/zero.bin"),
		filepath.Join(workspace, "node_modules/pkg/index.js"): filepath.Join(targetWorkspace, "node_modules/pkg/index.js"),
		filepath.Join(workspace, "venv/pyvenv.cfg"):           filepath.Join(targetWorkspace, "venv/pyvenv.cfg"),
		filepath.Join(workspace, "unicode-é.txt"):             filepath.Join(targetWorkspace, "unicode-é.txt"),
	} {
		if snapshotArchiveFileHash(t, source) != snapshotArchiveFileHash(t, target) {
			t.Fatalf("file bytes changed: %s", target)
		}
		before, _ := os.Stat(source)
		after, err := os.Stat(target)
		if err != nil || before.Mode().Perm() != after.Mode().Perm() || !before.ModTime().Equal(after.ModTime()) {
			t.Fatalf("file metadata changed: %s: %v", target, err)
		}
	}
	first, _ := os.Stat(filepath.Join(targetHome, ".local/lib/cache.bin"))
	second, err := os.Stat(filepath.Join(targetWorkspace, "node_modules/pkg/cache.bin"))
	if err != nil || !os.SameFile(first, second) {
		t.Fatal("hardlink relationship was not preserved", err)
	}
	if value, err := os.Readlink(filepath.Join(targetWorkspace, "venv/python")); err != nil || value != "/usr/bin/python3" {
		t.Fatal("virtualenv symlink was not preserved", value, err)
	}
	for _, absent := range []string{filepath.Join(targetWorkspace, "stale.txt"), filepath.Join(targetWorkspace, "runtime.pipe"), filepath.Join(targetWorkspace, ".canter/request.json"), filepath.Join(targetWorkspace, ".browser"), filepath.Join(targetHome, ".ssh/id_test")} {
		if _, err := os.Lstat(absent); !os.IsNotExist(err) {
			t.Fatalf("unexpected restored entry %s: %v", absent, err)
		}
	}
	for path, expected := range map[string]string{
		filepath.Join(targetWorkspace, ".canter/keep"):    "destination machine state",
		filepath.Join(targetHome, ".ssh/authorized_keys"): "destination key",
	} {
		value, err := os.ReadFile(path)
		if err != nil || string(value) != expected {
			t.Fatal("managed destination state changed", path, err)
		}
	}
}

func snapshotArchiveFixture(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressed)
	for _, header := range append([]tar.Header{{Name: "home/", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/", Typeflag: tar.TypeDir, Mode: 0700}}, headers...) {
		if err := archive.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := io.CopyN(archive, strings.NewReader(strings.Repeat("x", int(header.Size))), header.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestSandboxSnapshotArchiveRejectsUnsafeMembers(t *testing.T) {
	options := SandboxSnapshotOptions{MaxUnpackedBytes: 1 << 20, MaxEntries: 100}
	cases := map[string][]tar.Header{
		"traversal":       {{Name: "workspace/../../outside", Typeflag: tar.TypeReg, Mode: 0600}},
		"absolute":        {{Name: "/absolute", Typeflag: tar.TypeReg, Mode: 0600}},
		"private":         {{Name: "workspace/.canter/", Typeflag: tar.TypeDir, Mode: 0700}},
		"ssh":             {{Name: "home/.ssh/", Typeflag: tar.TypeDir, Mode: 0700}},
		"browser":         {{Name: "workspace/.browser/", Typeflag: tar.TypeDir, Mode: 0700}},
		"device":          {{Name: "workspace/device", Typeflag: tar.TypeChar, Mode: 0600}},
		"fifo":            {{Name: "workspace/pipe", Typeflag: tar.TypeFifo, Mode: 0600}},
		"setuid":          {{Name: "workspace/program", Typeflag: tar.TypeReg, Mode: 04755}},
		"hardlink escape": {{Name: "workspace/link", Typeflag: tar.TypeLink, Linkname: "../outside", Mode: 0600}},
		"hardlink metadata": {
			{Name: "workspace/base", Typeflag: tar.TypeReg, Mode: 0600},
			{Name: "workspace/link", Typeflag: tar.TypeLink, Linkname: "workspace/base", Mode: 0600, ModTime: time.Unix(1, 0)},
		},
		"timestamp overflow": {{Name: "workspace/future", Typeflag: tar.TypeReg, Mode: 0600, ModTime: time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC)}},
		"symlink parent": {
			{Name: "workspace/link", Typeflag: tar.TypeSymlink, Linkname: "../outside", Mode: 0777},
			{Name: "workspace/link/changed", Typeflag: tar.TypeReg, Mode: 0600, Size: 1},
		},
		"duplicate": {
			{Name: "workspace/file", Typeflag: tar.TypeReg, Mode: 0600},
			{Name: "workspace/file", Typeflag: tar.TypeReg, Mode: 0600},
		},
		"missing parent":     {{Name: "workspace/absent/file", Typeflag: tar.TypeReg, Mode: 0600}},
		"extended privilege": {{Name: "workspace/file", Typeflag: tar.TypeReg, Mode: 0600, PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "unsafe"}}},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			data := snapshotArchiveFixture(t, headers...)
			if _, err := ValidateSandboxArchive(bytes.NewReader(data), options); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			workspace, home := snapshotArchiveRoots(t)
			outside := filepath.Join(filepath.Dir(workspace), "outside/changed")
			snapshotArchiveWrite(t, outside, "untouched", 0600)
			sum := sha256.Sum256(data)
			expected := SandboxSnapshotStats{Format: SandboxSnapshotFormat, Entries: 2 + len(headers), CompressedBytes: int64(len(data)), ArchiveSHA256: hex.EncodeToString(sum[:])}
			for _, header := range headers {
				expected.RawBytes += header.Size
			}
			if err := snapshotArchiveRestore(t, workspace, home, options, expected, bytes.NewReader(data)); err == nil {
				t.Fatal("unsafe archive restored")
			}
			value, err := os.ReadFile(outside)
			if err != nil || string(value) != "untouched" {
				t.Fatal("restore escaped its roots", err)
			}
		})
	}
}

func TestSandboxSnapshotArchiveRejectsChangingSource(t *testing.T) {
	workspace, home := snapshotArchiveRoots(t)
	file, err := os.Create(filepath.Join(workspace, "changing.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = io.CopyN(file, rand.Reader, 4<<20); err != nil {
		t.Fatal(err)
	}
	options := SandboxSnapshotOptions{MaxUnpackedBytes: 8 << 20, MaxEntries: 100}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := snapshotArchiveProcess(t, ctx, workspace, home, bytes.NewReader(snapshotArchiveHeader(t, map[string]any{"operation": "export", "options": options})))
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	reader, err := newSnapshotFrameReader(output, options)
	if err != nil {
		t.Fatal(err)
	}
	// Pipe backpressure keeps export inside this file until reading resumes.
	var first [128]byte
	if _, err = io.ReadFull(reader, first[:]); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("changed"), (4<<20)-7); err != nil {
		t.Fatal(err)
	}
	_, streamErr := io.Copy(io.Discard, reader)
	exitErr := command.Wait()
	if streamErr == nil || exitErr == nil || reader.done || !strings.Contains(stderr.String(), "changed") {
		t.Fatalf("changed source produced a usable receipt: stream=%v exit=%v done=%v stderr=%s", streamErr, exitErr, reader.done, stderr.String())
	}
}

func TestSandboxSnapshotArchiveBrowserContinuation(t *testing.T) {
	workspace, home := snapshotArchiveRoots(t)
	snapshotArchiveWrite(t, filepath.Join(workspace, ".browser/Cookies"), "same-sandbox session", 0600)
	snapshotArchiveWrite(t, filepath.Join(home, ".ssh/id_test"), "machine identity", 0600)
	options := SandboxSnapshotOptions{MaxUnpackedBytes: 1 << 20, MaxEntries: 100, IncludeBrowser: true}
	path, receipt := snapshotArchiveExport(t, workspace, home, options)
	archive, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	targetWorkspace, targetHome := snapshotArchiveRoots(t)
	if err = snapshotArchiveRestore(t, targetWorkspace, targetHome, options, receipt, archive); err != nil {
		t.Fatal(err)
	}
	cookie, err := os.ReadFile(filepath.Join(targetWorkspace, ".browser/Cookies"))
	if err != nil || string(cookie) != "same-sandbox session" {
		t.Fatal("browser continuation data changed", err)
	}
	if _, err = os.Lstat(filepath.Join(targetHome, ".ssh/id_test")); !os.IsNotExist(err) {
		t.Fatal("browser continuation copied machine identity", err)
	}
	if _, err = archive.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	options.IncludeBrowser = false
	if _, err = ValidateSandboxArchive(archive, options); err == nil {
		t.Fatal("browser continuation accepted under template exclusions")
	}
}

func TestSandboxSnapshotArchiveRequiresCompleteGzip(t *testing.T) {
	options := SandboxSnapshotOptions{MaxUnpackedBytes: 1024, MaxEntries: 10}
	valid := snapshotArchiveFixture(t)
	if _, err := ValidateSandboxArchive(bytes.NewReader(valid), options); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-5] ^= 1
	for name, data := range map[string][]byte{
		"truncated":   valid[:len(valid)-1],
		"checksum":    corrupt,
		"trailing":    append(bytes.Clone(valid), 1),
		"second gzip": append(bytes.Clone(valid), valid...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateSandboxArchive(bytes.NewReader(data), options); err == nil {
				t.Fatal("incomplete or trailing gzip accepted")
			}
		})
	}
}
