// Package wrencomputer controls an isolated Wren guest through pinned SSH.
// Provider credentials and lifecycle ownership remain in the control plane.
package wrencomputer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const MaxFileBytes = 8 << 20
const maxResponseBytes = 12 << 20

// Config is private control-plane configuration, never a model observation.
type Config struct {
	// UserFiles marks this client as a public/user surface: read/write/list
	// calls are fenced to ordinary user files (excluding .canter/.browser),
	// while internal checkpoint clients keep the default private access.
	UserFiles     bool
	Address       string
	Port          int
	Username      string
	PrivateKeyPEM []byte
	HostKeySHA256 string
}
type Client struct {
	pointerProtocol atomic.Int32
	config          Config
	signer          ssh.Signer
	mu              sync.Mutex
	connection      *ssh.Client
	idle            *time.Timer
	active          int
	screenMu        sync.Mutex
	screen          *screenFeed
}

// Close releases this client's transport. Individual operations never close a
// sibling operation's SSH connection when their own context is cancelled.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle != nil {
		c.idle.Stop()
	}
	if c.connection != nil {
		c.connection.Close()
		c.connection = nil
	}
}

func (c *Client) connectionFor(ctx context.Context) (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle != nil {
		c.idle.Stop()
	}
	if c.connection == nil {
		connection, err := c.connect(ctx)
		if err != nil {
			return nil, err
		}
		c.connection = connection
	}
	c.active++
	return c.connection, nil
}
func (c *Client) releaseConnection(connection *ssh.Client, broken bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active--
	if broken && c.connection == connection {
		connection.Close()
		c.connection = nil
	}
	if c.active == 0 && c.connection != nil {
		c.idle = time.AfterFunc(30*time.Second, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.active == 0 && c.connection == connection {
				connection.Close()
				c.connection = nil
			}
		})
	}
}

