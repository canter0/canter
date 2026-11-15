package wrencomputer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	_ "embed"
	"golang.org/x/crypto/ssh"
)

//go:embed snapshot_stream.py
var sandboxSnapshotHelper []byte

// sandboxSnapshotCommand is the fixed root-owned agent command every private
// session runs; the forced authorized-key forces this entrypoint anyway.
func sandboxSnapshotCommand() string {
	return "/usr/bin/python3 -I /opt/canter/computer-agent.py"
}

// snapshotSessionTimeout bounds one export or restore. A shorter caller
// context still wins; source/destination implementations must honour ctx for
// their own blocking I/O — the transport cannot interrupt an arbitrary
// io.Reader or io.Writer that ignores it.
const snapshotSessionTimeout = 6 * time.Hour
const snapshotShutdownGrace = 8 * time.Second

// contextWriter refuses further writes once the operation is cancelled so
// cancelled work never hands more bytes to the caller's destination.
type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

// contextReader stops reads once the operation is cancelled; bytes already
// read past cancellation are discarded rather than delivered to the caller.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return n, err
}

// snapshotSessionResult carries the open session, its stdio, the operation
// context, and a finished channel the cancellation watcher keys on.
type snapshotSessionResult struct {
	client   *ssh.Client
	session  *ssh.Session
	stdout   io.Reader
	stdin    io.WriteCloser
	ctx      context.Context
	cancel   context.CancelFunc
	finished chan struct{}
}

// retireConnection removes a dead or unresponsive transport from the pool:
// it clears c.connection only when it still holds this client, stops the idle
// timer, and closes the client. It does not touch c.active — normal cleanup
// still decrements the operation exactly once.
func (c *Client) retireConnection(client *ssh.Client) {
	c.mu.Lock()
	if c.connection == client {
		if c.idle != nil {
			c.idle.Stop()
			c.idle = nil
		}
		c.connection = nil
	}
	c.mu.Unlock()
	client.Close()
}

