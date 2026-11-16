package wrencomputer

// Real guest-protocol tests: these run the actual guest.py over helperSSH with
// a sandbox marker, so Sandbox:true jobs and the upload/metrics/log surfaces
// are exercised end to end — not a fake client.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func sandboxHelper(t *testing.T) (*Client, string) {
	return sandboxHelperUser(t, true)
}

// sandboxHelperUser configures the client as the public user file surface
// (UserFiles) when requested — matching what the control plane uses.
func sandboxHelperUser(t *testing.T, userFiles bool) (*Client, string) {
	t.Helper()
	config, workspace, _ := helperSSH(t, true)
	config.UserFiles = userFiles
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, workspace
}

func TestGuestSandboxJobEnvCwd(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	subdir := filepath.Join(workspace, "jobdir")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	job, err := client.StartJob(ctx, JobRequest{ID: "job_envtest_0001", Command: "printf '%s' \"$WREN_JOB_VAR\" > out.txt && pwd", Directory: "/workspace/jobdir", Env: map[string]string{"WREN_JOB_VAR": "env-value-123"}, Sandbox: true, TimeoutSeconds: 60})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for i := 0; i < 50; i++ {
		job, _ = client.Job(ctx, job.ID)
		if job.Status == "completed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if job.Status != "completed" {
		t.Fatalf("job did not complete: %s", job.Status)
	}
	data, err := os.ReadFile(filepath.Join(subdir, "out.txt"))
	if err != nil {
		t.Fatalf("env var file missing: %v", err)
	}
	if string(data) != "env-value-123" {
		t.Fatalf("env not delivered to child: %q", data)
	}
	// cwd was honored: the job ran inside /workspace/jobdir
	logs, err := client.JobLogs(ctx, job.ID, "stdout", 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs.Data), "jobdir") {
		t.Fatalf("job did not run in /workspace/jobdir: %q", logs.Data)
	}
}

