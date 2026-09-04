package sdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestObserveHTTPSeparatesReachabilityFromStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	status, message := observeHTTP(context.Background(), server.URL)
	if status != http.StatusServiceUnavailable || message == "" {
		t.Fatalf("status=%d message=%q", status, message)
	}
}

func TestObserveHTTPReportsReadyEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	status, message := observeHTTP(context.Background(), server.URL)
	if status != http.StatusNoContent || message != "public endpoint is reachable" {
		t.Fatalf("status=%d message=%q", status, message)
	}
}
