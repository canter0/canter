package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sandboxTestClient(t *testing.T, handler http.HandlerFunc) (*SandboxClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := client.Sandboxes("wrk_1", "token_test")
	if err != nil {
		t.Fatal(err)
	}
	return sandboxes, server
}

func TestSandboxClientCreateIdempotencyAndErrors(t *testing.T) {
	var calls atomic.Int32
	var lastBody []byte
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workspaces/wrk_1/sandboxes" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token_test" {
			t.Error("missing bearer token")
		}
		calls.Add(1)
		lastBody, _ = io.ReadAll(r.Body)
		var input SandboxCreateInput
		_ = json.Unmarshal(lastBody, &input)
		if input.IdempotencyKey == "conflict-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "idempotency conflict"})
			return
		}
		if input.IdempotencyKey == "forbidden-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not permitted"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", WorkspaceID: "wrk_1", Phase: "provisioning", Size: input.Size, Name: input.Name})
	})
	input := SandboxCreateInput{Name: "build", Size: "s-2x4", IdempotencyKey: "create-key-1"}
	created, err := sandboxes.Create(context.Background(), input)
	if err != nil || created.ID != "sbx_1" {
		t.Fatalf("create: %#v %v", created, err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(lastBody, &decoded)
	if decoded["idempotencyKey"] != "create-key-1" || decoded["size"] != "s-2x4" {
		t.Fatalf("body: %s", lastBody)
	}
	// Missing idempotency key is rejected before any request.
	if _, err := sandboxes.Create(context.Background(), SandboxCreateInput{Name: "x", Size: "s-1x2"}); err == nil {
		t.Fatal("missing idempotency key must fail before the request")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 server call, got %d", got)
	}
	if _, err := sandboxes.Create(context.Background(), SandboxCreateInput{Name: "x", Size: "s-1x2", IdempotencyKey: "short"}); err == nil {
		t.Fatal("short idempotency key must fail")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected still 1 server call, got %d", got)
	}
	if _, err := sandboxes.Create(context.Background(), SandboxCreateInput{Name: "x", Size: "s-1x2", IdempotencyKey: "conflict-key"}); err == nil {
		t.Fatal("expected conflict error")
	} else {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
			t.Fatalf("expected 409 APIError, got %v", err)
		}
	}
	if _, err := sandboxes.Create(context.Background(), SandboxCreateInput{Name: "x", Size: "s-1x2", IdempotencyKey: "forbidden-key"}); err == nil {
		t.Fatal("expected forbidden error")
	} else {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
			t.Fatalf("expected 403 APIError, got %v", err)
		}
		if strings.Contains(apiErr.Message, "token_test") {
			t.Fatalf("error leaked token: %v", err)
		}
	}
}

func TestSandboxClientRejectsUnsafeIDsAndToken(t *testing.T) {
	client, err := New("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Sandboxes("wrk_1", "  "); err == nil {
		t.Fatal("empty token must fail")
	}
	if _, err := client.Sandboxes("../escape", "tok"); err == nil {
		t.Fatal("path-escape workspace must fail")
	}
	sandboxes, err := client.Sandboxes("wrk_1", "tok")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "a/b", "..", "sbx space", strings.Repeat("x", 200)} {
		if _, err := sandboxes.Get(context.Background(), id); err == nil {
			t.Fatalf("sandbox ID %q must fail", id)
		}
		if _, err := sandboxes.StartJob(context.Background(), id, SandboxExecInput{Command: "true"}); err == nil {
			t.Fatalf("start job with ID %q must fail", id)
		}
		if err := sandboxes.Delete(context.Background(), id); err == nil {
			t.Fatalf("delete with ID %q must fail", id)
		}
	}
	// Malicious base URLs are refused by New itself.
	for _, raw := range []string{"https://user:pw@canter.test", "https://canter.test/?q=1", "https://canter.test/#frag", "http://canter.test"} {
		if _, err := New(raw, nil); err == nil {
			t.Fatalf("base URL %q must fail", raw)
		}
	}
}

