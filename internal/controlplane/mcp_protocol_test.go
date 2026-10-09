package controlplane

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type readCounter struct {
	reads int
	io.Reader
}

func (r *readCounter) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func testMCPHandler() *HTTPServer {
	return &HTTPServer{mcpRequests: make(chan struct{}, 1), mcpLargeBodies: make(chan struct{}, 1)}
}

func TestMCPPublicDiscoveryRemainsAvailable(t *testing.T) {
	h := testMCPHandler()
	for _, method := range []string{"initialize", "ping", "tools/list"} {
		t.Run(method, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"}`
			r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.mcp(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestMCPRejectsUnauthenticatedLargeBodyBeforeReading(t *testing.T) {
	h := testMCPHandler()
	reader := &readCounter{Reader: strings.NewReader(strings.Repeat("x", mcpPublicBodyLimit+1))}
	r := httptest.NewRequest(http.MethodPost, "/mcp", io.NopCloser(reader))
	r.ContentLength = mcpPublicBodyLimit + 1
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.mcp(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if reader.reads != 0 {
		t.Fatalf("body was read %d times before rejecting oversized unauthenticated request", reader.reads)
	}
}

func TestMCPRejectsWhenParsingCapacityIsFullBeforeReading(t *testing.T) {
	h := testMCPHandler()
	h.mcpRequests <- struct{}{}
	reader := &readCounter{Reader: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}
	r := httptest.NewRequest(http.MethodPost, "/mcp", io.NopCloser(reader))
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.mcp(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if reader.reads != 0 {
		t.Fatalf("body was read %d times while parsing capacity was full", reader.reads)
	}
}