// snapshotSession opens a bounded SSH session running the snapshot helper.
// Open and Start are each bounded to 8 seconds; a transport that cannot open
// a channel or start the command is retired, while ordinary cancellation
// closes only this session and leaves healthy siblings running.
func (c *Client) snapshotSession(ctx context.Context) (*snapshotSessionResult, error) {
	client, err := c.connectionFor(ctx)
	if err != nil {
		return nil, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, snapshotSessionTimeout)
	fail := func(broken bool, err error) (*snapshotSessionResult, error) {
		wrapped := transportError(operationCtx, err)
		c.releaseConnection(client, broken)
		cancel()
		return nil, wrapped
	}
	openCtx, stopOpen := context.WithTimeout(operationCtx, 8*time.Second)
	opened := make(chan struct {
		session *ssh.Session
		err     error
	}, 1)
	go func() {
		session, err := client.NewSession()
		opened <- struct {
			session *ssh.Session
			err     error
		}{session, err}
	}()
	var session *ssh.Session
	select {
	case result := <-opened:
		stopOpen()
		if result.err != nil {
			return fail(true, result.err)
		}
		session = result.session
	case <-openCtx.Done():
		cause := openCtx.Err()
		stopOpen()
		if operationCtx.Err() != nil {
			// User cancellation: don't retire a healthy transport. Allow the
			// pending open a bounded grace to produce a session we close.
			select {
			case result := <-opened:
				if result.session != nil {
					result.session.Close()
				}
			case <-time.After(snapshotShutdownGrace):
				c.retireConnection(client)
			}
			return fail(false, cause)
		}
		// Genuine setup timeout while the operation is live: transport is
		// unresponsive — retire it, then collect the late result.
		c.retireConnection(client)
		result := <-opened
		if result.session != nil {
			result.session.Close()
		}
		return fail(true, cause)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return fail(true, err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return fail(true, err)
	}
	var stderr limitedBuffer
	stderr.limit = 4096
	session.Stderr = &stderr
	startCtx, stopStart := context.WithTimeout(operationCtx, 8*time.Second)
	started := make(chan error, 1)
	go func() { started <- session.Start(sandboxSnapshotCommand()) }()
	select {
	case err := <-started:
		stopStart()
		if err != nil {
			session.Close()
			return fail(true, err)
		}
	case <-startCtx.Done():
		cause := startCtx.Err()
		stopStart()
		session.Close()
		if operationCtx.Err() != nil {
			// User cancellation mid-Start: grace the started goroutine before
			// retiring, so a healthy transport keeps its siblings.
			select {
			case <-started:
			case <-time.After(snapshotShutdownGrace):
				c.retireConnection(client)
			}
			return fail(false, cause)
		}
		// Handshake timeout with a live operation: retire before collecting.
		c.retireConnection(client)
		<-started
		return fail(true, cause)
	}
	result := &snapshotSessionResult{
		client: client, session: session, stdout: stdout, stdin: stdin,
		ctx: operationCtx, cancel: cancel, finished: make(chan struct{}),
	}
	// Cancellation watcher: closing our own session may not unblock a stalled
	// peer — retire the whole connection only if it still hasn't finished
	// after the grace window, preserving concurrently active siblings.
	go func() {
		select {
		case <-operationCtx.Done():
			session.Close()
			select {
			case <-result.finished:
				return
			case <-time.After(snapshotShutdownGrace):
			}
			select {
			case <-result.finished:
				return // finished during grace — leave the pooled transport
			default:
				c.retireConnection(client)
			}
		case <-result.finished:
		}
	}()
	return result, nil
}

// sandboxSnapshotRPC envelopes the private snapshot request for the guest's
// forced-command dispatcher; the inner snapshotRequest feeds the helper.
type sandboxSnapshotRPC struct {
	Operation string          `json:"operation"`
	Input     snapshotRequest `json:"input"`
}

// snapshotRequest is the single JSON line fed to the helper's stdin.
type snapshotRequest struct {
	Operation string                 `json:"operation"`
	Options   SandboxSnapshotOptions `json:"options"`
	Expected  *SandboxSnapshotStats  `json:"expected,omitempty"`
}

// ExportSandboxSnapshot streams a bounded framed archive out of the sandbox's
// workspace+home roots. The remote receipt and a clean SSH exit are both
// required before the operation reports success; the archive body is never
// buffered in memory. Private clients only — a public user-file client cannot
// read the protected paths this export covers.
func (c *Client) ExportSandboxSnapshot(ctx context.Context, options SandboxSnapshotOptions, destination io.Writer) (SandboxSnapshotStats, error) {
	var empty SandboxSnapshotStats
	if c.config.UserFiles {
		return empty, fmt.Errorf("sandbox snapshot transport requires a private client")
	}
	if err := options.validate(); err != nil {
		return empty, err
	}
	conn, err := c.snapshotSession(ctx)
	if err != nil {
		return empty, err
	}
	session := conn.session
	defer func() {
		close(conn.finished)
		session.Close()
		c.releaseConnection(conn.client, false)
		conn.cancel()
	}()
	request, err := json.Marshal(sandboxSnapshotRPC{Operation: "sandbox_snapshot", Input: snapshotRequest{Operation: "export", Options: options}})
	if err != nil {
		return empty, err
	}
	if _, err := io.WriteString(conn.stdin, string(request)+"\n"); err != nil {
		session.Close()
		return empty, err
	}
	if err := conn.stdin.Close(); err != nil {
		session.Close()
		return empty, err
	}
	frames, err := newSnapshotFrameReader(contextReader{ctx: conn.ctx, r: conn.stdout}, options)
	if err != nil {
		session.Close()
		return empty, err
	}
	stats, validateErr := validateSnapshotStream(frames, contextWriter{ctx: conn.ctx, w: destination})
	if validateErr != nil {
		// A failed destination or validator can leave the producer blocked on
		// stdout — cancel + close our session BEFORE waiting, then return the
		// original error. The watcher retires the transport if the peer does
		// not shut down inside the grace window.
		conn.cancel()
		session.Close()
		_ = session.Wait()
		return empty, validateErr
	}
	if err := session.Wait(); err != nil {
		return empty, fmt.Errorf("snapshot export failed remotely: %w", err)
	}
	if err := conn.ctx.Err(); err != nil {
		return empty, err
	}
	return stats, nil
}

// RestoreSandboxSnapshot validates the archive while streaming it into a
// fresh, unexposed target and requires the remote receipt to match the
// expected export. The caller MUST point restore at a new allocation — after
// any failure the partially restored target must be discarded; production
// orchestration never restores into an active user workspace.
func (c *Client) RestoreSandboxSnapshot(ctx context.Context, options SandboxSnapshotOptions, expected SandboxSnapshotStats, source io.Reader) error {
	if c.config.UserFiles {
		return fmt.Errorf("sandbox snapshot transport requires a private client")
	}
	if err := options.validate(); err != nil {
		return err
	}
	// Validate the expected receipt before opening the remote session: invalid
	// expected metadata must never trigger the remote's destructive clearing.
	raw, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	parsed, err := snapshotReceipt(bytes.NewReader(raw), uint32(len(raw)), options)
	if err != nil {
		return err
	}
	if !sameSnapshotStats(parsed, expected) {
		return fmt.Errorf("expected snapshot receipt does not match the export")
	}
	conn, err := c.snapshotSession(ctx)
	if err != nil {
		return err
	}
	session := conn.session
	defer func() {
		close(conn.finished)
		session.Close()
		c.releaseConnection(conn.client, false)
		conn.cancel()
	}()
	request, err := json.Marshal(sandboxSnapshotRPC{Operation: "sandbox_snapshot", Input: snapshotRequest{Operation: "restore", Options: options, Expected: &expected}})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(conn.stdin, string(request)+"\n"); err != nil {
		session.Close()
		return err
	}
	// Validate while streaming: TeeReader feeds ValidateSandboxArchive the same
	// bytes the remote receives, so a malformed archive is caught mid-send.
	stats, validateErr := ValidateSandboxArchive(io.TeeReader(contextReader{ctx: conn.ctx, r: source}, contextWriter{ctx: conn.ctx, w: conn.stdin}), options)
	stats.RuntimeSkipped = expected.RuntimeSkipped // archive cannot infer omissions
	closeErr := conn.stdin.Close()                 // EOF on all paths, even error
	if validateErr != nil {
		conn.cancel()
		session.Close()
		_ = session.Wait()
		return validateErr
	}
	if closeErr != nil {
		session.Close()
		return closeErr
	}
	if !sameSnapshotStats(stats, expected) {
		session.Close()
		return fmt.Errorf("archive statistics do not match the expected receipt")
	}
	if err := readSnapshotRestoreReceipt(conn.stdout, options, expected); err != nil {
		session.Close()
		return err
	}
	if err := session.Wait(); err != nil {
		return fmt.Errorf("snapshot restore failed remotely: %w", err)
	}
	return conn.ctx.Err()
}