func TestSandboxClientNeverFollowsRedirectCredentials(t *testing.T) {
	var authorized atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/workspaces/wrk_1/sandboxes" {
			w.Header().Set("Location", "http://attacker.invalid/steal")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.Header.Get("Authorization") != "" {
			authorized.Store(true)
		}
	}))
	defer origin.Close()
	client, err := New(origin.URL, origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := client.Sandboxes("wrk_1", "token_secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxes.List(context.Background()); err == nil {
		t.Fatal("302 must surface as an error, not follow credentials")
	}
	if authorized.Load() {
		t.Fatal("credentials reached a redirected request")
	}
}

func sandboxJobView(id, status string, exit *int) map[string]any {
	out := map[string]any{"jobId": id, "status": status, "updatedAt": time.Now().UTC().Format(time.RFC3339)}
	if exit != nil {
		out["exitCode"] = *exit
	}
	return out
}

func TestSandboxClientStreamLogsReconnectExactOffset(t *testing.T) {
	var offsets []string
	exit := 0
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/jobs/job_1/events") {
			http.NotFound(w, r)
			return
		}
		offsets = append(offsets, r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		switch len(offsets) {
		case 1:
			// Fragmented UTF-8/CRLF frame: "héllo" split across writes.
			fmt.Fprint(w, "event: output\r\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"aMOpbGxv\",\"offset\":0,\"nextOffset\":6,\"eof\":false,\"status\":\"running\"}\r\n\r\n")
			if flusher != nil {
				flusher.Flush()
			}
		case 2:
			fmt.Fprintf(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"IHdvcmxk\",\"offset\":6,\"nextOffset\":12,\"eof\":false,\"status\":\"running\"}\n\n")
			status, _ := json.Marshal(sandboxJobView("job_1", "completed", &exit))
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
		default:
			t.Errorf("unexpected third connect")
		}
	})
	var collected bytes.Buffer
	var sawStatus bool
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(event SandboxLogEvent) error {
		if event.Event == "output" {
			collected.Write(event.Data)
		}
		if event.Event == "status" && event.Status == "completed" {
			sawStatus = true
			if event.ExitCode == nil || *event.ExitCode != 0 {
				t.Errorf("terminal status missing exit code: %#v", event)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if collected.String() != "héllo world" {
		t.Fatalf("log bytes: %q", collected.String())
	}
	if !sawStatus {
		t.Fatal("terminal status frame not delivered")
	}
	if len(offsets) != 2 || offsets[0] != "0" || offsets[1] != "6" {
		t.Fatalf("reconnect must resume at exact delivered offset, got %v", offsets)
	}
}

func TestSandboxClientStreamLogsRejectsBadReceipt(t *testing.T) {
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Wrong jobId inside the output frame.
		fmt.Fprint(w, "event: output\ndata: {\"jobId\":\"job_other\",\"stream\":\"combined\",\"dataBase64\":\"eA==\",\"offset\":0,\"nextOffset\":1,\"eof\":false,\"status\":\"running\"}\n\n")
	})
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(SandboxLogEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "receipt") {
		t.Fatalf("invalid receipt must fail: %v", err)
	}
}

func TestSandboxClientStreamLogsTerminalNonzeroAndCancel(t *testing.T) {
	exit := 3
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		status, _ := json.Marshal(sandboxJobView("job_1", "failed", &exit))
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
	})
	var got *int
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(event SandboxLogEvent) error {
		if event.Event == "status" {
			got = event.ExitCode
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got == nil || *got != 3 {
		t.Fatalf("terminal exit code not surfaced: %v", got)
	}
	// A cancelled context ends the stream.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sandboxes.StreamLogs(cancelled, "sbx_1", "job_1", "combined", 0, func(SandboxLogEvent) error { return nil }); err == nil {
		t.Fatal("cancelled stream must return an error")
	}
	// Handler abort ends the stream without reconnect.
	aborted := errors.New("stop reading")
	err = sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(SandboxLogEvent) error { return aborted })
	if !errors.Is(err, aborted) {
		t.Fatalf("handler abort must propagate: %v", err)
	}
}

