package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
)

const (
	ciCacheBlobMaxSize   = 512 << 20
	ciCacheBlobMarkerMax = 4096
)

var (
	errCICacheBlobInvalid  = errors.New("ci cache blob request is invalid")
	errCICacheBlobClosed   = errors.New("ci cache blob store is closed")
	errCICacheBlobConflict = errors.New("ci cache blob pending object already exists")
	errCICacheBlobMissing  = errors.New("ci cache blob object is absent")
	errCICacheBlobCorrupt  = errors.New("ci cache blob object failed integrity checks")
	errCICacheBlobIO       = errors.New("ci cache blob storage failure")
	ciCacheBlobHexPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type ciCacheBlobStore struct {
	root      string
	digest    string
	lockFile  *os.File
	opCtx     context.Context
	opCancel  context.CancelFunc
	mu        sync.Mutex
	closed    bool
	active    sync.WaitGroup
	locks     map[string]*ciCacheBlobLock
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

type ciCacheBlobLock struct {
	mu   sync.RWMutex
	refs int
}

func ciCacheBlobPermExact(mode os.FileMode, perm os.FileMode) bool {
	return mode.Perm() == perm &&
		mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

func ciCacheBlobStatOK(info os.FileInfo, mode os.FileMode, links uint64) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return info.Mode().IsRegular() && stat.Uid == uint32(os.Geteuid()) &&
		ciCacheBlobPermExact(info.Mode(), mode) && uint64(stat.Nlink) == links
}

func ciCacheBlobSyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return errCICacheBlobIO
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errCICacheBlobIO
	}
	return nil
}

// newCICacheBlobStore opens or initializes a private filesystem blob root. The
// process holds an exclusive nonblocking flock for the store lifetime; a second
// opener is rejected so a late writer can never race a deleter.
func newCICacheBlobStore(dir string) (*ciCacheBlobStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, errCICacheBlobInvalid
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || canonical != dir {
		return nil, errCICacheBlobInvalid
	}
	rootInfo, err := os.Lstat(dir)
	if err != nil || !rootInfo.IsDir() ||
		!ciCacheBlobPermExact(rootInfo.Mode(), 0700) ||
		rootInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errCICacheBlobInvalid
	}
	parentInfo, err := os.Lstat(filepath.Dir(dir))
	if err != nil || !parentInfo.IsDir() ||
		(parentInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) &&
			parentInfo.Sys().(*syscall.Stat_t).Uid != 0) ||
		parentInfo.Mode().Perm()&022 != 0 {
		return nil, errCICacheBlobInvalid
	}
	store := &ciCacheBlobStore{
		root:      dir,
		locks:     map[string]*ciCacheBlobLock{},
		closeDone: make(chan struct{}),
	}
	store.opCtx, store.opCancel = context.WithCancel(context.Background())
	fail := func() (*ciCacheBlobStore, error) {
		store.opCancel()
		if store.lockFile != nil {
			store.lockFile.Close()
		}
		return nil, errCICacheBlobInvalid
	}
	lockPath := filepath.Join(dir, "cache.lock")
	lockFile, err := os.OpenFile(lockPath,
		os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if errors.Is(err, fs.ErrExist) {
		lockFile, err = os.OpenFile(lockPath,
			os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	}
	if err != nil {
		return fail()
	}
	lockInfo, err := lockFile.Stat()
	if err != nil || !ciCacheBlobStatOK(lockInfo, 0600, 1) {
		lockFile.Close()
		return fail()
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		return fail()
	}
	store.lockFile = lockFile
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fail()
	}
	known := map[string]bool{
		"cache.lock": true, "format.json": true,
		"objects": true, "pending": true,
	}
	for _, entry := range entries {
		if !known[entry.Name()] {
			return fail()
		}
	}
	digest, err := ciCacheBlobLoadOrCreateMarker(dir)
	if err != nil {
		return fail()
	}
	store.digest = digest
	for _, child := range []string{"objects", "pending"} {
		path := filepath.Join(dir, child)
		info, statErr := os.Lstat(path)
		if errors.Is(statErr, fs.ErrNotExist) {
			if err := os.Mkdir(path, 0700); err != nil {
				return fail()
			}
			info, statErr = os.Lstat(path)
		}
		if statErr != nil || !info.IsDir() ||
			!ciCacheBlobPermExact(info.Mode(), 0700) ||
			info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			return fail()
		}
	}
	if err := ciCacheBlobSyncDir(dir); err != nil {
		return fail()
	}
	return store, nil
}

