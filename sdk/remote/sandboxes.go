package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	pathpkg "path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SandboxClient is a workspace-scoped REST client for Canter sandboxes. It
// wraps the shared Client transport: the base URL is the service origin and
// routes include /v1. Every method takes the caller's context; mutations are
// never retried and the access token is only ever sent to the configured
// origin (redirects are never followed by Client.New).
type SandboxClient struct {
	client      *Client
	workspaceID string // already path-escaped
	accessToken string
}

var sandboxIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var sandboxTokenPattern = regexp.MustCompile(`^[\x21-\x7e]+$`)

// Sandboxes returns a workspace-scoped sandbox client. The access token must
// be a non-empty caller token or installation credential; it is validated but
// never persisted or echoed into errors.
func (c *Client) Sandboxes(workspaceID, accessToken string) (*SandboxClient, error) {
	if !sandboxTokenPattern.MatchString(accessToken) {
		return nil, errors.New("a nonempty printable Canter access token without whitespace is required")
	}
	if !sandboxIDPattern.MatchString(workspaceID) {
		return nil, fmt.Errorf("invalid workspace ID for Canter sandbox client")
	}
	escaped, err := escapePathID("workspace ID", workspaceID)
	if err != nil {
		return nil, err
	}
	return &SandboxClient{client: c, workspaceID: escaped, accessToken: accessToken}, nil
}

// Sandbox mirrors the public sandbox JSON view: identifiers, sizes, phase,
// generation and labels — never plan internals, keys or guest addresses.
type Sandbox struct {
	ID                 string              `json:"id"`
	WorkspaceID        string              `json:"workspaceId"`
	Name               string              `json:"name"`
	Size               string              `json:"size"`
	Tier               string              `json:"tier"`
	Phase              string              `json:"phase"`
	VCPUs              int                 `json:"vcpus"`
	MemoryMiB          int                 `json:"memoryMiB"`
	DiskGiB            int                 `json:"diskGiB"`
	MonthlyCents       int64               `json:"monthlyCents"`
	Failure            string              `json:"failure,omitempty"`
	DeleteRequested    bool                `json:"deleteRequested"`
	CreatedByKind      string              `json:"createdByKind"`
	CreatedByID        string              `json:"createdById,omitempty"`
	CreatedByName      string              `json:"createdByName,omitempty"`
	LastUsedAt         *time.Time          `json:"lastUsedAt,omitempty"`
	AwakeSince         *time.Time          `json:"awakeSince,omitempty"`
	HasCheckpoint      bool                `json:"hasCheckpoint"`
	CreatedAt          time.Time           `json:"createdAt"`
	UpdatedAt          time.Time           `json:"updatedAt"`
	Generation         int                 `json:"generation"`
	Notice             string              `json:"notice,omitempty"`
	Labels             map[string]string   `json:"labels"`
	IdleTimeoutSeconds int                 `json:"idleTimeoutSeconds"`
	SnapshotID         string              `json:"snapshotId,omitempty"`
	Desktop            bool                `json:"desktop"`
	Environment        *SandboxEnvironment `json:"environment,omitempty"`
	Setup              SandboxSetup        `json:"setup"`
}

// SandboxCreateInput is the create body; IdempotencyKey is required and must
// be a caller-provided 8-128 printable ASCII key.
type SandboxCreateInput struct {
	Name               string              `json:"name"`
	Size               string              `json:"size"`
	Labels             map[string]string   `json:"labels,omitempty"`
	IdleTimeoutSeconds *int                `json:"idleTimeoutSeconds,omitempty"`
	IdempotencyKey     string              `json:"idempotencyKey"`
	SnapshotID         string              `json:"snapshotId,omitempty"`
	TemplateID         string              `json:"templateId,omitempty"`
	TemplateVersion    int                 `json:"templateVersion,omitempty"`
	Desktop            bool                `json:"desktop,omitempty"`
	Environment        *SandboxEnvironment `json:"environment,omitempty"`
}

var sandboxIdempotencyKey = regexp.MustCompile(`^[\x20-\x7e]{8,128}$`)

// SandboxExecInput starts an async job on an awake sandbox.
type SandboxExecInput struct {
	Command   string            `json:"command"`
	Directory string            `json:"directory,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	// SecretRefs intentionally has no omitempty: nil marshals null and
	// inherits the sandbox recipe; a nonnil empty map marshals {} and clears
	// saved references.
	SecretRefs     map[string]string `json:"secretRefs"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
}

// SandboxRepository is a public HTTPS repository checked out at setup.
type SandboxRepository struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

// SandboxEnvironment is the immutable creation recipe.
type SandboxEnvironment struct {
	Repository          *SandboxRepository `json:"repository,omitempty"`
	SetupCommand        string             `json:"setupCommand,omitempty"`
	SetupTimeoutSeconds int                `json:"setupTimeoutSeconds,omitempty"`
	Env                 map[string]string  `json:"env,omitempty"`
	SecretRefs          map[string]string  `json:"secretRefs,omitempty"`
}