func TestSandboxClientUploadRoundTrip(t *testing.T) {
	payload := make([]byte, (2<<20)+777)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	sum := sha256.Sum256(payload)
	var received bytes.Buffer
	var aborted atomic.Bool
	var completed atomic.Bool
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/uploads"):
			var in struct {
				Path   string `json:"path"`
				Size   int64  `json:"size"`
				SHA256 string `json:"sha256"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Size != int64(len(payload)) || in.SHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("begin: %#v", in)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(SandboxUpload{ID: "upl_1", Path: in.Path, Size: in.Size, Offset: 0})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/uploads/upl_1"):
			offset := r.URL.Query().Get("offset")
			data, _ := io.ReadAll(r.Body)
			var expected int64
			fmt.Sscanf(offset, "%d", &expected)
			if expected != int64(received.Len()) {
				t.Errorf("chunk offset %s but received %d", offset, received.Len())
			}
			if len(data) > 1<<20 {
				t.Errorf("chunk exceeds client bound: %d", len(data))
			}
			received.Write(data)
			_ = json.NewEncoder(w).Encode(SandboxUpload{ID: "upl_1", Path: "/work/file.bin", Size: int64(len(payload)), Offset: int64(received.Len())})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			completed.Store(true)
			_ = json.NewEncoder(w).Encode(SandboxUpload{ID: "upl_1", Path: "/work/file.bin", Size: int64(len(payload)), Offset: int64(len(payload)), SHA256: hex.EncodeToString(sum[:]), Complete: true})
		case r.Method == http.MethodDelete:
			aborted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	result, err := sandboxes.Upload(context.Background(), "sbx_1", "/work/file.bin", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !completed.Load() || aborted.Load() {
		t.Fatalf("completed=%v aborted=%v", completed.Load(), aborted.Load())
	}
	if !result.Complete || !bytes.Equal(received.Bytes(), payload) {
		t.Fatalf("upload receipt/payload mismatch")
	}
}

func TestSandboxClientUploadBadOffsetAborts(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1<<20+10)
	var aborted, completed atomic.Bool
	var chunks atomic.Int32
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/uploads"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(SandboxUpload{ID: "upl_1", Path: "/work/f", Size: int64(len(payload)), Offset: 0})
		case r.Method == http.MethodPatch:
			chunks.Add(1)
			data, _ := io.ReadAll(r.Body)
			// Lie: report an offset that doesn't match consumed bytes.
			_ = json.NewEncoder(w).Encode(SandboxUpload{ID: "upl_1", Path: "/work/f", Size: int64(len(payload)), Offset: int64(len(data)) * 2})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completed.Store(true)
		case r.Method == http.MethodDelete:
			aborted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	if _, err := sandboxes.Upload(context.Background(), "sbx_1", "/work/f", bytes.NewReader(payload)); err == nil {
		t.Fatal("bad chunk receipt must fail")
	}
	if completed.Load() {
		t.Fatal("incomplete upload must never complete")
	}
	if !aborted.Load() {
		t.Fatal("failed upload must be aborted")
	}
	if chunks.Load() != 1 {
		t.Fatalf("expected exactly 1 chunk attempt, got %d", chunks.Load())
	}
}

func TestSandboxClientDownload(t *testing.T) {
	payload := bytes.Repeat([]byte("data"), 4000)
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") == "/short" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.Write(payload[:100])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.Write(payload)
	})
	var dest bytes.Buffer
	if err := sandboxes.Download(context.Background(), "sbx_1", "/work/f", &dest); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(dest.Bytes(), payload) {
		t.Fatal("download bytes mismatch")
	}
	dest.Reset()
	err := sandboxes.Download(context.Background(), "sbx_1", "/short", &dest)
	if err == nil || !strings.Contains(err.Error(), "truncat") {
		t.Fatalf("truncated download must fail: %v", err)
	}
}

func TestSandboxClientDownloadOutlastsRequestTimeout(t *testing.T) {
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(" rest"))
	})
	sandboxes.client.http.Timeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var dest bytes.Buffer
	err := sandboxes.Download(ctx, "sbx_1", "/work/f", &dest)
	if sandboxes.client.http.Timeout != 20*time.Millisecond {
		t.Fatal("download changed the configured HTTP client timeout")
	}
	if err != nil {
		t.Fatalf("download outlasting request timeout: %v", err)
	}
	if dest.String() != "first rest" {
		t.Fatalf("download bytes: %q", dest.String())
	}
}

func TestSandboxClientDownloadCallerCancellation(t *testing.T) {
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	result := make(chan error, 1)
	go func() {
		err := sandboxes.Download(ctx, "sbx_1", "/work/f", writer)
		_ = writer.CloseWithError(err)
		result <- err
	}()
	first := make([]byte, 5)
	if _, err := io.ReadFull(reader, first); err != nil || string(first) != "first" {
		t.Fatalf("first chunk: %q, %v", first, err)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("download cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download did not stop after cancellation")
	}
	if rest, err := io.ReadAll(reader); len(rest) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancellation: %q, %v", rest, err)
	}
}

func TestSandboxClientLifecycleAndLibrary(t *testing.T) {
	var phase atomic.Value
	phase.Store("ready")
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/sleep"):
			phase.Store("sleeping")
			go func() { time.Sleep(50 * time.Millisecond); phase.Store("asleep") }()
			_ = json.NewEncoder(w).Encode(map[string]string{"phase": "sleep"})
		case strings.HasSuffix(path, "/wake"):
			phase.Store("ready")
			_ = json.NewEncoder(w).Encode(map[string]string{"phase": "wake"})
		case strings.HasSuffix(path, "/restore"):
			var in struct {
				SnapshotID         string `json:"snapshotId"`
				ExpectedGeneration int    `json:"expectedGeneration"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.SnapshotID != "snap_1" || in.ExpectedGeneration != 4 {
				t.Errorf("restore: %#v", in)
			}
			_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", Phase: "asleep", SnapshotID: "snap_1", Generation: 5})
		case strings.HasSuffix(path, "/sandbox-snapshots/snap_1"):
			if r.Method == http.MethodPatch {
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				if in["name"] != "saved-env" || in["retained"] != true {
					t.Errorf("snapshot patch: %v", in)
				}
			}
			_ = json.NewEncoder(w).Encode(SandboxSnapshot{ID: "snap_1", Name: "saved-env", State: "ready", Retained: true})
		case strings.HasSuffix(path, "/sandbox-snapshots"):
			_ = json.NewEncoder(w).Encode(SandboxSnapshotPage{Snapshots: []SandboxSnapshot{{ID: "snap_1"}}, HistoryEnabled: true})
		case strings.HasSuffix(path, "/sandbox-templates"):
			if r.Method == http.MethodPost {
				var in struct {
					Name       string `json:"name"`
					SnapshotID string `json:"snapshotId"`
				}
				_ = json.NewDecoder(r.Body).Decode(&in)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(SandboxTemplate{ID: "tmpl_1", Name: in.Name, Latest: &SandboxTemplateVersion{Version: 1, SnapshotID: in.SnapshotID}})
				return
			}
			_ = json.NewEncoder(w).Encode(SandboxTemplateList{Templates: []SandboxTemplate{{ID: "tmpl_1"}}, HistoryEnabled: true})
		case strings.HasSuffix(path, "/sandboxes/sbx_1"):
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", Phase: phase.Load().(string), SnapshotID: "snap_1", Generation: 4})
				return
			}
			if r.Method == http.MethodDelete {
				phase.Store("deleted")
				_ = json.NewEncoder(w).Encode(map[string]bool{"deleting": true})
				return
			}
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	snapshot, err := sandboxes.Save(ctx, "sbx_1", "saved-env", true)
	if err != nil || snapshot.ID != "snap_1" || !snapshot.Retained {
		t.Fatalf("save: %#v %v", snapshot, err)
	}
	page, err := sandboxes.Snapshots(ctx, "", "")
	if err != nil || len(page.Snapshots) != 1 {
		t.Fatalf("snapshots: %#v %v", page, err)
	}
	list, err := sandboxes.Templates(ctx)
	if err != nil || len(list.Templates) != 1 {
		t.Fatalf("templates: %#v %v", list, err)
	}
	tmpl, err := sandboxes.CreateTemplate(ctx, "golden", "snap_1")
	if err != nil || tmpl.ID != "tmpl_1" || tmpl.Latest == nil || tmpl.Latest.SnapshotID != "snap_1" {
		t.Fatalf("create template: %#v %v", tmpl, err)
	}
	restored, err := sandboxes.Restore(ctx, "sbx_1", "snap_1", 4)
	if err != nil || restored.SnapshotID != "snap_1" {
		t.Fatalf("restore: %#v %v", restored, err)
	}
	if err := sandboxes.Wake(ctx, "sbx_1"); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if err := sandboxes.Delete(ctx, "sbx_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestSandboxClientWaitTerminal(t *testing.T) {
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", Phase: "failed", Failure: "boom"})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, err := sandboxes.Wait(ctx, "sbx_1", "ready")
	if err == nil || v.Phase != "failed" {
		t.Fatalf("terminal phase must fail fast: %#v %v", v, err)
	}
}

func TestSandboxClientStreamLogsRetriesTransport(t *testing.T) {
	var attempts atomic.Int32
	exit := 0
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		switch n {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"aGk=\",\"offset\":0,\"nextOffset\":2,\"eof\":false,\"status\":\"running\"}\n\n")
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			status, _ := json.Marshal(sandboxJobView("job_1", "completed", &exit))
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
		}
	})
	var got bytes.Buffer
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(e SandboxLogEvent) error {
		got.Write(e.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.String() != "hi" {
		t.Fatalf("output: %q", got.String())
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts (503, output, terminal), got %d", attempts.Load())
	}
}

func TestSandboxClientStreamLogsNoRetryOnAuth(t *testing.T) {
	var attempts atomic.Int32
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
	})
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(SandboxLogEvent) error { return nil })
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("expected 403 APIError: %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("auth failure must not retry: %d attempts", attempts.Load())
	}
}