func ciCacheBlobLoadOrCreateMarker(dir string) (string, error) {
	path := filepath.Join(dir, "format.json")
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		for _, child := range []string{"objects", "pending"} {
			if _, err := os.Lstat(filepath.Join(dir, child)); err == nil {
				return "", errCICacheBlobInvalid
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", errCICacheBlobIO
			}
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", errCICacheBlobIO
		}
		digest := hex.EncodeToString(raw)
		body, err := json.Marshal(map[string]any{
			"version":       1,
			"backendDigest": digest,
		})
		if err != nil {
			return "", errCICacheBlobIO
		}
		file, err := os.OpenFile(path,
			os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err != nil {
			return "", errCICacheBlobIO
		}
		if _, err := file.Write(body); err != nil {
			file.Close()
			return "", errCICacheBlobIO
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return "", errCICacheBlobIO
		}
		if err := file.Chmod(0400); err != nil {
			file.Close()
			return "", errCICacheBlobIO
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return "", errCICacheBlobIO
		}
		if err := file.Close(); err != nil {
			return "", errCICacheBlobIO
		}
		if err := ciCacheBlobSyncDir(dir); err != nil {
			return "", err
		}
		return digest, nil
	}
	if err != nil {
		return "", errCICacheBlobIO
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errCICacheBlobIO
	}
	defer file.Close()
	// The open descriptor is the authority: fstat after the nofollow open,
	// never trusting a path-based check alone.
	fileInfo, err := file.Stat()
	if err != nil || !fileInfo.Mode().IsRegular() ||
		!ciCacheBlobPermExact(fileInfo.Mode(), 0400) ||
		fileInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) ||
		fileInfo.Sys().(*syscall.Stat_t).Nlink != 1 ||
		fileInfo.Size() > ciCacheBlobMarkerMax || fileInfo.Size() == 0 {
		return "", errCICacheBlobInvalid
	}
	body, err := io.ReadAll(io.LimitReader(file, ciCacheBlobMarkerMax+1))
	if err != nil || len(body) > ciCacheBlobMarkerMax {
		return "", errCICacheBlobInvalid
	}
	var marker map[string]json.RawMessage
	if ciEnforcerStrictJSON(body, &marker) != nil || len(marker) != 2 {
		return "", errCICacheBlobInvalid
	}
	var version int
	var digest string
	if json.Unmarshal(marker["version"], &version) != nil || version != 1 ||
		marker["version"][0] != '1' ||
		json.Unmarshal(marker["backendDigest"], &digest) != nil ||
		!ciCacheBlobHexPattern.MatchString(digest) {
		return "", errCICacheBlobInvalid
	}
	return digest, nil
}

func (s *ciCacheBlobStore) BackendDigest() string {
	return s.digest
}

// Close cancels in-flight I/O, waits for every active operation and queued
// acquirer to finish, and only then releases the process flock. Concurrent
// callers all wait for the same completion and observe the same result.
func (s *ciCacheBlobStore) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.opCancel()
		s.mu.Unlock()
		s.active.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lockFile != nil {
			if err := s.lockFile.Close(); err != nil {
				s.closeErr = errCICacheBlobIO
			}
			s.lockFile = nil
		}
		close(s.closeDone)
	})
	<-s.closeDone
	return s.closeErr
}

func (s *ciCacheBlobStore) acquire(id string, write bool) (func(), error) {
	s.mu.Lock()
	if s.closed || s.opCtx.Err() != nil {
		s.mu.Unlock()
		return nil, errCICacheBlobClosed
	}
	s.active.Add(1)
	lock := s.locks[id]
	if lock == nil {
		lock = &ciCacheBlobLock{}
		s.locks[id] = lock
	}
	lock.refs++
	s.mu.Unlock()
	if write {
		lock.mu.Lock()
	} else {
		lock.mu.RLock()
	}
	release := func() {
		if write {
			lock.mu.Unlock()
		} else {
			lock.mu.RUnlock()
		}
		s.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.locks, id)
		}
		s.mu.Unlock()
		s.active.Done()
	}
	return release, nil
}

