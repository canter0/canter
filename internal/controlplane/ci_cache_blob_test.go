package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func ciCacheBlobTestRoot(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "cache")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func ciCacheBlobTestID(n int) string {
	return strings.Repeat("0123456789abcdef"[n%16:n%16+1], 64)
}

func ciCacheBlobTestDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ciCacheBlobAllow(context.Context) error { return nil }

func ciCacheBlobOpenStore(t *testing.T, root string) *ciCacheBlobStore {
	t.Helper()
	store, err := newCICacheBlobStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func ciCacheBlobPut(t *testing.T, store *ciCacheBlobStore, id string, data []byte) {
	t.Helper()
	created, err := store.Put(context.Background(), id,
		io.NopCloser(strings.NewReader(string(data))), int64(len(data)),
		ciCacheBlobTestDigest(data), ciCacheBlobAllow)
	if err != nil || !created {
		t.Fatalf("put %s: created=%v err=%v", id[:8], created, err)
	}
}

type ciCacheBlobCountingReader struct {
	data     []byte
	reads    int
	consumed int64
	closes   int
}

func (r *ciCacheBlobCountingReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	r.consumed += int64(n)
	return n, nil
}

func (r *ciCacheBlobCountingReader) Close() error {
	r.closes++
	return nil
}

// ciCacheBlobFDNames lists the real process descriptor table; it fails the
// test when no fd directory can be enumerated rather than silently skipping.
func ciCacheBlobFDNames(t *testing.T) []string {
	t.Helper()
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		if entries, err := os.ReadDir(dir); err == nil {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return names
		}
		// Some platforms (e.g. macOS /dev/fd) reject path-based fstatat but
		// still serve descriptor listing through an open descriptor.
		if file, err := os.Open(dir); err == nil {
			names, readErr := file.Readdirnames(-1)
			file.Close()
			if readErr == nil {
				return names
			}
		}
	}
	t.Fatal("cannot enumerate process file descriptors")
	return nil
}