func TestSandboxClientStreamLogsMidBodyFailureResumes(t *testing.T) {
	var attempts atomic.Int32
	exit := 0
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "10000")
		if n == 1 {
			fmt.Fprint(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"Zmlyc3Q=\",\"offset\":0,\"nextOffset\":5,\"eof\":false,\"status\":\"running\"}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Hijack and close mid-body to simulate a transport cut.
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		status, _ := json.Marshal(sandboxJobView("job_1", "completed", &exit))
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
	})
	var got bytes.Buffer
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(e SandboxLogEvent) error {
		got.Write(e.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.String() != "first" {
		t.Fatalf("first output must be delivered once: %q", got.String())
	}
	// Second connection must resume at offset 5, not refetch from 0.
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 connections, got %d", attempts.Load())
	}
}

func TestSandboxClientStreamLogsMismatchedStatusJobID(t *testing.T) {
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: status\ndata: {\"jobId\":\"job_other\",\"status\":\"completed\",\"exitCode\":0}\n\n")
	})
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(SandboxLogEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("mismatched status jobID must fail: %v", err)
	}
}

func TestSandboxClientStreamLogsDropsPartialFrame(t *testing.T) {
	var attempts atomic.Int32
	exit := 0
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// Unterminated partial frame at EOF — dropped, reconnect resumes.
			fmt.Fprint(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"aGk=\",\"offset\":0,\"nextOff")
			return
		}
		fmt.Fprint(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"aGk=\",\"offset\":0,\"nextOffset\":2,\"eof\":false,\"status\":\"running\"}\n\n")
		status, _ := json.Marshal(sandboxJobView("job_1", "completed", &exit))
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
	})
	var got bytes.Buffer
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(e SandboxLogEvent) error {
		got.Write(e.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.String() != "hi" {
		t.Fatalf("output: %q", got.String())
	}
}

func TestSandboxClientStreamLogsOversizedFrame(t *testing.T) {
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// >1MiB frame with no newlines.
		fmt.Fprint(w, "data: ")
		w.Write(make([]byte, (1<<20)+1))
	})
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(SandboxLogEvent) error { return nil })
	if err == nil {
		t.Fatal("oversized frame must fail")
	}
}

