package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/canter0/canter/internal/controlplane"
)

func TestCIStartupManagedFailureKeepsHTTPAvailable(t *testing.T) {
	env := ciRunEnv(t, t.TempDir())
	env["CANTER_CI_MANAGED_PROFILE_FILE"] = "/missing/SECRET-profile"
	service := ciManagedServiceFromEnv(nil, controlplane.CIConfig{}, ciEnvMap(env))
	core, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Run(core); err == nil {
		t.Fatal("invalid managed configuration was not rejected")
	}
	if core.Err() != nil {
		t.Fatal("managed startup failure stopped the core context")
	}
	handler := controlplane.NewHTTPServer(&controlplane.Service{}, controlplane.HTTPConfig{CI: controlplane.CIConfig{ManagedService: service}})
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/v1/ci/runner-bootstrap", http.StatusServiceUnavailable},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Fatalf("%s: status=%d want=%d", tc.path, w.Code, tc.want)
		}
	}
}
