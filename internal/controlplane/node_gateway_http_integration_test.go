package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

type fakeNodeGatewayEngine struct{ observed sdk.ObservedRelease }

func (f *fakeNodeGatewayEngine) NodeSnapshot(_ context.Context, system sdk.System) (sdk.NodeSnapshot, error) {
	return sdk.NodeSnapshot{SchemaVersion: "v1", Generation: "generation-one", System: system.Metadata.Name}, nil
}
func (f *fakeNodeGatewayEngine) NodeArtifact(context.Context, sdk.System, string, string) ([]byte, error) {
	return []byte("artifact"), nil
}
func (f *fakeNodeGatewayEngine) PutNodeObserved(_ context.Context, _ sdk.System, value sdk.ObservedRelease) error {
	f.observed = value
	return nil
}
func (f *fakeNodeGatewayEngine) PutNodeControlAck(context.Context, sdk.System, string, time.Time) error {
	return nil
}
func (f *fakeNodeGatewayEngine) PutNodeRuntimeActionResult(context.Context, sdk.System, sdk.RuntimeActionResult) error {
	return nil
}

func TestNodeGatewayTLSExchangeAndScopedRequests(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	_, workspace, _, err := store.Signup(ctx, "gateway-owner@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("gateway-api", "run an api").OnHost("c1", 1, 1024, 256).WithM1("ignored").Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).Build()
	if err != nil {
		t.Fatal(err)
	}
	system, err = canonicalizeSystemForWorkspace(workspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PutSystem(ctx, workspace.ID, system); err != nil {
		t.Fatal(err)
	}
	enrollment, err := store.CreateNodeEnrollment(ctx, workspace.ID, system.Metadata.Name, system.Spec.M1.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	engine := &fakeNodeGatewayEngine{}
	server := httptest.NewTLSServer(NewHTTPServer(&Service{Store: store, NodeGateway: engine}, HTTPConfig{}))
	defer server.Close()
	exchange, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/node/enrollments/"+enrollment.ID+"/exchange", nil)
	if err != nil {
		t.Fatal(err)
	}
	exchange.Header.Set("Authorization", "Bearer "+enrollment.EnrollmentToken)
	response, err := server.Client().Do(exchange)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var credential NodeCredential
	if response.StatusCode != http.StatusOK {
		t.Fatalf("exchange HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&credential); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/node/snapshot", nil)
	request.Header.Set("Authorization", "Bearer "+credential.NodeToken)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"generation-one"` {
		t.Fatalf("snapshot HTTP %d etag=%q", response.StatusCode, response.Header.Get("ETag"))
	}
	observed := `{"schemaVersion":"v1","system":"gateway-api","phase":"running","restarts":0,"publicPort":8080,"healthy":true,"updatedAt":"2026-09-01T00:00:00Z"}`
	request, _ = http.NewRequestWithContext(ctx, http.MethodPut, server.URL+"/v1/node/observed", strings.NewReader(observed))
	request.Header.Set("Authorization", "Bearer "+credential.NodeToken)
	request.Header.Set("Content-Type", "application/json")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || engine.observed.System != "gateway-api" {
		t.Fatalf("observed HTTP %d value=%#v", response.StatusCode, engine.observed)
	}
}

func TestNodeGatewayRejectsUntrustedForwardedHTTP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://control.example/v1/node/snapshot", nil)
	r.RemoteAddr = "198.51.100.9:443"
	r.Header.Set("X-Forwarded-Proto", "https")
	if nodeRequestIsHTTPS(r) {
		t.Fatal("trusted forwarded scheme from a public peer")
	}
	r.RemoteAddr = "127.0.0.1:53000"
	if !nodeRequestIsHTTPS(r) {
		t.Fatal("did not trust loopback reverse proxy")
	}
}