func TestSandboxClientStreamLogsFragmentedUTF8AndCRLF(t *testing.T) {
	// Real split: CRLF endings, a multibyte UTF-8 char inside JSON split
	// across two writes, multiline data.
	exit := 0
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := []byte("event: output\r\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"w6k=\",\"offset\":0,\"nextOffset\":2,\"eof\":false,\"status\":\"run")
		rest := []byte("ning\"}\r\n\r\nevent: status\r\ndata: ")
		status, _ := json.Marshal(sandboxJobView("job_1", "completed", &exit))
		w.Write(chunk[:len(chunk)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write(append(chunk[len(chunk)/2:], rest...))
		w.Write(status)
		w.Write([]byte("\r\n\r\n"))
	})
	var got bytes.Buffer
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(e SandboxLogEvent) error {
		got.Write(e.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.String() != "é" {
		t.Fatalf("split UTF-8 output: %q", got.String())
	}
}

func TestSandboxClientWaitDeletingToDeleted(t *testing.T) {
	var calls atomic.Int32
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		phase := "deleting"
		if calls.Add(1) >= 2 {
			phase = "deleted"
		}
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", Phase: phase})
	})
	v, err := sandboxes.Wait(context.Background(), "sbx_1", "deleted")
	if err != nil || v.Phase != "deleted" {
		t.Fatalf("wait deleting->deleted: %#v %v", v, err)
	}
	// Cancelled wait: preflight must not issue a request.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sandboxes.Wait(cancelled, "sbx_1", "ready"); err == nil {
		t.Fatal("cancelled wait must fail")
	}
	if _, err := sandboxes.Wait(context.Background(), "sbx_1", "bogus"); err == nil {
		t.Fatal("invalid wait phase must fail")
	}
}