func (s *ciCacheBlobStore) opContext(ctx context.Context) (context.Context, context.CancelFunc, func()) {
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.opCtx, cancel)
	if s.opCtx.Err() != nil {
		// The store may already be closing while this op was queued on the
		// object lock; do not rely on the asynchronous AfterFunc alone.
		cancel()
	}
	return opCtx, cancel, func() { stop(); cancel() }
}

func ciCacheBlobValidID(id string) bool {
	return ciCacheBlobHexPattern.MatchString(id)
}

func ciCacheBlobValidDigest(digest string) bool {
	return ciCacheBlobHexPattern.MatchString(digest)
}

func ciCacheBlobValidSize(size int64) bool {
	return size >= 1 && size <= ciCacheBlobMaxSize
}

func (s *ciCacheBlobStore) objectPaths(id string) (string, string) {
	return filepath.Join(s.root, "objects", id+".blob"),
		filepath.Join(s.root, "pending", id+".part")
}

// ciCacheBlobSource closes the caller's body exactly once no matter how many
// cancellation hooks or deferred paths ask for it.
type ciCacheBlobSource struct {
	rc   io.ReadCloser
	once sync.Once
	err  error
}

func (s *ciCacheBlobSource) Read(p []byte) (int, error) { return s.rc.Read(p) }

func (s *ciCacheBlobSource) Close() error {
	s.once.Do(func() { s.err = s.rc.Close() })
	return s.err
}

// ciCacheBlobFinalUsable reports whether an existing final object is a valid
// sealed store-owned file; used for both the put fast path and link races.
func ciCacheBlobFinalUsable(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && ciCacheBlobStatOK(info, 0400, 1)
}

// Put streams one immutable blob. authorize is invoked while holding the
// per-object exclusive lock, before any filesystem mutation or source read;
// the future HTTP layer uses it to atomically recheck the admitted job and
// claim the DB write reservation under the same serialization point, so a
// cleanup that already marked the object can never be resurrected by a late
// writer whose claim now fails.
func (s *ciCacheBlobStore) Put(ctx context.Context, id string, source io.ReadCloser,
	size int64, digest string, authorize func(context.Context) error) (bool, error) {
	if source == nil {
		return false, errCICacheBlobInvalid
	}
	body := &ciCacheBlobSource{rc: source}
	defer body.Close()
	if ctx == nil || !ciCacheBlobValidID(id) ||
		!ciCacheBlobValidDigest(digest) || !ciCacheBlobValidSize(size) ||
		authorize == nil {
		return false, errCICacheBlobInvalid
	}
	release, err := s.acquire(id, true)
	if err != nil {
		return false, err
	}
	defer release()
	opCtx, _, cleanup := s.opContext(ctx)
	defer cleanup()
	if opCtx.Err() != nil {
		return false, errCICacheBlobIO
	}
	if err := authorize(opCtx); err != nil {
		return false, err
	}
	if opCtx.Err() != nil {
		return false, errCICacheBlobIO
	}
	finalPath, pendingPath := s.objectPaths(id)
	// First-writer-wins fast path: a valid sealed final already exists, so the
	// body is never read and the source is only closed.
	if _, err := os.Lstat(finalPath); err == nil {
		if !ciCacheBlobFinalUsable(finalPath) {
			return false, errCICacheBlobCorrupt
		}
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, errCICacheBlobIO
	}
	if _, err := os.Lstat(pendingPath); err == nil {
		return false, errCICacheBlobConflict
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, errCICacheBlobIO
	}
	pending, err := os.OpenFile(pendingPath,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, errCICacheBlobConflict
		}
		return false, errCICacheBlobIO
	}
	linked := false
	defer func() {
		if pending != nil {
			pending.Close()
		}
		if !linked {
			os.Remove(pendingPath)
		}
	}()
	stopSource := context.AfterFunc(opCtx, func() { body.Close() })
	defer stopSource()
	hasher := sha256.New()
	var written int64
	buf := make([]byte, 64*1024)
	failed := false
	for !failed {
		remaining := size + 1 - written
		if remaining <= 0 {
			failed = true
			break
		}
		want := int64(len(buf))
		if want > remaining {
			want = remaining
		}
		n, readErr := body.Read(buf[:int(want)])
		if n > 0 {
			// Never write past the declared size: the single extra byte is
			// read only to detect overflow, then the put fails.
			if int64(n) > size-written {
				failed = true
				break
			}
			wn, writeErr := pending.Write(buf[:n])
			if writeErr != nil || wn != n {
				failed = true
				break
			}
			hasher.Write(buf[:n])
			written += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			failed = true
			break
		}
	}
	if failed || written != size ||
		hex.EncodeToString(hasher.Sum(nil)) != digest ||
		opCtx.Err() != nil {
		return false, errCICacheBlobCorrupt
	}
	if err := pending.Sync(); err != nil {
		return false, errCICacheBlobIO
	}
	if err := pending.Chmod(0400); err != nil {
		return false, errCICacheBlobIO
	}
	// Seal metadata, then close the descriptor before publishing: no writable
	// or pending FD may survive into the committed object.
	if err := pending.Sync(); err != nil {
		return false, errCICacheBlobIO
	}
	if err := pending.Close(); err != nil {
		return false, errCICacheBlobIO
	}
	pending = nil
	if opCtx.Err() != nil {
		return false, errCICacheBlobIO
	}
	if err := os.Link(pendingPath, finalPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if !ciCacheBlobFinalUsable(finalPath) {
				return false, errCICacheBlobCorrupt
			}
			return false, nil
		}
		return false, errCICacheBlobIO
	}
	linked = true
	// Only report created once the pending name is gone, both directories are
	// synced and the sealed final verifies; a retained two-link pair is a
	// known crash state for the cleaner, never a silent success.
	if err := os.Remove(pendingPath); err != nil {
		return false, errCICacheBlobIO
	}
	if err := ciCacheBlobSyncDir(filepath.Dir(finalPath)); err != nil {
		return false, err
	}
	if err := ciCacheBlobSyncDir(filepath.Dir(pendingPath)); err != nil {
		return false, err
	}
	info, err := os.Lstat(finalPath)
	if err != nil || !ciCacheBlobStatOK(info, 0400, 1) {
		return false, errCICacheBlobCorrupt
	}
	return true, nil
}

