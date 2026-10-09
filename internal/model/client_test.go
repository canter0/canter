package model

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func modelClientForServer(server *httptest.Server) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		urlCopy := *req.URL
		urlCopy.Scheme = "http"
		urlCopy.Host = strings.TrimPrefix(server.URL, "http://")
		copy.URL = &urlCopy
		return http.DefaultTransport.RoundTrip(copy)
	})}
}

func withModelAPIKey(t *testing.T) {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", "test-secret")
}

func TestClientCompilesBoundedResponse(t *testing.T) {
	withModelAPIKey(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"fixture-model","choices":[{"message":{"content":"{\"ok\":true}"}}]}`)
	}))
	defer provider.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	modelName, _, err := (Client{HTTP: modelClientForServer(provider)}).Compile(context.Background(), "compile", &result)
	if err != nil || modelName != "fixture-model" || !result.OK {
		t.Fatalf("Compile returned %q, %+v, %v", modelName, result, err)
	}
}

func TestClientDoesNotFollowRedirectOrExposeProviderError(t *testing.T) {
	withModelAPIKey(t)
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls++
		if r.Header.Get("Authorization") != "" {
			t.Error("credential forwarded to redirect target")
		}
		_, _ = io.WriteString(w, `{"model":"redirected","choices":[{"message":{"content":"{\"ok\":true}"}}]}`)
	}))
	defer target.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("credential missing from provider request")
		}
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, `{"error":{"message":"prompt-echo-secret"}}`)
	}))
	defer provider.Close()

	client := Client{HTTP: modelClientForServer(provider)}
	var targetValue map[string]any
	_, _, err := client.Compile(context.Background(), "compile", &targetValue)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") || strings.Contains(err.Error(), "prompt-echo-secret") {
		t.Fatalf("unexpected redirect error: %v", err)
	}
	if targetCalls != 0 {
		t.Fatalf("redirect was followed %d times", targetCalls)
	}
}

func TestClientBoundsAndRedactsModelResponses(t *testing.T) {
	withModelAPIKey(t)
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{name: "oversized", body: strings.Repeat("x", maxModelResponseBytes+1), status: http.StatusOK, want: "size limit"},
		{name: "provider error", body: `{"error":{"message":"sensitive-prompt-echo"}}`, status: http.StatusTooManyRequests, want: "HTTP 429"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer provider.Close()
			var targetValue map[string]any
			_, _, err := (Client{HTTP: modelClientForServer(provider)}).Compile(context.Background(), "compile", &targetValue)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "sensitive-prompt-echo") {
				t.Fatalf("provider response leaked through error: %v", err)
			}
		})
	}
}