func TestSandboxClientUploadRelativePathAndCursor(t *testing.T) {
	payload := bytes.Repeat([]byte("p"), 300)
	var beginPath string
	var sent int64
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/uploads"):
			var in struct{ Path string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			beginPath = in.Path
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(SandboxUpload{ID: "upl_1", Path: "/workspace/rel/f", Size: int64(len(payload)), Offset: 0})
		case r.Method == http.MethodPatch:
			data, _ := io.ReadAll(r.Body)
			sent += int64(len(data))
			_ = json.NewEncoder(w).Encode(SandboxUpload{ID: "upl_1", Path: "/workspace/rel/f", Size: int64(len(payload)), Offset: sent})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			_ = json.NewEncoder(w).Encode(SandboxUpload{ID: "upl_1", Path: "/workspace/rel/f", Size: int64(len(payload)), Offset: int64(len(payload)), Complete: true})
		default:
			http.NotFound(w, r)
		}
	})
	// Relative path canonicalizes to /workspace/...; nonzero source cursor
	// uploads only bytes from the cursor and restores it.
	source := bytes.NewReader(append([]byte("SKIP"), payload...))
	if _, err := source.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	result, err := sandboxes.Upload(context.Background(), "sbx_1", "rel/f", source)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if beginPath != "/workspace/rel/f" {
		t.Fatalf("begin path %q", beginPath)
	}
	if !result.Complete || sent != int64(len(payload)) {
		t.Fatalf("upload incomplete: %#v sent=%d", result, sent)
	}
	if pos, _ := source.Seek(0, io.SeekCurrent); pos != 4 {
		t.Fatalf("source cursor not restored: %d", pos)
	}
}