func ciCacheBlobReadAllVerified(file *os.File, size int64, digest string) error {
	hasher := sha256.New()
	var total int64
	buf := make([]byte, 64*1024)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > size {
				return errCICacheBlobCorrupt
			}
			hasher.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return errCICacheBlobIO
		}
	}
	if total != size || hex.EncodeToString(hasher.Sum(nil)) != digest {
		return errCICacheBlobCorrupt
	}
	return nil
}

type ciCacheBlobReadCloser struct {
	*os.File
	once      sync.Once
	release   func()
	cancel    context.CancelFunc
	stopStore func() bool
	err       error
}

func (r *ciCacheBlobReadCloser) Close() error {
	r.once.Do(func() {
		if r.stopStore != nil {
			r.stopStore()
		}
		r.err = r.File.Close()
		r.cancel()
		r.release()
	})
	return r.err
}

func (s *ciCacheBlobStore) Open(ctx context.Context, id string,
	size int64, digest string) (io.ReadCloser, error) {
	if ctx == nil || !ciCacheBlobValidID(id) || !ciCacheBlobValidDigest(digest) ||
		!ciCacheBlobValidSize(size) {
		return nil, errCICacheBlobInvalid
	}
	release, err := s.acquire(id, false)
	if err != nil {
		return nil, err
	}
	opCtx, cancel := context.WithCancel(ctx)
	stopStore := context.AfterFunc(s.opCtx, cancel)
	if s.opCtx.Err() != nil {
		cancel()
	}
	fail := func(err error) (io.ReadCloser, error) {
		stopStore()
		cancel()
		release()
		return nil, err
	}
	finalPath, _ := s.objectPaths(id)
	file, err := os.OpenFile(finalPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fail(errCICacheBlobMissing)
		}
		return fail(errCICacheBlobIO)
	}
	// Cancellation must abort the integrity read too: closing the descriptor
	// makes an in-flight hash verification fail instead of running to EOF.
	stopFD := context.AfterFunc(opCtx, func() { file.Close() })
	info, err := file.Stat()
	if err != nil {
		stopFD()
		file.Close()
		return fail(errCICacheBlobIO)
	}
	if info.Size() != size || !ciCacheBlobStatOK(info, 0400, 1) {
		stopFD()
		file.Close()
		return fail(errCICacheBlobCorrupt)
	}
	if err := ciCacheBlobReadAllVerified(file, size, digest); err != nil {
		stopFD()
		file.Close()
		return fail(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		stopFD()
		file.Close()
		return fail(errCICacheBlobIO)
	}
	if opCtx.Err() != nil {
		stopFD()
		file.Close()
		return fail(errCICacheBlobIO)
	}
	stopFD()
	reader := &ciCacheBlobReadCloser{
		File:      file,
		release:   release,
		cancel:    cancel,
		stopStore: stopStore,
	}
	// The callback only closes the reader; once.Do makes a repeat harmless and
	// no callback-assigned field is ever read unsynchronized.
	context.AfterFunc(opCtx, func() { reader.Close() })
	if opCtx.Err() != nil {
		reader.Close()
		return nil, errCICacheBlobIO
	}
	return reader, nil
}