type Capabilities struct {
	WorkflowProtocol int    `json:"workflowProtocol"`
	InputProtocol    int    `json:"inputProtocol"`
	Version          string `json:"version"`
	Workspace        string `json:"workspace"`
	Desktop          bool   `json:"desktop"`
	Browser          bool   `json:"browser"`
	PersistentFiles  bool   `json:"persistentFiles"`
	DurableJobs      bool   `json:"durableJobs"`
}
type JobRequest struct {
	ID             string            `json:"id"`
	Command        string            `json:"command"`
	Directory      string            `json:"directory,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	// Sandbox selects the sandbox job protocol (32MiB logs, 86400s max). It is
	// set only by the control plane, never accepted from untrusted input.
	Sandbox bool `json:"sandbox,omitempty"`
}
type JobLogChunk struct {
	JobID        string `json:"jobId"`
	Stream       string `json:"stream"`
	Data         []byte `json:"dataBase64"`
	Offset       int64  `json:"offset"`
	NextOffset   int64  `json:"nextOffset"`
	EOF          bool   `json:"eof"`
	Status       string `json:"status"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	LogTruncated bool   `json:"logTruncated"`
}
type FileUpload struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Offset   int64  `json:"offset"`
	SHA256   string `json:"sha256,omitempty"`
	Complete bool   `json:"complete"`
}
type ResourceMetrics struct {
	SampledAt     string  `json:"sampledAt"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
	CPU           struct {
		Count  int     `json:"count"`
		Load1  float64 `json:"load1"`
		Load5  float64 `json:"load5"`
		Load15 float64 `json:"load15"`
	} `json:"cpu"`
	Memory struct {
		TotalBytes     int64 `json:"totalBytes"`
		AvailableBytes int64 `json:"availableBytes"`
		UsedBytes      int64 `json:"usedBytes"`
	} `json:"memory"`
	Disk struct {
		TotalBytes     int64 `json:"totalBytes"`
		UsedBytes      int64 `json:"usedBytes"`
		AvailableBytes int64 `json:"availableBytes"`
	} `json:"disk"`
}
type Job struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	ExitCode        *int   `json:"exitCode,omitempty"`
	Output          string `json:"output"`
	OutputTruncated bool   `json:"outputTruncated"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
	Error           string `json:"error,omitempty"`
}
type FileChunk struct {
	Path       string `json:"path"`
	Data       []byte `json:"data"`
	Size       int64  `json:"size"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	EOF        bool   `json:"eof"`
	Version    string `json:"version,omitempty"`
}
type FileEntry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size"`
	Symlink   bool   `json:"symlink,omitempty"`
}
type DesktopInput struct {
	Kind   string `json:"kind"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	X2     int    `json:"x2"`
	Y2     int    `json:"y2"`
	Button int    `json:"button,omitempty"`
	Amount int    `json:"amount,omitempty"`
	Text   string `json:"text,omitempty"`
	Key    string `json:"key,omitempty"`
	URL    string `json:"url,omitempty"`
}
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return "computer: " + e.Message }

func NewSSH(config Config) (*Client, error) {
	if strings.TrimSpace(config.Address) == "" || strings.ContainsAny(config.Address, "/\r\n\x00") {
		return nil, fmt.Errorf("computer address is required")
	}
	if config.Port == 0 {
		config.Port = 22
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, fmt.Errorf("invalid computer SSH port")
	}
	if config.Username == "" {
		config.Username = "wren"
	}
	if config.Username != "wren" {
		return nil, fmt.Errorf("computer access must use the unprivileged wren account")
	}
	if !strings.HasPrefix(config.HostKeySHA256, "SHA256:") {
		return nil, fmt.Errorf("a verified computer SSH host fingerprint is required")
	}
	signer, err := ssh.ParsePrivateKey(config.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("invalid computer SSH key")
	}
	return &Client{config: config, signer: signer}, nil
}
func (c *Client) call(ctx context.Context, op string, input any, out any) error {
	return c.callWithTimeout(ctx, 45*time.Second, op, input, out)
}

func (c *Client) callWithTimeout(ctx context.Context, timeout time.Duration, op string, input any, out any) error {
	payload, err := json.Marshal(map[string]any{"operation": op, "input": input})
	if err != nil {
		return err
	}
	if len(payload) > maxResponseBytes {
		return fmt.Errorf("computer request is too large")
	}
	client, err := c.connectionFor(ctx)
	if err != nil {
		return err
	}
	broken := false
	defer func() { c.releaseConnection(client, broken) }()
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Channel setup can stall on a dead TCP peer. Bound it before registering
	// session-local cancellation; a transport that cannot open a channel is
	// unusable and must be retired, including its stalled sibling operations.
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
			broken = true
			return transportError(operationCtx, result.err)
		}
		session = result.session
	case <-openCtx.Done():
		broken = true
		client.Close()
		stopOpen()
		result := <-opened
		if result.session != nil {
			result.session.Close()
		}
		return transportError(operationCtx, openCtx.Err())
	}
	defer session.Close()
	stop := context.AfterFunc(operationCtx, func() { session.Close() })
	defer stop()
	session.Stdin = bytes.NewReader(payload)
	var stdout limitedBuffer
	stdout.limit = maxResponseBytes
	session.Stdout = &stdout
	session.Stderr = io.Discard
	if err = session.Run("python3 /opt/canter/computer-agent.py"); err != nil {
		return transportError(operationCtx, err)
	}
	if stdout.exceeded {
		return fmt.Errorf("computer response exceeded its size limit")
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err = json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		return fmt.Errorf("computer returned an invalid response")
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return envelope.Error
		}
		return fmt.Errorf("computer operation failed")
	}
	if out == nil {
		return nil
	}
	if err = json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("computer returned an invalid result")
	}
	return nil
}
func (c *Client) connect(ctx context.Context) (*ssh.Client, error) {
	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(c.config.Address, strconv.Itoa(c.config.Port)))
	if err != nil {
		return nil, transportError(ctx, err)
	}

	deadline := time.Now().Add(45 * time.Second)
	if when, ok := ctx.Deadline(); ok && when.Before(deadline) {
		deadline = when
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	// Bootstrap attests the Ed25519 key. Negotiate exactly that key type; a
	// normal multi-key sshd may otherwise select an unrelated ECDSA/RSA key.
	sshConfig := &ssh.ClientConfig{HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}, User: c.config.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(c.signer)}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if ssh.FingerprintSHA256(key) != c.config.HostKeySHA256 {
			return fmt.Errorf("computer host identity changed")
		}
		return nil
	}, Timeout: 15 * time.Second}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, c.config.Address, sshConfig)
	if err != nil {
		conn.Close()
		return nil, transportError(ctx, err)
	}
	conn.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConn, chans, reqs)

	return client, nil
}
func transportError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("computer SSH connection or command failed; inspect its state before retrying")
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	b.Buffer.Write(p)
	return n, nil
}
func (c *Client) Probe(ctx context.Context) (Capabilities, error) {
	var v Capabilities
	err := c.call(ctx, "probe", nil, &v)
	if err == nil {
		c.pointerProtocol.Store(int32(max(1, v.InputProtocol)))
	}
	return v, err
}
func (c *Client) StartJob(ctx context.Context, request JobRequest) (Job, error) {
	var v Job
	err := c.call(ctx, "start", request, &v)
	return v, err
}
func (c *Client) Job(ctx context.Context, id string) (Job, error) {
	var v Job
	err := c.call(ctx, "job", map[string]any{"id": id}, &v)
	return v, err
}
func (c *Client) StopJob(ctx context.Context, id string) (Job, error) {
	var v Job
	err := c.call(ctx, "stop", map[string]any{"id": id}, &v)
	return v, err
}
func (c *Client) ReadFile(ctx context.Context, path string, offset int64, limit int) (FileChunk, error) {
	var v FileChunk
	err := c.call(ctx, "read", map[string]any{"path": path, "offset": offset, "limit": limit, "userFiles": c.config.UserFiles}, &v)
	return v, err
}
func (c *Client) WriteFile(ctx context.Context, path string, data []byte) (FileChunk, error) {
	if len(data) > MaxFileBytes {
		return FileChunk{}, fmt.Errorf("computer file exceeds 8 MiB")
	}
	var v FileChunk
	err := c.call(ctx, "write", map[string]any{"path": path, "data": data, "userFiles": c.config.UserFiles}, &v)
	return v, err
}
func (c *Client) JobLogs(ctx context.Context, id, stream string, offset int64, limit int) (JobLogChunk, error) {
	var v JobLogChunk
	err := c.call(ctx, "job_logs", map[string]any{"id": id, "stream": stream, "offset": offset, "limit": limit}, &v)
	return v, err
}
func (c *Client) ReadFileVersion(ctx context.Context, path string, offset int64, limit int, version string) (FileChunk, error) {
	var v FileChunk
	input := map[string]any{"path": path, "offset": offset, "limit": limit, "userFiles": c.config.UserFiles}
	if version != "" {
		input["version"] = version
	}
	err := c.call(ctx, "read", input, &v)
	return v, err
}
func (c *Client) BeginUpload(ctx context.Context, id, path string, size int64, sha256 string) (FileUpload, error) {
	var v FileUpload
	input := map[string]any{"id": id, "path": path, "size": size}
	if sha256 != "" {
		input["sha256"] = sha256
	}
	err := c.call(ctx, "begin_upload", input, &v)
	return v, err
}
func (c *Client) UploadChunk(ctx context.Context, id string, offset int64, data []byte) (FileUpload, error) {
	var v FileUpload
	err := c.call(ctx, "upload_chunk", map[string]any{"id": id, "offset": offset, "data": data}, &v)
	return v, err
}
func (c *Client) CompleteUpload(ctx context.Context, id string) (FileUpload, error) {
	var v FileUpload
	err := c.callWithTimeout(ctx, 10*time.Minute, "complete_upload", map[string]any{"id": id}, &v)
	return v, err
}
func (c *Client) AbortUpload(ctx context.Context, id string) error {
	var v map[string]bool
	return c.call(ctx, "abort_upload", map[string]any{"id": id}, &v)
}
func (c *Client) Metrics(ctx context.Context) (ResourceMetrics, error) {
	var v ResourceMetrics
	err := c.call(ctx, "metrics", nil, &v)
	return v, err
}
func (c *Client) ListFiles(ctx context.Context, path string) ([]FileEntry, error) {
	var v []FileEntry
	err := c.call(ctx, "list", map[string]any{"path": path, "userFiles": c.config.UserFiles}, &v)
	return v, err
}
func (c *Client) Screenshot(ctx context.Context) ([]byte, error) {
	var v struct {
		Data []byte `json:"data"`
	}
	err := c.call(ctx, "screenshot", nil, &v)
	return v.Data, err
}
func (c *Client) Input(ctx context.Context, input DesktopInput) error {
	if input.Kind == "open_url" {
		input.Kind = "url"
	}
	if input.Kind == "right_click" {
		input.Kind = "click"
		input.Button = 3
	}
	pointer := input.Kind == "click" || input.Kind == "move" || input.Kind == "double_click" || input.Kind == "drag"
	if pointer {
		if input.X < 0 || input.Y < 0 || input.X > 4095 || input.Y > 4095 || (input.Kind == "drag" && (input.X2 < 0 || input.Y2 < 0 || input.X2 > 4095 || input.Y2 > 4095)) {
			return fmt.Errorf("invalid screen coordinates")
		}
		if input.Button == 0 {
			input.Button = 1
		}
		if input.Button < 1 || input.Button > 3 {
			return fmt.Errorf("invalid mouse button")
		}
		if c.pointerProtocol.Load() == 0 {
			if _, err := c.Probe(ctx); err != nil {
				return err
			}
		}
		if c.pointerProtocol.Load() < 2 {
			return c.legacyPointerInput(ctx, input)
		}
	}

	return c.call(ctx, "input", input, nil)
}

// Older Ubuntu xdotool waits forever when --sync targets the current pointer
// position. Existing root-owned helpers cannot be replaced through guest file
// tools. A bounded, numeric-only command repairs those guests without widening
// SSH access or reinstalling their desktop. New helpers use the direct RPC.
func (c *Client) legacyPointerInput(ctx context.Context, input DesktopInput) error {
	command := fmt.Sprintf("export DISPLAY=:99; xdotool mousemove %d %d", input.X, input.Y)
	switch input.Kind {
	case "click":
		command += fmt.Sprintf(" click %d", input.Button)
	case "double_click":
		command += " click --repeat 2 --delay 100 1"
	case "drag":
		command = fmt.Sprintf("export DISPLAY=:99; trap 'xdotool mouseup %d' EXIT; xdotool mousemove %d %d mousedown %d sleep 0.05 mousemove %d %d", input.Button, input.X, input.Y, input.Button, input.X2, input.Y2)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	job, err := c.StartJob(ctx, JobRequest{ID: fmt.Sprintf("input_%x", random), Command: command, TimeoutSeconds: 5})
	if err != nil {
		return err
	}
	for job.Status == "starting" || job.Status == "running" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(60 * time.Millisecond):
		}
		job, err = c.Job(ctx, job.ID)
		if err != nil {
			return err
		}
	}
	if job.Status != "completed" {
		return fmt.Errorf("computer pointer input did not complete")
	}
	return nil
}

// Terminal is a live pseudo-terminal on the guest. Output arrives on Read;
// input and resizes are framed JSON lines on stdin.
type Terminal struct {
	session    *ssh.Session
	connection *ssh.Client
	client     *Client
	stdin      io.WriteCloser
	stdout     io.Reader
	writeMu    sync.Mutex
	once       sync.Once
}

func (t *Terminal) Write(input []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	frame, err := json.Marshal(map[string]string{"t": "i", "d": base64.StdEncoding.EncodeToString(input)})
	if err != nil {
		return err
	}
	_, err = t.stdin.Write(append(frame, '\n'))
	return err
}

func (t *Terminal) Resize(cols, rows int) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	frame, err := json.Marshal(map[string]any{"t": "r", "cols": cols, "rows": rows})
	if err != nil {
		return err
	}
	_, err = t.stdin.Write(append(frame, '\n'))
	return err
}

// Read streams raw pty output until the shell exits or the session closes.
func (t *Terminal) Read(buffer []byte) (int, error) { return t.stdout.Read(buffer) }

func (t *Terminal) Close() {
	t.once.Do(func() {
		t.stdin.Close()
		t.session.Close()
		t.client.releaseConnection(t.connection, false)
	})
}

// openStream starts the guest agent on a dedicated session, writes the JSON
// header line for a streaming op, and returns the live pipes.
func (c *Client) openStream(ctx context.Context, header map[string]any) (*ssh.Session, io.WriteCloser, io.Reader, *ssh.Client, error) {
	connection, err := c.connectionFor(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	session, err := connection.NewSession()
	if err != nil {
		c.releaseConnection(connection, true)
		return nil, nil, nil, nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		c.releaseConnection(connection, true)
		return nil, nil, nil, nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		c.releaseConnection(connection, true)
		return nil, nil, nil, nil, err
	}
	if err = session.Start("python3 /opt/canter/computer-agent.py"); err != nil {
		session.Close()
		c.releaseConnection(connection, true)
		return nil, nil, nil, nil, err
	}
	line, err := json.Marshal(header)
	if err != nil {
		session.Close()
		c.releaseConnection(connection, true)
		return nil, nil, nil, nil, err
	}
	if _, err = stdin.Write(append(line, '\n')); err != nil {
		session.Close()
		c.releaseConnection(connection, true)
		return nil, nil, nil, nil, err
	}
	return session, stdin, stdout, connection, nil
}

// OpenTerminal opens a bash -l pty on the guest (sandboxes only).
func (c *Client) OpenTerminal(ctx context.Context, cols, rows int) (*Terminal, error) {
	session, stdin, stdout, connection, err := c.openStream(ctx, map[string]any{"operation": "pty", "input": map[string]int{"cols": cols, "rows": rows}})
	if err != nil {
		return nil, err
	}
	return &Terminal{session: session, connection: connection, client: c, stdin: stdin, stdout: stdout}, nil
}

// relayConn adapts a streaming guest session to net.Conn for tcp_relay.
type relayConn struct {
	stdin   io.WriteCloser
	stdout  io.Reader
	session *ssh.Session
	client  *Client
	conn    *ssh.Client
	once    sync.Once
}

func (r *relayConn) Read(b []byte) (int, error)  { return r.stdout.Read(b) }
func (r *relayConn) Write(b []byte) (int, error) { return r.stdin.Write(b) }
func (r *relayConn) Close() error {
	r.once.Do(func() {
		r.stdin.Close()
		r.session.Close()
		r.client.releaseConnection(r.conn, false)
	})
	return nil
}
func (r *relayConn) LocalAddr() net.Addr              { return relayAddr("guest") }
func (r *relayConn) RemoteAddr() net.Addr             { return relayAddr("sandbox") }
func (r *relayConn) SetDeadline(time.Time) error      { return nil }
func (r *relayConn) SetReadDeadline(time.Time) error  { return nil }
func (r *relayConn) SetWriteDeadline(time.Time) error { return nil }

type relayAddr string

func (a relayAddr) Network() string { return "ssh" }
func (a relayAddr) String() string  { return string(a) }

// DialPort opens a raw TCP relay to 127.0.0.1:port on the guest (sandboxes only).
func (c *Client) DialPort(ctx context.Context, port int) (net.Conn, error) {
	session, stdin, stdout, connection, err := c.openStream(ctx, map[string]any{"operation": "tcp_relay", "input": map[string]int{"port": port}})
	if err != nil {
		return nil, err
	}
	return &relayConn{stdin: stdin, stdout: stdout, session: session, client: c, conn: connection}, nil
}