// boundedReader delivers chunks of a fixed size to prove the parser bounds
// aggregate frame bytes without ever reading unbounded data.
type boundedReader struct {
	remaining []byte
	chunk     int
	reads     int
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if len(r.remaining) == 0 {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.remaining) {
		n = len(r.remaining)
	}
	copy(p, r.remaining[:n])
	r.remaining = r.remaining[n:]
	r.reads++
	return n, nil
}

func TestReadSSEFramesOversizeFailsFast(t *testing.T) {
	// >1MiB with no newline must hit the bound, not read unbounded data.
	r := &boundedReader{remaining: make([]byte, 10<<20), chunk: 64 << 10}
	called := false
	err := readSSEFrames(context.Background(), r, func(sseFrame) bool { called = true; return true })
	var protocol sandboxProtocolError
	if !errors.As(err, &protocol) {
		t.Fatalf("expected protocol bound error, got %v", err)
	}
	if called {
		t.Fatal("no frame may be delivered for an oversized frame")
	}
}

func TestReadSSEFramesLargeFragmentedFrame(t *testing.T) {
	// A valid ~350KiB output frame split into 1KiB reads must parse once —
	// the old carry double-counting falsely rejected this.
	payload := strings.Repeat("x", 350<<10)
	frame := "event: output\ndata: " + payload + "\n\n"
	r := &boundedReader{remaining: []byte(frame), chunk: 1024}
	var got sseFrame
	calls := 0
	err := readSSEFrames(context.Background(), r, func(f sseFrame) bool { got = f; calls++; return true })
	if err != nil {
		t.Fatalf("valid large frame must parse: %v", err)
	}
	if calls != 1 || len(got.Data) != len(payload) {
		t.Fatalf("expected one %d-byte frame, got %d calls of %d bytes", len(payload), calls, len(got.Data))
	}
}

func TestSandboxClientStreamLogsCallbackErrorSingleRequest(t *testing.T) {
	var attempts atomic.Int32
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: status\ndata: {\"jobId\":\"job_1\",\"status\":\"running\"}\n\n")
	})
	sentinel := errors.New("handler says stop")
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(SandboxLogEvent) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback error must propagate: %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("callback errors must never retry: %d requests", attempts.Load())
	}
}

func TestSandboxClientStreamLogsResumedOffsetAsserted(t *testing.T) {
	var offsets []string
	exit := 0
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "text/event-stream")
		if len(offsets) == 1 {
			fmt.Fprint(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"YWJj\",\"offset\":0,\"nextOffset\":3,\"eof\":false,\"status\":\"running\"}\n\n")
			return // clean EOF end -> reconnect
		}
		status, _ := json.Marshal(sandboxJobView("job_1", "completed", &exit))
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
	})
	var got bytes.Buffer
	if err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(e SandboxLogEvent) error { got.Write(e.Data); return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(offsets) != 2 || offsets[1] != "3" {
		t.Fatalf("reconnect must resume at delivered offset 3: %v", offsets)
	}
}

func TestSandboxClientStreamLogsProgressResetsNoProgress(t *testing.T) {
	// Five consecutive feeds that each deliver progress then end must all
	// reconnect — the no-progress bound only counts truly stalled feeds.
	var attempts atomic.Int32
	exit := 0
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 5 {
			offset := r.URL.Query().Get("offset")
			fmt.Fprintf(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"eA==\",\"offset\":%s,\"nextOffset\":%d,\"eof\":false,\"status\":\"running\"}\n\n", offset, func() int64 { v, _ := strconv.ParseInt(offset, 10, 64); return v + 1 }())
			return
		}
		status, _ := json.Marshal(sandboxJobView("job_1", "completed", &exit))
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
	})
	var got bytes.Buffer
	err := sandboxes.StreamLogs(context.Background(), "sbx_1", "job_1", "combined", 0, func(e SandboxLogEvent) error { got.Write(e.Data); return nil })
	if err != nil {
		t.Fatalf("progressing feeds must never hit the no-progress bound: %v", err)
	}
	if got.Len() != 5 {
		t.Fatalf("expected 5 output bytes, got %d", got.Len())
	}
}