// SandboxSetup is the durable setup state on a sandbox.
type SandboxSetup struct {
	Status    string     `json:"status,omitempty"`
	JobID     string     `json:"jobId,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// SandboxJob is the public job receipt — jobId, status, exitCode, error and
// logTruncated; it never carries request fields or environment.
type SandboxJob struct {
	JobID        string    `json:"jobId"`
	Status       string    `json:"status"`
	UpdatedAt    time.Time `json:"updatedAt"`
	ExitCode     *int      `json:"exitCode,omitempty"`
	Error        string    `json:"error,omitempty"`
	LogTruncated bool      `json:"logTruncated,omitempty"`
}

// SandboxLogEvent is one emitted SSE frame from the job event feed: an
// "output" event carries decoded bytes with its byte offsets; a "status"
// event carries the job view and terminal exit code.
type SandboxLogEvent struct {
	Event        string
	JobID        string
	Stream       string
	Status       string
	Offset       int64
	NextOffset   int64
	Data         []byte
	ExitCode     *int
	LogTruncated bool
}

// SandboxUpload is the upload receipt across begin/chunk/complete calls.
type SandboxUpload struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Offset   int64  `json:"offset"`
	SHA256   string `json:"sha256,omitempty"`
	Complete bool   `json:"complete"`
}

// SandboxSnapshotStats mirrors the stored snapshot statistics.
type SandboxSnapshotStats struct {
	Format          string `json:"format"`
	IncludeBrowser  bool   `json:"includeBrowser"`
	RawBytes        int64  `json:"rawBytes"`
	Entries         int    `json:"entries"`
	CompressedBytes int64  `json:"compressedBytes"`
	ArchiveSHA256   string `json:"archiveSha256"`
	RuntimeSkipped  int    `json:"runtimeSkipped"`
}

// SandboxSnapshot is a ready saved environment.
type SandboxSnapshot struct {
	ID               string               `json:"id"`
	WorkspaceID      string               `json:"workspaceId"`
	SourceSandboxID  string               `json:"sourceSandboxId"`
	SourceGeneration int                  `json:"sourceGeneration"`
	SourceSize       string               `json:"sourceSize"`
	SourceDiskGiB    int                  `json:"sourceDiskGiB"`
	Name             string               `json:"name"`
	State            string               `json:"state"`
	Retained         bool                 `json:"retained"`
	Stats            SandboxSnapshotStats `json:"stats"`
	StoredBytes      int64                `json:"storedBytes"`
	Failure          string               `json:"failure,omitempty"`
	CreatedAt        time.Time            `json:"createdAt"`
	ReadyAt          *time.Time           `json:"readyAt,omitempty"`
}

// SandboxSnapshotPage is one ready-snapshot page with a before cursor.
type SandboxSnapshotPage struct {
	Snapshots      []SandboxSnapshot `json:"snapshots"`
	NextCursor     string            `json:"nextCursor,omitempty"`
	HistoryEnabled bool              `json:"historyEnabled"`
}

// SandboxTemplateVersion pins a snapshot as a template version.
type SandboxTemplateVersion struct {
	Version       int       `json:"version"`
	SnapshotID    string    `json:"snapshotId"`
	SourceSize    string    `json:"sourceSize"`
	SourceDiskGiB int       `json:"sourceDiskGiB"`
	StoredBytes   int64     `json:"storedBytes"`
	CreatedAt     time.Time `json:"createdAt"`
}

// SandboxTemplate is a named, versioned starting environment.
type SandboxTemplate struct {
	ID          string                   `json:"id"`
	WorkspaceID string                   `json:"workspaceId"`
	Name        string                   `json:"name"`
	CreatedAt   time.Time                `json:"createdAt"`
	UpdatedAt   time.Time                `json:"updatedAt"`
	Latest      *SandboxTemplateVersion  `json:"latest,omitempty"`
	Versions    []SandboxTemplateVersion `json:"versions,omitempty"`
}

// SandboxTemplateList is the template listing response.
type SandboxTemplateList struct {
	Templates      []SandboxTemplate `json:"templates"`
	HistoryEnabled bool              `json:"historyEnabled"`
}

// SandboxList is the sandbox listing response.
type SandboxList struct {
	Sandboxes       []Sandbox        `json:"sandboxes"`
	Enabled         bool             `json:"enabled"`
	PreviewsEnabled bool             `json:"previewsEnabled"`
	FreeUsage       SandboxFreeUsage `json:"freeUsage"`
	Sizes           []SandboxSize    `json:"sizes"`
	Limits          SandboxLimits    `json:"limits"`
}

// SandboxFreeUsage reports the workspace's free-tier consumption.
type SandboxFreeUsage struct {
	UsedSeconds  int64 `json:"usedSeconds"`
	LimitSeconds int64 `json:"limitSeconds"`
}

// SandboxSize describes a selectable sandbox size.
type SandboxSize struct {
	ID           string `json:"id"`
	VCPUs        int    `json:"vcpus"`
	MemoryMiB    int    `json:"memoryMiB"`
	DiskGiB      int    `json:"diskGiB"`
	Free         bool   `json:"free"`
	MonthlyCents int64  `json:"monthlyCents"`
}

// SandboxLimits reports the server's published operation limits.
type SandboxLimits struct {
	MaxCommandSeconds  int   `json:"maxCommandSeconds"`
	MaxFileBytes       int64 `json:"maxFileBytes"`
	UploadChunkBytes   int64 `json:"uploadChunkBytes"`
	DefaultIdleSeconds int   `json:"defaultIdleSeconds"`
	MaxJobLogBytes     int64 `json:"maxJobLogBytes"`
}

func (s *SandboxClient) base() string {
	return "/v1/workspaces/" + s.workspaceID
}

func sandboxSegment(label, id string) (string, error) {
	if !sandboxIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid %s for Canter sandbox path", label)
	}
	return url.PathEscape(id), nil
}

func (s *SandboxClient) sandboxPath(id, suffix string) (string, error) {
	escaped, err := sandboxSegment("sandbox ID", id)
	if err != nil {
		return "", err
	}
	return s.base() + "/sandboxes/" + escaped + suffix, nil
}

// List returns the workspace sandbox listing.
func (s *SandboxClient) List(ctx context.Context) (SandboxList, error) {
	var result SandboxList
	err := s.client.do(ctx, http.MethodGet, s.base()+"/sandboxes", s.accessToken, nil, &result)
	if err == nil {
		for _, item := range result.Sandboxes {
			if item.ID == "" || item.Phase == "" {
				return result, errors.New("invalid sandbox list receipt")
			}
		}
	}
	return result, err
}

// Create creates a sandbox. The caller must supply a stable idempotency key —
// the same frozen body is replayed by the server for the same key, a changed
// body conflicts. Mutations are never retried by the transport.
func (s *SandboxClient) Create(ctx context.Context, in SandboxCreateInput) (Sandbox, error) {
	var result Sandbox
	if !sandboxIdempotencyKey.MatchString(in.IdempotencyKey) {
		return result, errors.New("sandbox create requires an idempotency key of 8-128 printable ASCII characters")
	}
	err := s.client.do(ctx, http.MethodPost, s.base()+"/sandboxes", s.accessToken, in, &result)
	if err == nil && (result.ID == "" || result.Phase == "") {
		return result, errors.New("invalid sandbox create receipt")
	}
	return result, err
}

// Get returns one sandbox.
func (s *SandboxClient) Get(ctx context.Context, id string) (Sandbox, error) {
	var result Sandbox
	path, err := s.sandboxPath(id, "")
	if err != nil {
		return result, err
	}
	err = s.client.do(ctx, http.MethodGet, path, s.accessToken, nil, &result)
	if err == nil && result.ID != id {
		return result, errors.New("sandbox receipt ID does not match request")
	}
	return result, err
}

// sandboxWaitPhases are the only phases a caller may wait for; "deleting" is
// an acceptable intermediate state only when waiting for "deleted".
func sandboxWaitPhases(phase string) bool {
	return phase == "ready" || phase == "asleep" || phase == "deleted"
}

// Wait polls Get until the sandbox reaches the wanted phase, a terminal
// failure phase, or the caller's deadline (15 minutes by default).
// Callers waiting for "asleep" should tolerate the
// intermediate "sleeping" phase. An unrecognized or missing phase is a
// protocol error, never an endless wait.
func (s *SandboxClient) Wait(ctx context.Context, id, phase string) (Sandbox, error) {
	if !sandboxWaitPhases(phase) {
		return Sandbox{}, fmt.Errorf("invalid wait phase %q; expected ready, asleep, or deleted", phase)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
	}
	last := Sandbox{ID: id}
	waitError := func(err error) (Sandbox, error) {
		return last, fmt.Errorf("waiting for sandbox %s phase %q: %w (last phase %q, setup %q, job %q)", id, phase, err, last.Phase, last.Setup.Status, last.Setup.JobID)
	}
	for {
		v, err := s.Get(ctx, id)
		if err != nil {
			return waitError(err)
		}
		last = v
		if v.ID != id {
			return v, errors.New("sandbox receipt ID does not match request")
		}
		if v.Setup.Status != "" && v.Setup.Status != "succeeded" && v.Setup.Status != "pending" && v.Setup.Status != "running" && v.Setup.Status != "failed" {
			return v, fmt.Errorf("sandbox %s reported unrecognized setup status %q", id, v.Setup.Status)
		}
		if phase == "ready" && v.Setup.Status == "failed" {
			return v, fmt.Errorf("sandbox %s setup failed (job %s): %s", id, v.Setup.JobID, v.Setup.Error)
		}
		if v.Phase == phase && !(phase == "ready" && sandboxSetupPendingSDK(v.Setup)) {
			return v, nil
		}
		switch v.Phase {
		case "failed", "save_failed":
			return v, fmt.Errorf("sandbox %s reached terminal phase %q while waiting for %q: %s", id, v.Phase, phase, v.Failure)
		case "deleted", "deleting":
			if phase != "deleted" {
				return v, fmt.Errorf("sandbox %s reached terminal phase %q while waiting for %q", id, v.Phase, phase)
			}
		case "queued", "provisioning", "waking", "ready", "sleeping", "asleep":
			// Recognized non-terminal states — keep waiting.
		default:
			return v, fmt.Errorf("sandbox %s reported unrecognized phase %q", id, v.Phase)
		}
		select {
		case <-ctx.Done():
			return waitError(ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// StartJob begins an async command job; it is a mutation and is never
// retried.
func (s *SandboxClient) StartJob(ctx context.Context, id string, in SandboxExecInput) (SandboxJob, error) {
	var result SandboxJob
	path, err := s.sandboxPath(id, "/jobs")
	if err != nil {
		return result, err
	}
	err = s.client.do(ctx, http.MethodPost, path, s.accessToken, in, &result)
	if err == nil && (result.JobID == "" || result.Status == "" || !sandboxJobStatuses[result.Status]) {
		return result, errors.New("invalid sandbox job receipt")
	}
	return result, err
}

// Job returns the current job receipt.
func (s *SandboxClient) Job(ctx context.Context, id, jobID string) (SandboxJob, error) {
	var result SandboxJob
	segment, err := sandboxSegment("job ID", jobID)
	if err != nil {
		return result, err
	}
	path, err := s.sandboxPath(id, "/jobs/"+segment)
	if err != nil {
		return result, err
	}
	err = s.client.do(ctx, http.MethodGet, path, s.accessToken, nil, &result)
	if err == nil && (result.JobID != jobID || !sandboxJobStatuses[result.Status]) {
		return result, errors.New("invalid sandbox job receipt")
	}
	return result, err
}

// StopJob requests job cancellation and returns the current receipt.
func (s *SandboxClient) StopJob(ctx context.Context, id, jobID string) (SandboxJob, error) {
	var result SandboxJob
	segment, err := sandboxSegment("job ID", jobID)
	if err != nil {
		return result, err
	}
	path, err := s.sandboxPath(id, "/jobs/"+segment+"/stop")
	if err != nil {
		return result, err
	}
	err = s.client.do(ctx, http.MethodPost, path, s.accessToken, struct{}{}, &result)
	if err == nil && (result.JobID != jobID || !sandboxJobStatuses[result.Status]) {
		return result, errors.New("invalid sandbox job receipt")
	}
	return result, err
}

// streamHTTPClient returns a client clone whose default 20s timeout is raised
// for the streaming SSE feed; the caller context still governs the overall
// bound.
func (s *SandboxClient) streamHTTPClient() *http.Client {
	clone := *s.client.http
	if clone.Timeout < 35*time.Second {
		clone.Timeout = 35 * time.Second
	}
	return &clone
}

// sseFrame is one parsed SSE frame; Heartbeat marks comment-only frames.
type sseFrame struct {
	Event     string
	Data      string
	Heartbeat bool
}

// readSSEFrames reads complete SSE frames from r honoring ctx: a bounded
// scanner carries partial lines and CRLF; the raw byte size of the current
// frame — comments and partial lines included — is bounded at 1MiB; an
// unterminated partial frame at EOF is dropped.
func readSSEFrames(ctx context.Context, r io.Reader, onFrame func(sseFrame) bool) error {
	const maxFrame = 1 << 20
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxFrame+1)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0x0a); i >= 0 {
			return i + 1, data[:i+1], nil
		}
		if len(data) > maxFrame {
			return 0, nil, sandboxProtocolError{errors.New("sandbox job event frame exceeds 1MiB")}
		}
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
	var event string
	var data strings.Builder
	haveData, frameSize := false, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !scanner.Scan() {
			break
		}
		raw := scanner.Bytes()
		frameSize += len(raw)
		if frameSize > maxFrame {
			return sandboxProtocolError{errors.New("sandbox job event frame exceeds 1MiB")}
		}
		line := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		switch {
		case line == "":
			if haveData || event != "" {
				if !onFrame(sseFrame{Event: event, Data: data.String()}) {
					return nil
				}
			}
			event, haveData, frameSize = "", false, 0
			data.Reset()
		case strings.HasPrefix(line, ":"):
			if !onFrame(sseFrame{Heartbeat: true}) {
				return nil
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if haveData {
				data.WriteByte(0x0a)
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			haveData = true
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return sandboxProtocolError{errors.New("sandbox job event frame exceeds 1MiB")}
		}
		return err
	}
	return nil
}

// jobLogChunk is the on-wire output frame payload (dataBase64 JSON field).
type jobLogChunk struct {
	JobID        string `json:"jobId"`
	Stream       string `json:"stream"`
	DataBase64   string `json:"dataBase64"`
	Offset       int64  `json:"offset"`
	NextOffset   int64  `json:"nextOffset"`
	EOF          bool   `json:"eof"`
	Status       string `json:"status"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	LogTruncated bool   `json:"logTruncated"`
}