func ciCacheBlobEntryOK(info os.FileInfo, allowed ...os.FileMode) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() ||
		stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	for _, mode := range allowed {
		if ciCacheBlobPermExact(info.Mode(), mode) {
			return true
		}
	}
	return false
}

func (s *ciCacheBlobStore) Delete(ctx context.Context, id string) (bool, error) {
	if ctx == nil || !ciCacheBlobValidID(id) {
		return false, errCICacheBlobInvalid
	}
	release, err := s.acquire(id, true)
	if err != nil {
		return false, err
	}
	defer release()
	opCtx, _, cleanup := s.opContext(ctx)
	defer cleanup()
	if opCtx.Err() != nil {
		return false, errCICacheBlobIO
	}
	finalPath, pendingPath := s.objectPaths(id)
	finalInfo, finalErr := os.Lstat(finalPath)
	pendingInfo, pendingErr := os.Lstat(pendingPath)
	if finalErr != nil && !errors.Is(finalErr, fs.ErrNotExist) {
		return false, errCICacheBlobIO
	}
	if pendingErr != nil && !errors.Is(pendingErr, fs.ErrNotExist) {
		return false, errCICacheBlobIO
	}
	finalPresent := finalErr == nil
	pendingPresent := pendingErr == nil
	if finalPresent && !ciCacheBlobEntryOK(finalInfo, 0400) {
		return false, errCICacheBlobCorrupt
	}
	if pendingPresent && !ciCacheBlobEntryOK(pendingInfo, 0400, 0600) {
		return false, errCICacheBlobCorrupt
	}
	if finalPresent && pendingPresent {
		fstat := finalInfo.Sys().(*syscall.Stat_t)
		pstat := pendingInfo.Sys().(*syscall.Stat_t)
		if fstat.Dev == pstat.Dev && fstat.Ino == pstat.Ino {
			if fstat.Nlink != 2 || pstat.Nlink != 2 {
				return false, errCICacheBlobCorrupt
			}
		} else if fstat.Nlink != 1 || pstat.Nlink != 1 {
			return false, errCICacheBlobCorrupt
		}
	} else if finalPresent {
		if finalInfo.Sys().(*syscall.Stat_t).Nlink != 1 {
			return false, errCICacheBlobCorrupt
		}
	} else if pendingPresent {
		if pendingInfo.Sys().(*syscall.Stat_t).Nlink != 1 {
			return false, errCICacheBlobCorrupt
		}
	}
	if finalPresent {
		if err := os.Remove(finalPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, errCICacheBlobIO
		}
	}
	if pendingPresent {
		if err := os.Remove(pendingPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, errCICacheBlobIO
		}
	}
	if err := ciCacheBlobSyncDir(filepath.Dir(finalPath)); err != nil {
		return false, err
	}
	if err := ciCacheBlobSyncDir(filepath.Dir(pendingPath)); err != nil {
		return false, err
	}
	_, finalErr = os.Lstat(finalPath)
	_, pendingErr = os.Lstat(pendingPath)
	if errors.Is(finalErr, fs.ErrNotExist) && errors.Is(pendingErr, fs.ErrNotExist) {
		return true, nil
	}
	if finalErr != nil && !errors.Is(finalErr, fs.ErrNotExist) {
		return false, errCICacheBlobIO
	}
	if pendingErr != nil && !errors.Is(pendingErr, fs.ErrNotExist) {
		return false, errCICacheBlobIO
	}
	return false, fmt.Errorf("%w: CI cache blob object could not be removed", ErrConflict)
}