func TestSandboxClientWaitQueuedToReadyAndTerminal(t *testing.T) {
	var calls atomic.Int32
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var v Sandbox
		switch {
		case calls.Add(1) == 1:
			v = Sandbox{ID: "sbx_1", Phase: "queued"}
		case calls.Load() == 2:
			v = Sandbox{ID: "sbx_1", Phase: "provisioning"}
		default:
			v = Sandbox{ID: "sbx_1", Phase: "ready"}
		}
		_ = json.NewEncoder(w).Encode(v)
	})
	v, err := sandboxes.Wait(context.Background(), "sbx_1", "ready")
	if err != nil || v.Phase != "ready" {
		t.Fatalf("queued->ready must succeed: %#v %v", v, err)
	}
	calls.Store(0)
	sandboxes2, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", Phase: "deleted"})
	})
	if _, err := sandboxes2.Wait(context.Background(), "sbx_1", "ready"); err == nil {
		t.Fatal("deleted->ready must fail")
	}
	sandboxes3, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", Phase: "bogus-phase"})
	})
	if _, err := sandboxes3.Wait(context.Background(), "sbx_1", "ready"); err == nil {
		t.Fatal("unknown phase must fail")
	}
}

func TestSandboxClientWaitHonorsExplicitDeadline(t *testing.T) {
	// An explicit longer caller deadline must not be clamped to 15min.
	var deadline time.Time
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if d, ok := req.Context().Deadline(); ok {
			deadline = d
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"sbx_1","phase":"ready"}`)), Header: make(http.Header)}, nil
	})
	client, err := New("http://127.0.0.1:1", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := client.Sandboxes("wrk_1", "tok")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if _, err := sandboxes.Wait(ctx, "sbx_1", "ready"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if deadline.IsZero() || time.Until(deadline) < 25*time.Minute {
		t.Fatalf("explicit 30min deadline was clamped: %v", deadline)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSandboxClientWaitSetupGates(t *testing.T) {
	var calls atomic.Int32
	sandboxes, _ := sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var v Sandbox
		switch calls.Add(1) {
		case 1:
			v = Sandbox{ID: "sbx_1", Phase: "ready", Setup: SandboxSetup{Status: "pending"}}
		case 2:
			v = Sandbox{ID: "sbx_1", Phase: "ready", Setup: SandboxSetup{Status: "running", JobID: "job_setup_1"}}
		default:
			v = Sandbox{ID: "sbx_1", Phase: "ready", Setup: SandboxSetup{Status: "succeeded"}}
		}
		_ = json.NewEncoder(w).Encode(v)
	})
	v, err := sandboxes.Wait(context.Background(), "sbx_1", "ready")
	if err != nil || v.Setup.Status != "succeeded" {
		t.Fatalf("pending->running->succeeded must finish waiting: %+v %v", v, err)
	}
	calls.Store(0)
	sandboxes, _ = sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", Phase: "ready", Setup: SandboxSetup{Status: "failed", JobID: "job_setup_1", Error: "boom"}})
	})
	if _, err := sandboxes.Wait(context.Background(), "sbx_1", "ready"); err == nil || !strings.Contains(err.Error(), "job_setup_1") {
		t.Fatalf("failed setup must error with job handle: %v", err)
	}
	sandboxes, _ = sandboxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sbx_1", Phase: "ready", Setup: SandboxSetup{Status: "bogus"}})
	})
	if _, err := sandboxes.Wait(context.Background(), "sbx_1", "ready"); err == nil {
		t.Fatal("unknown setup status must be a protocol error")
	}
}
