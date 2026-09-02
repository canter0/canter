package compute

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeImage(t *testing.T) {
	if normalizeImage("Ubuntu-24.04-amd64") != normalizeImage("ubuntu-24.04") {
		t.Fatal("public image alias does not resolve to the provider-neutral form")
	}
}

func TestDeleteSecurityPolicyTreatsNotFoundAsDeleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v2.0/security-groups/already-gone" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, `{"NeutronError":{"type":"SecurityGroupNotFound"}}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	if err := client.DeleteSecurityPolicy(context.Background(), "already-gone"); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
}

func TestDeleteSecurityPolicyRetriesConflict(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "still attached", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	if err := client.DeleteSecurityPolicy(context.Background(), "eventually-gone"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("got %d attempts, want 2", attempts.Load())
	}
}

func TestCreateManagedWritesDeterministicIntentMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/servers" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			Server struct {
				Metadata map[string]string `json:"metadata"`
			} `json:"server"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"canter.managed": "true", "canter.sandbox": "demo", "canter.operation": "op-1", "canter.resource": "canter-demo-1"}
		for key, value := range want {
			if payload.Server.Metadata[key] != value {
				t.Fatalf("metadata[%q]=%q, want %q", key, payload.Server.Metadata[key], value)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"server": map[string]string{"id": "server-1", "name": "canter-demo-1", "status": "BUILD"}})
	}))
	defer server.Close()

	client := &Client{http: server.Client(), session: session{Token: "test", ComputeURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	created, err := client.CreateManaged(context.Background(), ManagedServerRequest{Name: "canter-demo-1", Sandbox: "demo", OperationID: "op-1", FlavorID: "shape", ImageID: "image", NetworkID: "network", UserData: "boot"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "server-1" {
		t.Fatalf("created=%+v", created)
	}
}

func TestFindManagedServersFiltersExactIntentAndSorts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/servers/detail" || r.URL.Query().Get("name") != "canter-demo-1" {
			t.Fatalf("unexpected request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		managed := func(id, name, sandbox, operation, resource string) Server {
			return Server{ID: id, Name: name, Metadata: map[string]string{"canter.managed": "true", "canter.sandbox": sandbox, "canter.operation": operation, "canter.resource": resource}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"servers": []Server{
			managed("z", "canter-demo-1", "demo", "op-1", "canter-demo-1"),
			managed("ignored-name", "canter-demo-10", "demo", "op-1", "canter-demo-1"),
			managed("ignored-sandbox", "canter-demo-1", "other", "op-1", "canter-demo-1"),
			managed("a", "canter-demo-1", "demo", "op-1", "canter-demo-1"),
			managed("ignored-op", "canter-demo-1", "demo", "op-2", "canter-demo-1"),
		}})
	}))
	defer server.Close()

	client := &Client{http: server.Client(), session: session{Token: "test", ComputeURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	matches, err := client.FindManagedServers(context.Background(), "demo", "op-1", "canter-demo-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || matches[0].ID != "a" || matches[1].ID != "z" {
		t.Fatalf("matches=%+v", matches)
	}
}

func TestExposeTCPReconcilesAmbiguousCreateAndAttachResponses(t *testing.T) {
	groupExists, ruleExists, attached := false, false, false
	ownership := "sha256:test-owner"
	description := managedPolicyDescription(ownership)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/ports":
			groups := []string{"default"}
			if attached {
				groups = append(groups, "group-1")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []any{map[string]any{"id": "port-1", "security_groups": groups}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/security-groups":
			groups := []any{}
			if groupExists {
				rules := []any{}
				if ruleExists {
					rules = append(rules, map[string]any{"id": "rule-1", "direction": "ingress", "protocol": "tcp", "port_range_min": 8080, "port_range_max": 8080})
				}
				groups = append(groups, map[string]any{"id": "group-1", "name": "canter-demo", "description": description, "security_group_rules": rules})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"security_groups": groups})
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-groups":
			groupExists = true
			http.Error(w, "response lost after create", http.StatusGatewayTimeout)
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-group-rules":
			ruleExists = true
			http.Error(w, "response lost after create", http.StatusGatewayTimeout)
		case r.Method == http.MethodPut && r.URL.Path == "/v2.0/ports/port-1":
			attached = true
			http.Error(w, "response lost after attach", http.StatusGatewayTimeout)
		default:
			t.Fatalf("unexpected request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer server.Close()

	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	policy, err := client.ExposeManagedTCP(context.Background(), ManagedTCPExposureRequest{ServerID: "server-1", Name: "canter-demo", Ownership: ownership, Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if policy.ID != "group-1" || policy.RuleID != "rule-1" || policy.PortID != "port-1" || !attached {
		t.Fatalf("policy=%+v attached=%t", policy, attached)
	}
}

func TestExposeTCPRejectsDuplicateManagedPolicies(t *testing.T) {
	ownership := "sha256:test-owner"
	description := managedPolicyDescription(ownership)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.0/ports":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []any{map[string]any{"id": "port-1"}}})
		case "/v2.0/security-groups":
			_ = json.NewEncoder(w).Encode(map[string]any{"security_groups": []any{
				map[string]any{"id": "group-1", "name": "canter-demo", "description": description}, map[string]any{"id": "group-2", "name": "canter-demo", "description": description},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	_, err := client.ExposeManagedTCP(context.Background(), ManagedTCPExposureRequest{ServerID: "server-1", Name: "canter-demo", Ownership: ownership, Port: 8080})
	if !IsDuplicateManagedResource(err) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagedExposureNeverAdoptsNameOnlyPolicy(t *testing.T) {
	ownership := "sha256:workspace-owner"
	description := managedPolicyDescription(ownership)
	createdDescription := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/ports":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []any{map[string]any{"id": "port-1", "security_groups": []string{"default"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/security-groups":
			_ = json.NewEncoder(w).Encode(map[string]any{"security_groups": []any{map[string]any{"id": "foreign", "name": "canter-demo-abcd", "description": "unmanaged"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-groups":
			var payload struct {
				SecurityGroup map[string]string `json:"security_group"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			createdDescription = payload.SecurityGroup["description"]
			_ = json.NewEncoder(w).Encode(map[string]any{"security_group": map[string]any{"id": "owned", "name": "canter-demo-abcd", "description": description}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-group-rules":
			_ = json.NewEncoder(w).Encode(map[string]any{"security_group_rule": map[string]any{"id": "rule-1"}})
		case r.Method == http.MethodPut && r.URL.Path == "/v2.0/ports/port-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	policy, err := client.ExposeManagedTCP(context.Background(), ManagedTCPExposureRequest{ServerID: "server-1", Name: "canter-demo-abcd", Ownership: ownership, Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if policy.ID != "owned" || createdDescription != description {
		t.Fatalf("policy=%+v description=%q", policy, createdDescription)
	}
}

