package sdk

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestPublicEndpointRequestsDoNotFollowRedirects(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/private", http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()
	for _, timeout := range []time.Duration{3 * time.Second, 5 * time.Second, 10 * time.Second} {
		response, err := endpointHTTPClient(timeout).Get(endpoint.URL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("redirect response status=%d", response.StatusCode)
		}
	}
	status, _ := observeHTTP(context.Background(), endpoint.URL)
	if status != http.StatusTemporaryRedirect || targetRequests.Load() != 0 {
		t.Fatalf("health status=%d redirected requests=%d", status, targetRequests.Load())
	}
}

func TestObserveHTTPReusesConnections(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("healthy"))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	for range 3 {
		if status, message := observeHTTP(context.Background(), server.URL); status != http.StatusOK {
			t.Fatalf("status=%d message=%s", status, message)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("three health polls opened %d connections, want one", connections.Load())
	}
}

func TestReadEndpointBodyRejectsOversizedResponses(t *testing.T) {
	for _, size := range []int{0, endpointBodyLimit, endpointBodyLimit + 1} {
		body, err := readEndpointBody(strings.NewReader(strings.Repeat("x", size)))
		if size > endpointBodyLimit {
			if err == nil {
				t.Fatal("oversized body was accepted as a truncated success")
			}
		} else if err != nil || len(body) != size {
			t.Fatalf("size=%d length=%d err=%v", size, len(body), err)
		}
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