// ciCacheBlobFDs counts open descriptors matching pred.
func ciCacheBlobFDs(t *testing.T, pred func(*syscall.Stat_t) bool) int {
	t.Helper()
	count := 0
	for _, name := range ciCacheBlobFDNames(t) {
		fd, convErr := strconv.Atoi(name)
		if convErr != nil || fd < 0 {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil {
			continue
		}
		if pred(&st) {
			count++
		}
	}
	return count
}

// ciCacheBlobFDsFor counts open descriptors referring to one inode.
func ciCacheBlobFDsFor(t *testing.T, dev, ino uint64) int {
	t.Helper()
	return ciCacheBlobFDs(t, func(st *syscall.Stat_t) bool {
		return uint64(st.Dev) == dev && uint64(st.Ino) == ino
	})
}

// ciCacheBlobUnlinkedFDs counts open regular files with no remaining name;
// a leaked pending descriptor shows up here after its path is removed.
func ciCacheBlobUnlinkedFDs(t *testing.T) int {
	t.Helper()
	return ciCacheBlobFDs(t, func(st *syscall.Stat_t) bool {
		return st.Nlink == 0 && st.Mode&syscall.S_IFMT == syscall.S_IFREG &&
			st.Uid == uint32(os.Geteuid())
	})
}

// ciCacheBlobLockRefs reports how many operations hold or await the object
// lock, letting tests coordinate on real lock state instead of sleeps.
func ciCacheBlobLockRefs(store *ciCacheBlobStore, id string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	lock := store.locks[id]
	if lock == nil {
		return 0
	}
	return lock.refs
}

func ciCacheBlobWaitRefs(t *testing.T, store *ciCacheBlobStore, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ciCacheBlobLockRefs(store, id) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lock refs for %s never reached %d", id[:8], want)
}

type ciCacheBlobBlockingReader struct {
	data      []byte
	delivered bool
	started   chan struct{}
	sentOnce  sync.Once
	release   chan struct{}
	once      sync.Once
}

func newCICacheBlobBlockingReader(data []byte) *ciCacheBlobBlockingReader {
	return &ciCacheBlobBlockingReader{
		data:    data,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *ciCacheBlobBlockingReader) Read(p []byte) (int, error) {
	b.sentOnce.Do(func() { close(b.started) })
	if !b.delivered {
		b.delivered = true
		return copy(p, b.data), nil
	}
	<-b.release
	return 0, io.ErrClosedPipe
}

func (b *ciCacheBlobBlockingReader) Close() error {
	b.once.Do(func() { close(b.release) })
	return nil
}

func TestCICacheBlobRoundTripReopenAndDelete(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	data := []byte("canter-ci-cache-payload-\x00\x01\x02binary")
	id := ciCacheBlobTestID(1)
	ciCacheBlobPut(t, store, id, data)

	finalPath := filepath.Join(root, "objects", id+".blob")
	info, err := os.Lstat(finalPath)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("final mode: %v %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "pending", id+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending must be unlinked after publish")
	}

	rc, err := store.Open(context.Background(), id, int64(len(data)), ciCacheBlobTestDigest(data))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != string(data) {
		t.Fatalf("roundtrip mismatch: %v", err)
	}

	// Wrong digest or size must not expose the object.
	if _, err := store.Open(context.Background(), id, int64(len(data)), ciCacheBlobTestDigest([]byte("other"))); err == nil {
		t.Fatal("open with wrong digest must fail")
	}
	if _, err := store.Open(context.Background(), id, int64(len(data))+1, ciCacheBlobTestDigest(data)); err == nil {
		t.Fatal("open with wrong size must fail")
	}
	if _, err := store.Open(context.Background(), ciCacheBlobTestID(9), int64(len(data)), ciCacheBlobTestDigest(data)); err == nil {
		t.Fatal("open missing object must fail")
	}

	// No descriptor may still reference the published inode after Put returns.
	info2, err := os.Lstat(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	stat := info2.Sys().(*syscall.Stat_t)
	if n := ciCacheBlobFDsFor(t, uint64(stat.Dev), uint64(stat.Ino)); n != 0 {
		t.Fatalf("put leaked %d descriptors on published inode", n)
	}

	digest := store.BackendDigest()
	if len(digest) != 64 {
		t.Fatalf("backend digest shape: %q", digest)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Reopen keeps the same fingerprint.
	reopened := ciCacheBlobOpenStore(t, root)
	if reopened.BackendDigest() != digest {
		t.Fatal("backend digest must be stable across reopen")
	}
	absent, err := reopened.Delete(context.Background(), id)
	if err != nil || !absent {
		t.Fatalf("delete: absent=%v err=%v", absent, err)
	}
	if _, err := reopened.Open(context.Background(), id, int64(len(data)), ciCacheBlobTestDigest(data)); err == nil {
		t.Fatal("open after delete must fail")
	}
	// Deleting an absent object reports absence without error.
	absent, err = reopened.Delete(context.Background(), id)
	if err != nil || !absent {
		t.Fatalf("second delete: absent=%v err=%v", absent, err)
	}
	reopened.Close()
}

func TestCICacheBlobPutCollisionAndStreamFailures(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	defer store.Close()
	ctx := context.Background()
	id := ciCacheBlobTestID(2)
	first := []byte("first-wins")
	ciCacheBlobPut(t, store, id, first)

	// A second put of different content must not overwrite and must not even
	// read the body: the sealed final wins the fast path.
	second := []byte("second-loses!")
	collisionSrc := &ciCacheBlobCountingReader{data: second}
	created, err := store.Put(ctx, id, collisionSrc,
		int64(len(second)), ciCacheBlobTestDigest(second), ciCacheBlobAllow)
	if err != nil || created {
		t.Fatalf("collision put: created=%v err=%v", created, err)
	}
	if collisionSrc.reads != 0 {
		t.Fatal("collision fast path must not read the source")
	}
	if collisionSrc.closes != 1 {
		t.Fatalf("source must be closed exactly once, got %d", collisionSrc.closes)
	}
	rc, err := store.Open(ctx, id, int64(len(first)), ciCacheBlobTestDigest(first))
	if err != nil {
		t.Fatalf("open after collision: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(first) {
		t.Fatal("first write must win")
	}

	badID := ciCacheBlobTestID(3)
	data := []byte("payload")
	digest := ciCacheBlobTestDigest(data)
	unlinkedBaseline := ciCacheBlobUnlinkedFDs(t)
	for name, mutate := range map[string]func() (int64, string){
		"short source":  func() (int64, string) { return int64(len(data) + 5), digest },
		"long source":   func() (int64, string) { return int64(len(data) - 1), digest },
		"wrong digest":  func() (int64, string) { return int64(len(data)), ciCacheBlobTestDigest([]byte("x")) },
		"zero size":     func() (int64, string) { return 0, digest },
		"oversize":      func() (int64, string) { return ciCacheBlobMaxSize + 1, digest },
		"huge declared": func() (int64, string) { return int64(len(data)), strings.Repeat("0", 64) },
	} {
		size, dg := mutate()
		sourceData := append([]byte(nil), data...)
		if name == "long source" {
			sourceData = append(sourceData, 0xde, 0xad)
		}
		src := &ciCacheBlobCountingReader{data: sourceData}
		created, err := store.Put(ctx, badID, src, size, dg, ciCacheBlobAllow)
		if err == nil || created {
			t.Fatalf("%s: expected failure, created=%v err=%v", name, created, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "pending", badID+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed puts must remove their own pending file")
	}
	if _, err := os.Lstat(filepath.Join(root, "objects", badID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed puts must not publish")
	}

	// Oversize is rejected before any read.
	src := &ciCacheBlobCountingReader{data: data}
	if _, err := store.Put(ctx, badID, src, ciCacheBlobMaxSize+1, digest, ciCacheBlobAllow); err == nil {
		t.Fatal("oversize must reject")
	}
	if src.reads != 0 {
		t.Fatal("oversize must reject before reading source")
	}

	// A source that always fills the offered buffer proves the stream is capped
	// at size+1 bytes read and never writes past the declared size.
	greedyID := ciCacheBlobTestID(12)
	greedy := &ciCacheBlobCountingReader{data: []byte(strings.Repeat("z", 1<<20))}
	if _, err := store.Put(ctx, greedyID, greedy, 1,
		ciCacheBlobTestDigest([]byte("z")), ciCacheBlobAllow); err == nil {
		t.Fatal("greedy oversized stream must reject")
	}
	if greedy.consumed > 2 {
		t.Fatalf("put consumed %d bytes, want <= size+1", greedy.consumed)
	}
	if _, err := os.Lstat(filepath.Join(root, "pending", greedyID+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("greedy put must remove its pending file")
	}
	if n := ciCacheBlobUnlinkedFDs(t); n != unlinkedBaseline {
		t.Fatalf("put leaked %d unlinked descriptors", n-unlinkedBaseline)
	}

	// Invalid IDs never touch the filesystem.
	for _, bad := range []string{"", "abc", strings.Repeat("A", 64), "../" + ciCacheBlobTestID(4),
		ciCacheBlobTestID(4) + ".blob", strings.Repeat("g", 64)} {
		if _, err := store.Put(ctx, bad, io.NopCloser(strings.NewReader("x")), 1,
			ciCacheBlobTestDigest([]byte("x")), ciCacheBlobAllow); err == nil {
			t.Fatalf("invalid id %q must reject", bad)
		}
	}

	// Nil context and nil source reject before any use.
	if _, err := store.Put(nil, badID, io.NopCloser(strings.NewReader("x")), 1,
		ciCacheBlobTestDigest([]byte("x")), ciCacheBlobAllow); err == nil {
		t.Fatal("nil context put must reject")
	}
	if _, err := store.Put(ctx, badID, nil, 1,
		ciCacheBlobTestDigest([]byte("x")), ciCacheBlobAllow); err == nil {
		t.Fatal("nil source put must reject")
	}
	if _, err := store.Open(nil, badID, 1, ciCacheBlobTestDigest([]byte("x"))); err == nil {
		t.Fatal("nil context open must reject")
	}
	if _, err := store.Delete(nil, badID); err == nil {
		t.Fatal("nil context delete must reject")
	}

	// An existing pending file is preserved, never overwritten or deleted.
	pendingID := ciCacheBlobTestID(5)
	pendingPath := filepath.Join(root, "pending", pendingID+".part")
	if err := os.WriteFile(pendingPath, []byte("in-flight"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, pendingID, io.NopCloser(strings.NewReader("new")), 3,
		ciCacheBlobTestDigest([]byte("new")), ciCacheBlobAllow); !errors.Is(err, errCICacheBlobConflict) {
		t.Fatalf("existing pending must conflict, got %v", err)
	}
	kept, _ := os.ReadFile(pendingPath)
	if string(kept) != "in-flight" {
		t.Fatal("existing pending must be preserved for cleanup")
	}
}

func TestCICacheBlobRejectsUnsafeObjects(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	defer store.Close()
	ctx := context.Background()
	id := ciCacheBlobTestID(6)
	data := []byte("guarded")
	ciCacheBlobPut(t, store, id, data)
	finalPath := filepath.Join(root, "objects", id+".blob")
	digest := ciCacheBlobTestDigest(data)

	// Wrong mode rejects.
	if err := os.Chmod(finalPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, id, int64(len(data)), digest); err == nil {
		t.Fatal("0644 object must reject")
	}
	if err := os.Chmod(finalPath, 0400); err != nil {
		t.Fatal(err)
	}

	// Corrupted bytes reject on open.
	if err := os.Chmod(finalPath, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(finalPath)
	raw[0] ^= 0xff
	if err := os.WriteFile(finalPath, raw, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(finalPath, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, id, int64(len(data)), digest); err == nil {
		t.Fatal("corrupted object must reject")
	}
	if err := os.Chmod(finalPath, 0600); err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xff
	if err := os.WriteFile(finalPath, raw, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(finalPath, 0400); err != nil {
		t.Fatal(err)
	}

	// A symlink at the object path rejects without following it.
	outside := filepath.Join(t.TempDir(), "outside-target")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, finalPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, id, int64(len(data)), digest); err == nil {
		t.Fatal("symlink object must reject")
	}
	if absent, err := store.Delete(ctx, id); err == nil && absent {
		t.Fatal("delete of symlink must not report absence")
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Fatal("delete must not touch files outside the store root")
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}

	// A FIFO at the object path rejects without blocking.
	if err := syscall.Mkfifo(finalPath, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.Open(ctx, id, int64(len(data)), digest)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO object must reject")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("open on FIFO blocked")
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}

	// A directory at the object path rejects as non-regular.
	if err := os.Mkdir(finalPath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, id, int64(len(data)), digest); err == nil {
		t.Fatal("directory object must reject")
	}
	if absent, err := store.Delete(ctx, id); err == nil && absent {
		t.Fatal("delete of directory must not report absence")
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}

	// A foreign hardlink (nlink=2 without our pending pair) rejects delete.
	ciCacheBlobPut(t, store, id, data)
	foreign := filepath.Join(t.TempDir(), "foreign-link")
	if err := os.Link(finalPath, foreign); err != nil {
		t.Fatal(err)
	}
	if absent, err := store.Delete(ctx, id); err == nil {
		t.Fatalf("foreign-linked object delete must fail, absent=%v", absent)
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatal("foreign link target must be preserved")
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	absent, err := store.Delete(ctx, id)
	if err != nil || !absent {
		t.Fatalf("delete after foreign link removed: absent=%v err=%v", absent, err)
	}
}

func TestCICacheBlobCrashPairDelete(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	defer store.Close()
	ctx := context.Background()
	id := ciCacheBlobTestID(7)
	objectsDir := filepath.Join(root, "objects")
	pendingDir := filepath.Join(root, "pending")
	finalPath := filepath.Join(objectsDir, id+".blob")
	pendingPath := filepath.Join(pendingDir, id+".part")

	// Simulate a crash between hardlink and pending unlink: same inode, nlink=2.
	if err := os.WriteFile(pendingPath, []byte("crashed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pendingPath, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(pendingPath, finalPath); err != nil {
		t.Fatal(err)
	}
	absent, err := store.Delete(ctx, id)
	if err != nil || !absent {
		t.Fatalf("crash-pair delete: absent=%v err=%v", absent, err)
	}
	for _, path := range []string{finalPath, pendingPath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must be gone", path)
		}
	}
}

func TestCICacheBlobContextAndCloseCancellation(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	defer store.Close()
	ctx := context.Background()
	id := ciCacheBlobTestID(8)
	data := []byte("cancel-me")
	digest := ciCacheBlobTestDigest(data)

	// Cancelled context before Put rejects and still closes the source.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	src := &ciCacheBlobCountingReader{data: data}
	if _, err := store.Put(cancelled, id, src, int64(len(data)), digest, ciCacheBlobAllow); err == nil {
		t.Fatal("cancelled put must fail")
	}
	if src.reads != 0 {
		t.Fatal("cancelled put must not read source")
	}

	// Context cancelled mid-Put closes the blocked source and fails.
	blocking := newCICacheBlobBlockingReader(data)
	putCtx, putCancel := context.WithCancel(ctx)
	putDone := make(chan error, 1)
	go func() {
		_, err := store.Put(putCtx, id, blocking, int64(len(data)), digest, ciCacheBlobAllow)
		putDone <- err
	}()
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("put never started reading")
	}
	putCancel()
	select {
	case err := <-putDone:
		if err == nil {
			t.Fatal("cancelled mid-put must fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("put did not abort on context cancel")
	}
	if _, err := os.Lstat(filepath.Join(root, "objects", id+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled put must not publish")
	}

	// Close cancels an in-flight Put and waits for it; nothing publishes late.
	id2 := ciCacheBlobTestID(9)
	blocking2 := newCICacheBlobBlockingReader(data)
	putDone2 := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, id2, blocking2, int64(len(data)), digest, ciCacheBlobAllow)
		putDone2 <- err
	}()
	select {
	case <-blocking2.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second put never started reading")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close did not wait for in-flight put")
	}
	select {
	case err := <-putDone2:
		if err == nil {
			t.Fatal("put interrupted by close must fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("put did not finish before close returned")
	}
	for _, path := range []string{
		filepath.Join(root, "objects", id2+".blob"),
		filepath.Join(root, "pending", id2+".part"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must not survive close", path)
		}
	}
	// The process lock is released: a fresh store opens the same root.
	reopened := ciCacheBlobOpenStore(t, root)
	if reopened.BackendDigest() != store.BackendDigest() {
		t.Fatal("digest must survive close/reopen")
	}
	reopened.Close()
}

func TestCICacheBlobDeleteWaitsForReadersAndWriters(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	defer store.Close()
	ctx := context.Background()
	id := ciCacheBlobTestID(10)
	data := []byte("locked-object")
	ciCacheBlobPut(t, store, id, data)

	// Delete waits for an open reader to close.
	rc, err := store.Open(ctx, id, int64(len(data)), ciCacheBlobTestDigest(data))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	type deleteResult struct {
		absent bool
		err    error
	}
	// Delete queues behind the held read lock: once both operations are
	// registered on the object lock, the writer cannot proceed until the
	// reader closes — proven by lock state, not a sleep.
	delDone := make(chan deleteResult, 1)
	go func() {
		absent, err := store.Delete(ctx, id)
		delDone <- deleteResult{absent, err}
	}()
	ciCacheBlobWaitRefs(t, store, id, 2)
	select {
	case res := <-delDone:
		t.Fatalf("delete finished while reader held lock: %+v", res)
	default:
	}
	rc.Close()
	select {
	case res := <-delDone:
		if res.err != nil || !res.absent {
			t.Fatalf("delete after reader close: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete did not proceed after reader close")
	}

	// Delete waits for an in-flight writer on the same object.
	writerID := ciCacheBlobTestID(11)
	blocking := newCICacheBlobBlockingReader(data)
	putDone := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, writerID, blocking, int64(len(data)), ciCacheBlobTestDigest(data), ciCacheBlobAllow)
		putDone <- err
	}()
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never started")
	}
	delDone2 := make(chan deleteResult, 1)
	go func() {
		absent, err := store.Delete(ctx, writerID)
		delDone2 <- deleteResult{absent, err}
	}()
	ciCacheBlobWaitRefs(t, store, writerID, 2)
	select {
	case res := <-delDone2:
		t.Fatalf("delete finished while writer held lock: %+v", res)
	default:
	}
	blocking.Close()
	select {
	case err := <-putDone:
		if err == nil {
			t.Fatal("interrupted writer must fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not abort")
	}
	select {
	case res := <-delDone2:
		if res.err != nil || !res.absent {
			t.Fatalf("delete after writer abort: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete did not proceed after writer released")
	}
}

func TestCICacheBlobSingletonAndCtorValidation(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	defer store.Close()

	// A second in-process instance is rejected while the lock is held.
	if _, err := newCICacheBlobStore(root); err == nil {
		t.Fatal("second instance must be rejected")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Unknown files in the root are refused and preserved — tested with the
	// flock free so the children check is what actually rejects.
	stray := filepath.Join(root, "stray.txt")
	if err := os.WriteFile(stray, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newCICacheBlobStore(root); err == nil {
		t.Fatal("unknown root child must reject")
	}
	if _, err := os.Lstat(stray); err != nil {
		t.Fatal("unknown file must be preserved")
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	reopened := ciCacheBlobOpenStore(t, root)
	reopened.Close()

	// Unsafe parent permissions reject.
	parent := filepath.Dir(root)
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := newCICacheBlobStore(root); err == nil {
		t.Fatal("0777 parent must reject")
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}

	// A symlinked root path rejects (non-canonical alias).
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := newCICacheBlobStore(alias); err == nil {
		t.Fatal("symlink alias must reject")
	}

	// Wrong root mode rejects.
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := newCICacheBlobStore(root); err == nil {
		t.Fatal("0755 root must reject")
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}

	// Corrupt marker JSON fails closed: restore exact 0400 mode first so the
	// descriptor check passes and the parser is what rejects.
	marker := filepath.Join(root, "format.json")
	if err := os.Chmod(marker, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(marker, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := newCICacheBlobStore(root); err == nil {
		t.Fatal("corrupt marker must reject")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	// Children without a marker are a partial/corrupt state: reject, do not
	// silently reinitialize.
	if _, err := newCICacheBlobStore(root); err == nil {
		t.Fatal("missing marker with existing children must reject")
	}
}

func TestCICacheBlobConcurrentCloseWaits(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	ctx := context.Background()
	id := ciCacheBlobTestID(13)
	data := []byte("close-race")
	digest := ciCacheBlobTestDigest(data)
	blocking := newCICacheBlobBlockingReader(data)
	putDone := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, id, blocking, int64(len(data)), digest, ciCacheBlobAllow)
		putDone <- err
	}()
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("put never started")
	}
	// Two concurrent Close callers must both wait for the same completion:
	// the in-flight put can only finish after cancellation propagates.
	results := make(chan error, 2)
	go func() { results <- store.Close() }()
	go func() { results <- store.Close() }()
	select {
	case err := <-putDone:
		if err == nil {
			t.Fatal("put interrupted by close must fail")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("put did not abort on close")
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("close: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent close did not complete")
		}
	}
	// A closed store rejects every new operation.
	if _, err := store.Put(ctx, ciCacheBlobTestID(14),
		io.NopCloser(strings.NewReader("x")), 1,
		ciCacheBlobTestDigest([]byte("x")), ciCacheBlobAllow); err == nil {
		t.Fatal("put on closed store must reject")
	}
	if _, err := store.Open(ctx, id, int64(len(data)), digest); err == nil {
		t.Fatal("open on closed store must reject")
	}
	if _, err := store.Delete(ctx, id); err == nil {
		t.Fatal("delete on closed store must reject")
	}
	reopened := ciCacheBlobOpenStore(t, root)
	defer reopened.Close()
	if reopened.BackendDigest() != store.BackendDigest() {
		t.Fatal("digest must survive concurrent close")
	}
}

func TestCICacheBlobAuthorizeSeam(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	defer store.Close()
	ctx := context.Background()
	id := ciCacheBlobTestID(15)
	data := []byte("authorized")
	digest := ciCacheBlobTestDigest(data)

	// A nil authorize callback rejects before any I/O; the source still closes.
	src := &ciCacheBlobCountingReader{data: data}
	if _, err := store.Put(ctx, id, src, int64(len(data)), digest, nil); err == nil {
		t.Fatal("nil authorize must reject")
	}
	if src.reads != 0 || src.closes != 1 {
		t.Fatalf("nil authorize: reads=%d closes=%d", src.reads, src.closes)
	}

	// A denied callback propagates its error with no read, one close, and no
	// pending or final files.
	denySrc := &ciCacheBlobCountingReader{data: data}
	_, err := store.Put(ctx, id, denySrc, int64(len(data)), digest,
		func(context.Context) error { return ErrUnauthorized })
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("deny callback error must propagate, got %v", err)
	}
	if denySrc.reads != 0 || denySrc.closes != 1 {
		t.Fatalf("deny: reads=%d closes=%d", denySrc.reads, denySrc.closes)
	}
	for _, path := range []string{
		filepath.Join(root, "objects", id+".blob"),
		filepath.Join(root, "pending", id+".part"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("denied put must leave nothing at %s", path)
		}
	}

	// A cancelled context never invokes the callback at all.
	invoked := false
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Put(cancelled, id,
		io.NopCloser(strings.NewReader(string(data))), int64(len(data)), digest,
		func(context.Context) error { invoked = true; return nil }); err == nil {
		t.Fatal("cancelled put must fail")
	}
	if invoked {
		t.Fatal("authorize must not run on an already-cancelled context")
	}

	// Atomic cleanup model: once metadata marks the object for cleanup and the
	// backend delete reports absence, a late scheduled put whose authorize
	// rechecks the flag must fail and create neither file.
	permitted := true
	lateID := ciCacheBlobTestID(16)
	permitted = false
	if absent, err := store.Delete(ctx, lateID); err != nil || !absent {
		t.Fatalf("pre-delete: absent=%v err=%v", absent, err)
	}
	lateSrc := &ciCacheBlobCountingReader{data: data}
	_, err = store.Put(ctx, lateID, lateSrc, int64(len(data)), digest,
		func(context.Context) error {
			if !permitted {
				return ErrUnauthorized
			}
			return nil
		})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("late put after cleanup mark must reject, got %v", err)
	}
	for _, path := range []string{
		filepath.Join(root, "objects", lateID+".blob"),
		filepath.Join(root, "pending", lateID+".part"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("late put resurrected %s", path)
		}
	}

	// Ordering proof: a blocked authorize holds the object lock, so Delete
	// queues behind it; once released, the writer completes and Delete then
	// removes the object.
	entered := make(chan struct{})
	release := make(chan struct{})
	var ctxLive bool
	orderID := ciCacheBlobTestID(17)
	putDone := make(chan error, 1)
	go func() {
		created, err := store.Put(ctx, orderID,
			io.NopCloser(strings.NewReader(string(data))), int64(len(data)), digest,
			func(opCtx context.Context) error {
				ctxLive = opCtx.Err() == nil
				close(entered)
				<-release
				return nil
			})
		if err == nil && !created {
			err = errors.New("put reported not created")
		}
		putDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("authorize was never invoked")
	}
	if !ctxLive {
		t.Fatal("authorize must receive a live operation context")
	}
	type deleteResult struct {
		absent bool
		err    error
	}
	delDone := make(chan deleteResult, 1)
	go func() {
		absent, err := store.Delete(ctx, orderID)
		delDone <- deleteResult{absent, err}
	}()
	ciCacheBlobWaitRefs(t, store, orderID, 2)
	select {
	case res := <-delDone:
		t.Fatalf("delete finished while authorize held the lock: %+v", res)
	default:
	}
	close(release)
	select {
	case err := <-putDone:
		if err != nil {
			t.Fatalf("writer after authorize release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not complete")
	}
	select {
	case res := <-delDone:
		if res.err != nil || !res.absent {
			t.Fatalf("delete after writer: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete did not run after writer released")
	}
	if _, err := os.Lstat(filepath.Join(root, "objects", orderID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("object must be absent after delete")
	}
}

func TestCICacheBlobReexecChild(t *testing.T) {
	root := os.Getenv("CANTER_CI_BLOB_REEXEC_ROOT")
	if root == "" {
		t.Skip("reexec child only")
	}
	if _, err := newCICacheBlobStore(root); err == nil {
		t.Fatal("second process must be rejected by flock")
	}
}

func TestCICacheBlobSingletonAcrossProcesses(t *testing.T) {
	root := ciCacheBlobTestRoot(t)
	store := ciCacheBlobOpenStore(t, root)
	defer store.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCICacheBlobReexecChild$", "-test.v")
	cmd.Env = append(os.Environ(), "CANTER_CI_BLOB_REEXEC_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reexec child failed: %v\n%s", err, out)
	}
}
