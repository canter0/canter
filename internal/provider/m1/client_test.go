package m1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestObjectStoreDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	t.Setenv("CANTER_M1_ENDPOINT", origin.URL)
	t.Setenv("CANTER_M1_BUCKET", "test-bucket")
	t.Setenv("CANTER_M1_ACCESS_KEY", "test-access")
	t.Setenv("CANTER_M1_SECRET_KEY", "test-secret")
	t.Setenv("CANTER_M1_REGION", "auto")
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Put(context.Background(), "test-key", []byte("test"), "application/octet-stream"); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect target received %d requests", redirected.Load())
	}
}

func TestConfiguredEndpointRequiresSecureOrLoopbackURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://objects.example", true},
		{"http://127.0.0.1:9000", true},
		{"http://objects.example", false},
		{"https://user:secret@objects.example", false},
		{"https://objects.example/?token=secret", false},
		{"https://objects.example/#fragment", false},
	} {
		if got := validEndpointURL(tc.url); got != tc.want {
			t.Errorf("validEndpointURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