const maxLogChunkData = 262144

// sandboxProtocolError marks a fatal protocol/receipt error — never retried.
type sandboxProtocolError struct{ err error }

func (e sandboxProtocolError) Error() string { return e.err.Error() }
func (e sandboxProtocolError) Unwrap() error { return e.err }

// sandboxJobStatusActive reports whether a recognized guest job status is
// still running.
func sandboxJobStatusActive(status string) bool {
	return status == "starting" || status == "running"
}

var sandboxJobStatuses = map[string]bool{
	"starting": true, "running": true, "completed": true, "failed": true,
	"cancelled": true, "timed_out": true, "interrupted": true,
}

// StreamLogs follows the job SSE feed, invoking onEvent for each output and
// status frame. It reconnects at the exact delivered offset after a normal
// feed end, a transport failure, or 429/5xx — at most three consecutive
// attempts without progress — and stops immediately on 4xx auth or protocol
// errors. The feed ends when a terminal status arrives after all delivered
// output. Cancelling ctx closes the response body.
func (s *SandboxClient) StreamLogs(ctx context.Context, id, jobID, stream string, offset int64, onEvent func(SandboxLogEvent) error) error {
	if onEvent == nil {
		return errors.New("a sandbox log event handler is required")
	}
	if stream == "" {
		stream = "combined"
	}
	if stream != "combined" && stream != "stdout" && stream != "stderr" {
		return fmt.Errorf("invalid sandbox log stream %q", stream)
	}
	if offset < 0 {
		return fmt.Errorf("invalid sandbox log offset %d", offset)
	}
	segment, err := sandboxSegment("job ID", jobID)
	if err != nil {
		return err
	}
	path, err := s.sandboxPath(id, "/jobs/"+segment+"/events")
	if err != nil {
		return err
	}
	httpClient := s.streamHTTPClient()
	delivered := offset
	noProgress := 0
	lastStatus := ""
	backoff := func() error {
		delay := 200 * time.Millisecond * time.Duration(noProgress)
		if delay > 600*time.Millisecond {
			delay = 600 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	for {
		query := url.Values{}
		query.Set("stream", stream)
		query.Set("offset", strconv.FormatInt(delivered, 10))
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.client.baseURL+path+"?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		request.Header.Set("Accept", "text/event-stream")
		request.Header.Set("Authorization", "Bearer "+s.accessToken)
		feedStart := time.Now()
		response, err := httpClient.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			noProgress++
			if noProgress > 3 {
				return fmt.Errorf("sandbox log stream transport failed: %w", err)
			}
			if backoffErr := backoff(); backoffErr != nil {
				return backoffErr
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			raw, _ := readResponse(response.Body)
			response.Body.Close()
			status := response.StatusCode
			if status == http.StatusTooManyRequests || status >= 500 {
				noProgress++
				if noProgress > 3 {
					return decodeResponse(status, raw, nil)
				}
				if backoffErr := backoff(); backoffErr != nil {
					return backoffErr
				}
				continue
			}
			return decodeResponse(status, raw, nil)
		}
		terminal := false
		madeProgress := false
		sawHeartbeat := false
		var streamErr error
		readErr := readSSEFrames(ctx, response.Body, func(frame sseFrame) bool {
			if frame.Heartbeat {
				sawHeartbeat = true
				return true
			}
			switch frame.Event {
			case "output":
				var chunk jobLogChunk
				if err := json.Unmarshal([]byte(frame.Data), &chunk); err != nil {
					streamErr = sandboxProtocolError{fmt.Errorf("invalid sandbox log output frame: %w", err)}
					return false
				}
				if chunk.DataBase64 == "" {
					streamErr = sandboxProtocolError{errors.New("invalid sandbox log output receipt: empty data")}
					return false
				}
				data, err := base64.StdEncoding.DecodeString(chunk.DataBase64)
				if err != nil {
					streamErr = sandboxProtocolError{fmt.Errorf("invalid sandbox log output encoding: %w", err)}
					return false
				}
				if len(data) > maxLogChunkData || chunk.JobID != jobID || chunk.Stream != stream ||
					chunk.Offset != delivered || chunk.NextOffset < chunk.Offset ||
					chunk.NextOffset-chunk.Offset != int64(len(data)) || chunk.Status == "" || !sandboxJobStatuses[chunk.Status] {
					streamErr = sandboxProtocolError{errors.New("invalid sandbox log output receipt")}
					return false
				}
				// Deliver before advancing — a later bad frame must not drop
				// already-valid output.
				event := SandboxLogEvent{
					Event: "output", JobID: chunk.JobID, Stream: chunk.Stream,
					Status: chunk.Status, Offset: chunk.Offset, NextOffset: chunk.NextOffset,
					Data: data, ExitCode: chunk.ExitCode, LogTruncated: chunk.LogTruncated,
				}
				if err := onEvent(event); err != nil {
					streamErr = err
					return false
				}
				delivered = chunk.NextOffset
				if len(data) > 0 {
					madeProgress = true
				}
				return true
			case "status":
				var view SandboxJob
				if err := json.Unmarshal([]byte(frame.Data), &view); err != nil {
					streamErr = sandboxProtocolError{fmt.Errorf("invalid sandbox log status frame: %w", err)}
					return false
				}
				if view.JobID != jobID || !sandboxJobStatuses[view.Status] {
					streamErr = sandboxProtocolError{errors.New("invalid sandbox log status receipt")}
					return false
				}
				if err := onEvent(SandboxLogEvent{
					Event: "status", JobID: view.JobID, Status: view.Status,
					ExitCode: view.ExitCode, LogTruncated: view.LogTruncated,
				}); err != nil {
					streamErr = err
					return false
				}
				if view.Status != lastStatus {
					lastStatus = view.Status
					madeProgress = true
				}
				if !sandboxJobStatusActive(view.Status) {
					terminal = true
					return false
				}
				return true
			default:
				return true
			}
		})
		response.Body.Close()
		// Callback and parser receipt errors are always fatal — never retried.
		if streamErr != nil {
			return streamErr
		}
		if terminal {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Unified no-progress accounting applies to both clean EOF ends and
		// mid-body transport errors.
		if madeProgress || (sawHeartbeat && time.Since(feedStart) >= time.Second) {
			noProgress = 0
		} else {
			noProgress++
			if noProgress > 3 {
				return errors.New("sandbox log stream made no progress after repeated reconnects")
			}
		}
		if readErr != nil {
			var protocol sandboxProtocolError
			if errors.As(readErr, &protocol) {
				return readErr
			}
			if backoffErr := backoff(); backoffErr != nil {
				return backoffErr
			}
			continue
		}
		if backoffErr := backoff(); backoffErr != nil {
			return backoffErr
		}
	}
}

// sandboxUploadPath normalizes a remote file path the way the API does:
// trimmed, relative paths rooted under /workspace, then POSIX-cleaned. The
// server still owns the allowed-root policy.
func sandboxUploadPath(raw string) string {
	target := strings.TrimSpace(raw)
	if target == "" {
		target = "/workspace"
	}
	if !strings.HasPrefix(target, "/") {
		target = "/workspace/" + target
	}
	return pathpkg.Clean(target)
}

// Upload streams a file through begin/chunk/complete. Chunks are sent as raw
// octet-stream PATCHes of at most 1MiB; every receipt is validated for the
// exact consumed offset. On any failure the upload is aborted best-effort on
// an independent bounded context — an incomplete upload is never completed.
func (s *SandboxClient) Upload(ctx context.Context, id, path string, source io.ReadSeeker) (SandboxUpload, error) {
	var result SandboxUpload
	if source == nil {
		return result, errors.New("a sandbox upload source is required")
	}
	path = sandboxUploadPath(path)
	base, err := s.sandboxPath(id, "/uploads")
	if err != nil {
		return result, err
	}
	// Hash and upload from the caller's current cursor through EOF, rewinding
	// to exactly that cursor between passes and restoring it on every outcome.
	cursor, err := source.Seek(0, io.SeekCurrent)
	if err != nil {
		return result, fmt.Errorf("measure sandbox upload source: %w", err)
	}
	defer func() { _, _ = source.Seek(cursor, io.SeekStart) }()
	end, err := source.Seek(0, io.SeekEnd)
	if err != nil {
		return result, fmt.Errorf("measure sandbox upload source: %w", err)
	}
	size := end - cursor
	if size < 0 {
		return result, errors.New("invalid sandbox upload source bounds")
	}
	if _, err := source.Seek(cursor, io.SeekStart); err != nil {
		return result, fmt.Errorf("rewind sandbox upload source: %w", err)
	}
	hasher := sha256.New()
	hashed := int64(0)
	for hashed < size {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		want := int64(1 << 20)
		if remaining := size - hashed; remaining < want {
			want = remaining
		}
		n, err := io.CopyN(hasher, source, want)
		hashed += n
		if err != nil {
			return result, fmt.Errorf("hash sandbox upload source: %w", err)
		}
	}
	if hashed != size {
		return result, fmt.Errorf("sandbox upload source size changed during hashing")
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if _, err := source.Seek(cursor, io.SeekStart); err != nil {
		return result, fmt.Errorf("rewind sandbox upload source: %w", err)
	}
	var begun SandboxUpload
	if err := s.client.do(ctx, http.MethodPost, base, s.accessToken, struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}{path, size, sum}, &begun); err != nil {
		return result, err
	}
	// Establish the cleanup ID first: only a validated server upload ID is ever
	// sent on the abort route.
	segment, err := sandboxSegment("upload ID", begun.ID)
	if err != nil {
		return result, fmt.Errorf("invalid sandbox upload begin receipt: %w", err)
	}
	abort := func() {
		abortCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		request, reqErr := http.NewRequestWithContext(abortCtx, http.MethodDelete, s.client.baseURL+base+"/"+segment, nil)
		if reqErr != nil {
			return
		}
		request.Header.Set("Authorization", "Bearer "+s.accessToken)
		if response, doErr := s.client.http.Do(request); doErr == nil {
			response.Body.Close()
		}
	}
	fail := func(err error) (SandboxUpload, error) {
		abort()
		return result, err
	}
	if begun.Path != path || begun.Size != size || begun.Offset != 0 || begun.Complete {
		return fail(errors.New("invalid sandbox upload begin receipt"))
	}
	const chunkSize = 1 << 20
	sent := int64(0)
	buffer := make([]byte, chunkSize)
	for sent < size {
		want := int64(chunkSize)
		if remaining := size - sent; remaining < want {
			want = remaining
		}
		n, err := io.ReadFull(source, buffer[:want])
		if err != nil {
			return fail(fmt.Errorf("read sandbox upload source: %w", err))
		}
		var receipt SandboxUpload
		if err := s.rawRequest(ctx, http.MethodPatch, base+"/"+segment+"?offset="+strconv.FormatInt(sent, 10), bytes.NewReader(buffer[:n]), "application/octet-stream", &receipt); err != nil {
			return fail(err)
		}
		if receipt.ID != begun.ID || receipt.Path != path || receipt.Size != size || receipt.Offset != sent+int64(n) || receipt.Complete {
			return fail(errors.New("invalid sandbox upload chunk receipt"))
		}
		sent += int64(n)
	}
	var completed SandboxUpload
	if err := s.client.do(ctx, http.MethodPost, base+"/"+segment+"/complete", s.accessToken, struct{}{}, &completed); err != nil {
		return fail(err)
	}
	if !completed.Complete || completed.ID != begun.ID || completed.Path != path || completed.Size != size || completed.Offset != size {
		return fail(errors.New("invalid sandbox upload completion receipt"))
	}
	if completed.SHA256 != "" && completed.SHA256 != sum {
		return fail(errors.New("sandbox upload checksum mismatch"))
	}
	return completed, nil
}

// Download streams a sandbox file into dest, verifying any declared
// Content-Length and propagating truncation or read failures.
// Download duration is controlled by ctx, not the HTTP client's timeout.
func (s *SandboxClient) Download(ctx context.Context, id, path string, dest io.Writer) error {
	if dest == nil {
		return errors.New("a sandbox download destination is required")
	}
	base, err := s.sandboxPath(id, "/files/content")
	if err != nil {
		return err
	}
	query := url.Values{}
	query.Set("path", path)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.client.baseURL+base+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+s.accessToken)
	client := *s.client.http
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := readResponse(response.Body)
		return decodeResponse(response.StatusCode, raw, nil)
	}
	written, err := io.Copy(dest, response.Body)
	if errors.Is(err, io.ErrUnexpectedEOF) || (err == nil && response.ContentLength >= 0 && written != response.ContentLength) {
		return fmt.Errorf("sandbox download truncated: received %d bytes", written)
	}
	return err
}

// rawRequest sends a non-JSON body and decodes the JSON receipt.
func (s *SandboxClient) rawRequest(ctx context.Context, method, path string, body io.Reader, contentType string, output any) error {
	request, err := http.NewRequestWithContext(ctx, method, s.client.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Authorization", "Bearer "+s.accessToken)
	response, err := s.client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := readResponse(response.Body)
	if err != nil {
		return err
	}
	return decodeResponse(response.StatusCode, raw, output)
}

// Sleep requests sandbox suspension.
func (s *SandboxClient) Sleep(ctx context.Context, id string) error {
	path, err := s.sandboxPath(id, "/sleep")
	if err != nil {
		return err
	}
	return s.client.do(ctx, http.MethodPost, path, s.accessToken, struct{}{}, nil)
}

// Wake requests sandbox resume.
func (s *SandboxClient) Wake(ctx context.Context, id string) error {
	path, err := s.sandboxPath(id, "/wake")
	if err != nil {
		return err
	}
	return s.client.do(ctx, http.MethodPost, path, s.accessToken, struct{}{}, nil)
}

// Delete requests verified sandbox teardown.
func (s *SandboxClient) Delete(ctx context.Context, id string) error {
	path, err := s.sandboxPath(id, "")
	if err != nil {
		return err
	}
	return s.client.do(ctx, http.MethodDelete, path, s.accessToken, nil, nil)
}

// Save suspends the sandbox, waits for it to fall asleep producing a snapshot,
// then patches the snapshot name and optionally marks it retained. An empty
// name with keep=false returns the snapshot as-is.
func (s *SandboxClient) Save(ctx context.Context, id, name string, keep bool) (SandboxSnapshot, error) {
	var result SandboxSnapshot
	if err := s.Sleep(ctx, id); err != nil {
		return result, err
	}
	asleep, err := s.Wait(ctx, id, "asleep")
	if err != nil {
		return result, err
	}
	if asleep.SnapshotID == "" {
		return result, fmt.Errorf("sandbox %s slept without producing a snapshot", id)
	}
	segment, err := sandboxSegment("snapshot ID", asleep.SnapshotID)
	if err != nil {
		return result, err
	}
	path := s.base() + "/sandbox-snapshots/" + segment
	if name == "" && !keep {
		err = s.client.do(ctx, http.MethodGet, path, s.accessToken, nil, &result)
		return result, err
	}
	input := struct {
		Name     *string `json:"name,omitempty"`
		Retained *bool   `json:"retained,omitempty"`
	}{}
	if name != "" {
		input.Name = &name
	}
	if keep {
		retained := true
		input.Retained = &retained
	}
	err = s.client.do(ctx, http.MethodPatch, path, s.accessToken, input, &result)
	return result, err
}

// Snapshots lists ready saved environments, newest first.
func (s *SandboxClient) Snapshots(ctx context.Context, source, before string) (SandboxSnapshotPage, error) {
	var result SandboxSnapshotPage
	query := url.Values{}
	if source != "" {
		if !sandboxIDPattern.MatchString(source) {
			return result, fmt.Errorf("invalid source sandbox ID for Canter sandbox path")
		}
		query.Set("sourceSandboxId", source)
	}
	if before != "" {
		if !sandboxIDPattern.MatchString(before) {
			return result, fmt.Errorf("invalid snapshot cursor for Canter sandbox path")
		}
		query.Set("before", before)
	}
	path := s.base() + "/sandbox-snapshots"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	err := s.client.do(ctx, http.MethodGet, path, s.accessToken, nil, &result)
	return result, err
}

// Templates lists workspace sandbox templates.
func (s *SandboxClient) Templates(ctx context.Context) (SandboxTemplateList, error) {
	var result SandboxTemplateList
	err := s.client.do(ctx, http.MethodGet, s.base()+"/sandbox-templates", s.accessToken, nil, &result)
	return result, err
}

// CreateTemplate creates a named template from a ready snapshot.
func (s *SandboxClient) CreateTemplate(ctx context.Context, name, snapshotID string) (SandboxTemplate, error) {
	var result SandboxTemplate
	if !sandboxIDPattern.MatchString(snapshotID) {
		return result, fmt.Errorf("invalid snapshot ID for Canter sandbox path")
	}
	err := s.client.do(ctx, http.MethodPost, s.base()+"/sandbox-templates", s.accessToken, struct {
		Name       string `json:"name"`
		SnapshotID string `json:"snapshotId"`
	}{name, snapshotID}, &result)
	return result, err
}

// Restore selects which ready snapshot an asleep sandbox restores.
func (s *SandboxClient) Restore(ctx context.Context, id, snapshotID string, generation int) (Sandbox, error) {
	var result Sandbox
	path, err := s.sandboxPath(id, "/restore")
	if err != nil {
		return result, err
	}
	if !sandboxIDPattern.MatchString(snapshotID) {
		return result, fmt.Errorf("invalid snapshot ID for Canter sandbox path")
	}
	err = s.client.do(ctx, http.MethodPost, path, s.accessToken, struct {
		SnapshotID         string `json:"snapshotId"`
		ExpectedGeneration int    `json:"expectedGeneration"`
	}{snapshotID, generation}, &result)
	if err == nil && result.ID != id {
		return result, errors.New("sandbox receipt ID does not match request")
	}
	return result, err
}

func sandboxSetupPendingSDK(setup SandboxSetup) bool {
	return setup.Status == "pending" || setup.Status == "running"
}

// Exec starts a job synchronously: the server waits up to 30 seconds before
// returning a possibly-running SandboxJob. It uses the streamed 35-second
// HTTP budget so the longer response is allowed; the caller context still
// wins. No retries — job start is a mutation.
func (s *SandboxClient) Exec(ctx context.Context, id string, in SandboxExecInput) (SandboxJob, error) {
	var job SandboxJob
	path, err := s.sandboxPath(id, "/exec")
	if err != nil {
		return SandboxJob{}, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return SandboxJob{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.client.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return SandboxJob{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.accessToken)
	response, err := s.streamHTTPClient().Do(request)
	if err != nil {
		return SandboxJob{}, err
	}
	defer response.Body.Close()
	body, err := readResponse(response.Body)
	if err != nil {
		return SandboxJob{}, err
	}
	if err := decodeResponse(response.StatusCode, body, &job); err != nil {
		return SandboxJob{}, err
	}
	if !sandboxIDPattern.MatchString(job.JobID) || !sandboxJobStatuses[job.Status] {
		return SandboxJob{}, errors.New("invalid sandbox exec receipt")
	}
	return job, nil
}