func TestManagedExposureReturnsTerminalAmbiguityWhenGroupInvisible(t *testing.T) {
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/ports":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []any{map[string]any{"id": "port-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/security-groups":
			_ = json.NewEncoder(w).Encode(map[string]any{"security_groups": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-groups":
			creates.Add(1)
			http.Error(w, "response lost", http.StatusGatewayTimeout)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	_, err := client.ExposeManagedTCP(context.Background(), ManagedTCPExposureRequest{ServerID: "server-1", Name: "canter-demo-abcd", Ownership: "sha256:owner", Port: 8080})
	if !IsAmbiguousManagedResource(err) || creates.Load() != 1 {
		t.Fatalf("err=%v creates=%d", err, creates.Load())
	}
}

func TestManagedExposureReturnsTerminalAmbiguityWhenRuleInvisible(t *testing.T) {
	var creates atomic.Int32
	ownership := "sha256:owner"
	description := managedPolicyDescription(ownership)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/ports":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []any{map[string]any{"id": "port-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/security-groups":
			_ = json.NewEncoder(w).Encode(map[string]any{"security_groups": []any{map[string]any{"id": "owned", "name": "canter-demo-abcd", "description": description, "security_group_rules": []any{}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-group-rules":
			creates.Add(1)
			http.Error(w, "response lost", http.StatusGatewayTimeout)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	_, err := client.ExposeManagedTCP(context.Background(), ManagedTCPExposureRequest{ServerID: "server-1", Name: "canter-demo-abcd", Ownership: ownership, Port: 8080})
	if !IsAmbiguousManagedResource(err) || creates.Load() != 1 {
		t.Fatalf("err=%v creates=%d", err, creates.Load())
	}
}