func TestGuestSandboxJobStopCancel(t *testing.T) {
	client, _ := sandboxHelper(t)
	ctx := context.Background()
	job, err := client.StartJob(ctx, JobRequest{ID: "job_cancelt_0001", Command: "sleep 600", Directory: "/workspace", Sandbox: true, TimeoutSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := client.StopJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status == "running" || stopped.Status == "starting" {
		for i := 0; i < 30 && (stopped.Status == "running" || stopped.Status == "starting"); i++ {
			time.Sleep(100 * time.Millisecond)
			stopped, _ = client.Job(ctx, job.ID)
		}
	}
	if stopped.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %s", stopped.Status)
	}
}

func TestGuestUploadRoundtrip(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	// >8MiB file exercises chunked upload; verify digest + final bytes.
	content := bytes.Repeat([]byte("chunked-upload-pattern-"), 400000) // ~9.2MB
	sum := sha256.Sum256(content)
	begin, err := client.BeginUpload(ctx, "upl_test0001", "/workspace/big.bin", int64(len(content)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if begin.ID != "upl_test0001" || begin.Size != int64(len(content)) {
		t.Fatalf("begin receipt wrong: %+v", begin)
	}
	chunk := 4 << 20
	for offset := int64(0); offset < int64(len(content)); offset += int64(chunk) {
		end := offset + int64(chunk)
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		// Idempotent retry of the same chunk must succeed.
		if offset == 0 {
			if _, err := client.UploadChunk(ctx, begin.ID, offset, content[offset:end]); err != nil {
				t.Fatalf("first chunk: %v", err)
			}
		}
		if _, err := client.UploadChunk(ctx, begin.ID, offset, content[offset:end]); err != nil {
			t.Fatalf("chunk at %d: %v", offset, err)
		}
	}
	done, err := client.CompleteUpload(ctx, begin.ID)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !done.Complete || done.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("complete wrong: %+v", done)
	}
	stored, err := os.ReadFile(filepath.Join(workspace, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("uploaded file mismatch: %d vs %d bytes", len(stored), len(content))
	}
}

func TestGuestUploadGapOverrunDigest(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	// Gap: second chunk must be sequential.
	if _, err := client.BeginUpload(ctx, "upl_test0002", "/workspace/g.bin", 8, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadChunk(ctx, "upl_test0002", 4, []byte("abcd")); err == nil {
		t.Fatal("gap chunk accepted")
	}
	// Overrun: declared size exceeded.
	if _, err := client.BeginUpload(ctx, "upl_test0003", "/workspace/o.bin", 4, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadChunk(ctx, "upl_test0003", 0, []byte("abcdefgh")); err == nil {
		t.Fatal("overrun chunk accepted")
	}
	// Digest mismatch: uploaded bytes don't match declared sha256.
	badSum := strings.Repeat("0", 64)
	if err := os.WriteFile(filepath.Join(workspace, "d.bin"), []byte("old target"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BeginUpload(ctx, "upl_test0004", "/workspace/d.bin", 4, badSum); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadChunk(ctx, "upl_test0004", 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CompleteUpload(ctx, "upl_test0004"); err == nil {
		t.Fatal("digest mismatch completed")
	}
	old, err := os.ReadFile(filepath.Join(workspace, "d.bin"))
	if err != nil || string(old) != "old target" {
		t.Fatal("failed upload replaced existing file")
	}
}

func TestGuestUploadEmptyFile(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	if _, err := client.BeginUpload(ctx, "upl_test0005", "/workspace/empty.txt", 0, ""); err != nil {
		t.Fatal(err)
	}
	done, err := client.CompleteUpload(ctx, "upl_test0005")
	if err != nil {
		t.Fatalf("empty complete: %v", err)
	}
	if !done.Complete {
		t.Fatal("empty upload not complete")
	}
	info, err := os.Stat(filepath.Join(workspace, "empty.txt"))
	if err != nil || info.Size() != 0 {
		t.Fatalf("empty file wrong: %v", err)
	}
}

func TestGuestUploadCompleteFreesSlot(t *testing.T) {
	client, _ := sandboxHelper(t)
	ctx := context.Background()
	// Four completions must not block a fifth begin (completed frees the slot).
	for i := 0; i < 4; i++ {
		id := "upl_slot" + string(rune('a'+i)) + "0001"
		if _, err := client.BeginUpload(ctx, id, "/workspace/f"+string(rune('a'+i))+".txt", 1, ""); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		if _, err := client.UploadChunk(ctx, id, 0, []byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := client.CompleteUpload(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.BeginUpload(ctx, "upl_slotf0001", "/workspace/f5.txt", 1, ""); err != nil {
		t.Fatalf("fifth begin blocked by completed uploads: %v", err)
	}
}

func TestGuestJobLogsUTF8(t *testing.T) {
	client, _ := sandboxHelper(t)
	ctx := context.Background()
	// Output that splits a multi-byte character across chunk boundaries.
	job, err := client.StartJob(ctx, JobRequest{ID: "job_utf8_0001", Command: "python3 -c 'print(\"héllo wörld éééé\")'", Directory: "/workspace", Sandbox: true, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		job, _ = client.Job(ctx, job.ID)
		if job.Status == "completed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if job.Status != "completed" {
		t.Fatalf("job did not complete: %s", job.Status)
	}
	// Read in small chunks; reconstruct must be byte-exact.
	var buf bytes.Buffer
	offset := int64(0)
	for {
		chunk, err := client.JobLogs(ctx, job.ID, "combined", offset, 4)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(chunk.Data)
		offset = chunk.NextOffset
		if chunk.EOF && len(chunk.Data) == 0 {
			break
		}
	}
	if !strings.Contains(buf.String(), "héllo wörld éééé") {
		t.Fatalf("UTF-8 output corrupted: %q", buf.String())
	}
}

func TestGuestJobLogsStreams(t *testing.T) {
	client, _ := sandboxHelper(t)
	ctx := context.Background()
	job, err := client.StartJob(ctx, JobRequest{ID: "job_split_0001", Command: "echo to-stdout; echo to-stderr >&2", Directory: "/workspace", Sandbox: true, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		job, _ = client.Job(ctx, job.ID)
		if job.Status == "completed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	stdout, err := client.JobLogs(ctx, job.ID, "stdout", 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := client.JobLogs(ctx, job.ID, "stderr", 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdout.Data), "to-stdout") || strings.Contains(string(stdout.Data), "to-stderr") {
		t.Fatalf("stdout stream polluted: %q", stdout.Data)
	}
	if !strings.Contains(string(stderr.Data), "to-stderr") {
		t.Fatalf("stderr stream missing: %q", stderr.Data)
	}
}

func TestGuestMetrics(t *testing.T) {
	if _, err := os.Stat("/proc/meminfo"); os.IsNotExist(err) {
		t.Skip("Linux /proc unavailable")
	}
	client, _ := sandboxHelper(t)
	m, err := client.Metrics(context.Background())
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if m.CPU.Count < 1 || m.Memory.TotalBytes <= 0 {
		t.Fatalf("metrics returned zeros: %+v", m)
	}
	if m.Disk.TotalBytes <= 0 || m.UptimeSeconds < 0 {
		t.Fatalf("disk/uptime invalid: %+v", m)
	}
}

func TestGuestUploadIDAllowsServerAlphabet(t *testing.T) {
	client, _ := sandboxHelper(t)
	ctx := context.Background()
	// newID() uses base64.RawURLEncoding — '_' and '-' appear in real server IDs.
	up, err := client.BeginUpload(ctx, "upl_ab-_cdef12", "/workspace/id.bin", 4, "")
	if err != nil {
		t.Fatalf("server-alphabet upload ID rejected: %v", err)
	}
	if _, err := client.UploadChunk(ctx, up.ID, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	done, err := client.CompleteUpload(ctx, up.ID)
	if err != nil || !done.Complete {
		t.Fatalf("complete: %v %+v", err, done)
	}
}

func TestGuestJobLogsOffsetRejects(t *testing.T) {
	client, _ := sandboxHelper(t)
	ctx := context.Background()
	job, err := client.StartJob(ctx, JobRequest{ID: "job_off_reject", Command: "echo short", Directory: "/workspace", Sandbox: true, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		job, _ = client.Job(ctx, job.ID)
		if job.Status == "completed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := client.JobLogs(ctx, job.ID, "combined", 1<<20, 4096); err == nil {
		t.Fatal("offset beyond log end accepted instead of rejected")
	}
}

func TestGuestDeadWorkerReconciles(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	job, err := client.StartJob(ctx, JobRequest{ID: "job_deadworker", Command: "sleep 300", Directory: "/workspace", Sandbox: true, TimeoutSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// Kill the recorded worker PID itself — inspect must reconcile to interrupted.
	stateRaw, err := os.ReadFile(filepath.Join(filepath.Dir(workspace), "state", "jobs", "job_deadworker", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		t.Fatal(err)
	}
	pid, _ := state["workerPid"].(float64)
	if pid == 0 {
		t.Fatalf("no workerPid in state: %v", state)
	}
	if child, ok := state["pid"].(float64); ok && child > 0 {
		t.Cleanup(func() { _ = syscall.Kill(-int(child), syscall.SIGKILL) })
	}
	if proc, err := os.FindProcess(int(pid)); err == nil {
		_ = proc.Kill()
	}
	time.Sleep(300 * time.Millisecond)
	logs, err := client.JobLogs(ctx, job.ID, "combined", 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if logs.Status != "interrupted" {
		t.Fatalf("log polling did not reconcile dead worker: %s", logs.Status)
	}
}

func TestGuestLogCapContinuesDrain(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	// >32MiB output: recording stops at cap but the process still completes.
	job, err := client.StartJob(ctx, JobRequest{ID: "job_bigout_001", Command: "python3 -c 'import sys; sys.stdout.write(\"x\"*40_000_000)'", Directory: "/workspace", Sandbox: true, TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		job, _ = client.Job(ctx, job.ID)
		if job.Status == "completed" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if job.Status != "completed" {
		t.Fatalf("capped job did not complete: %s", job.Status)
	}
	chunk, err := client.JobLogs(ctx, job.ID, "combined", 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !chunk.LogTruncated {
		t.Fatal("logTruncated absent from capped log chunk")
	}
	base := filepath.Join(filepath.Dir(workspace), "state", "jobs", job.ID)
	for _, name := range []string{"output.log", "stdout.log"} {
		stat, err := os.Stat(filepath.Join(base, name))
		if err != nil || stat.Size() != 32<<20 {
			t.Fatalf("%s was not capped at32MiB: %v %v", name, stat, err)
		}
	}
}

func TestGuestReadVersionRejectsChange(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	path := filepath.Join(workspace, "versioned.txt")
	if err := os.WriteFile(path, []byte("version-one"), 0644); err != nil {
		t.Fatal(err)
	}
	first, err := client.ReadFileVersion(ctx, "/workspace/versioned.txt", 0, 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version == "" {
		t.Fatal("no version token returned")
	}
	time.Sleep(1100 * time.Millisecond) // ensure mtime changes
	if err := os.WriteFile(path, []byte("version-two-different"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadFileVersion(ctx, "/workspace/versioned.txt", 0, 4096, first.Version); err == nil {
		t.Fatal("stale version accepted after file change")
	}
}

func TestGuestUserFilesFencesPrivatePaths(t *testing.T) {
	clientConfig, workspace, _ := helperSSH(t, true)
	clientConfig.UserFiles = true
	client, err := NewSSH(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	// Configure the client as a user surface.
	private := filepath.Join(workspace, ".canter")
	if err := os.MkdirAll(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "secret.txt"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	// Direct .canter path denied.
	if _, err := client.ReadFile(ctx, "/workspace/.canter/secret.txt", 0, 4096); err == nil {
		t.Fatal("user client read private .canter file")
	}
	// Symlink alias to private path denied (resolved path check).
	link := filepath.Join(workspace, "link-to-private")
	_ = os.Symlink(private, link)
	if _, err := client.ReadFile(ctx, "/workspace/link-to-private/secret.txt", 0, 4096); err == nil {
		t.Fatal("symlink alias bypassed private path guard")
	}
	for _, target := range []string{"/workspace/.canter", "/workspace/link-to-private"} {
		if _, err := client.ListFiles(ctx, target); err == nil {
			t.Fatalf("listed private directory %s", target)
		}
		if _, err := client.WriteFile(ctx, target+"/written.txt", []byte("no")); err == nil {
			t.Fatalf("wrote private directory %s", target)
		}
		if _, err := client.BeginUpload(ctx, "upl_private01", target+"/uploaded.bin", 1, ""); err == nil {
			t.Fatalf("upload into private directory %s", target)
		}
	}
	// Internal client (UserFiles=false) to the SAME server/workspace keeps
	// private access — the flag is per-client, not per-connection.
	internalConfig := clientConfig
	internalConfig.UserFiles = false
	internalClient, err := NewSSH(internalConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer internalClient.Close()
	if _, err := internalClient.ReadFile(ctx, "/workspace/.canter/secret.txt", 0, 4096); err != nil {
		t.Fatalf("internal client lost private access: %v", err)
	}
}

func TestGuestUploadConcurrentBeginAndComplete(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]FileUpload, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = client.BeginUpload(ctx, "upl_parallel0"+string(rune('a'+i)), "/workspace/concurrent-"+string(rune('a'+i)), 4, "")
		}(i)
	}
	wg.Wait()
	success := 0
	var first FileUpload
	for i, result := range results {
		if errs[i] == nil {
			success++
			first = result
		}
	}
	if success != 4 {
		t.Fatalf("concurrent begin admitted %d uploads, expected4", success)
	}
	if _, err := client.UploadChunk(ctx, first.ID, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	// Force both completers past the initial receipt check before either
	// acquires the upload lock. This deterministically reproduces the race.
	script := `
import concurrent.futures, fcntl, json, os, threading
import guest
barrier = threading.Barrier(2)
original = fcntl.flock
def lock(fd, operation):
    if operation == fcntl.LOCK_EX:
        barrier.wait(timeout=5)
    return original(fd, operation)
fcntl.flock = lock
with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
    futures = [pool.submit(guest.complete_upload, {"id": os.environ["TEST_UPLOAD_ID"]}) for _ in range(2)]
    receipts = [future.result(timeout=10) for future in futures]
assert receipts[0] == receipts[1], receipts
assert receipts[0]["complete"] and receipts[0]["offset"] == 4
print(json.dumps(receipts[0]))
`
	command := exec.Command("python3", "-c", script)
	command.Env = append(os.Environ(), "CANTER_COMPUTER_ROOT="+workspace, "CANTER_COMPUTER_STATE="+filepath.Join(filepath.Dir(workspace), "state"), "TEST_UPLOAD_ID="+first.ID)
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("concurrent completion: %v %s", err, raw)
	}
	var completed FileUpload
	if err := json.Unmarshal(raw, &completed); err != nil {
		t.Fatal(err)
	}
	replayed, err := client.BeginUpload(ctx, first.ID, first.Path, 4, "")
	if err != nil || replayed != completed {
		t.Fatalf("begin replay lost completed receipt: %+v %+v %v", replayed, completed, err)
	}
	again, err := client.CompleteUpload(ctx, first.ID)
	if err != nil || again != completed {
		t.Fatalf("complete replay mismatch: %+v %v", again, err)
	}
}

func TestGuestReadFIFOsDontHang(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	fifo := filepath.Join(workspace, "pipe.fifo")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Skip("mkfifo unavailable")
	}
	done := make(chan error, 1)
	go func() {
		_, err := client.ReadFile(ctx, "/workspace/pipe.fifo", 0, 4096)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO read returned data instead of invalid_file")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("FIFO read hung (O_NONBLOCK missing)")
	}
}

func TestGuestUploadExpiry(t *testing.T) {
	client, workspace := sandboxHelper(t)
	ctx := context.Background()
	up, err := client.BeginUpload(ctx, "upl_expired01", "/workspace/exp.bin", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	// The helper's state dir is a sibling of workspace: backdate the metadata
	// beyond the TTL so the next begin's sweep reclaims it.
	metaPath := filepath.Join(filepath.Dir(workspace), "state", "uploads", up.ID, "meta.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("upload meta not at %s: %v", metaPath, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	meta["updatedUnix"] = 1.0
	meta["createdUnix"] = 1.0
	stale, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, stale, 0600); err != nil {
		t.Fatal(err)
	}
	// A new begin sweeps the expired upload without counting it against the cap.
	if _, err := client.BeginUpload(ctx, "upl_fresh0001", "/workspace/new.bin", 4, ""); err != nil {
		t.Fatalf("begin blocked by expired upload: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(metaPath)); !os.IsNotExist(err) {
		t.Fatalf("expired upload not swept: err=%v", err)
	}
}

func TestGuestUploadAbortIdempotent(t *testing.T) {
	client, _ := sandboxHelper(t)
	ctx := context.Background()
	if _, err := client.BeginUpload(ctx, "upl_aborttest", "/workspace/ab.bin", 4, ""); err != nil {
		t.Fatal(err)
	}
	if err := client.AbortUpload(ctx, "upl_aborttest"); err != nil {
		t.Fatal(err)
	}
	// Second abort on a missing upload must be idempotent (no error).
	if err := client.AbortUpload(ctx, "upl_aborttest"); err != nil {
		t.Fatalf("abort not idempotent: %v", err)
	}
}

// Metrics fixture test wired into Go: run test_guest_metrics.py through python3.
func TestGuestMetricsFixture(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	out, err := exec.Command("python3", "test_guest_metrics.py").Output()
	if err != nil {
		t.Fatalf("metrics fixture failed: %v\n%s", err, out)
	}
	t.Logf("metrics fixture: %s", out)
}
