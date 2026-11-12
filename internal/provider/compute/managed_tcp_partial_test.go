package compute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedTCPPartialLookup(t *testing.T) {
	const name, ownership = "canter-demo", "sha256:test-owner"
	rule := securityRule{ID: "rule-1", Direction: "ingress", Protocol: "tcp", Min: 8080, Max: 8080}
	owned := securityGroup{ID: "group-1", Name: name, Description: managedPolicyDescription(ownership), Rules: []securityRule{rule}}
	noRule := owned
	noRule.Rules = nil
	duplicateGroup := owned
	duplicateGroup.ID = "group-2"
	duplicateRules := owned
	secondRule := rule
	secondRule.ID = "rule-2"
	duplicateRules.Rules = []securityRule{rule, secondRule}
	foreign := owned
	foreign.Description = managedPolicyDescription("sha256:different-owner")
	unrelated := owned
	unrelated.Name = "unrelated"

	for _, tc := range []struct {
		name         string
		groups       []securityGroup
		unknownPort  bool
		failedLookup bool
		wantOwned    *SecurityPolicy
		duplicate    bool
		wantError    bool
	}{
		{name: "owned rule unattached", groups: []securityGroup{owned}, wantOwned: &SecurityPolicy{ID: "group-1", PortID: "port-1", RuleID: "rule-1", Port: 8080}, wantError: true},
		{name: "owned group without rule", groups: []securityGroup{noRule}, wantOwned: &SecurityPolicy{ID: "group-1", PortID: "port-1", Port: 8080}, wantError: true},
		{name: "duplicate groups", groups: []securityGroup{owned, duplicateGroup}, duplicate: true, wantError: true},
		{name: "duplicate rules", groups: []securityGroup{duplicateRules}, duplicate: true, wantError: true},
		{name: "different ownership", groups: []securityGroup{foreign}},
		{name: "unrelated name", groups: []securityGroup{unrelated}},
		{name: "unknown port", groups: []securityGroup{owned}, unknownPort: true, wantError: true},
		{name: "failed group lookup", failedLookup: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mutations, portGets, groupGets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations.Add(1)
					t.Errorf("lookup attempted mutation: %s %s", r.Method, r.URL.Path)
					http.Error(w, "lookup must not mutate", http.StatusMethodNotAllowed)
					return
				}
				switch r.URL.Path {
				case "/v2.0/ports":
					portGets.Add(1)
					if got := r.URL.Query().Get("device_id"); got != "server-1" {
						t.Errorf("device_id = %q", got)
					}
					ports := []networkPort{{ID: "port-1", SecurityGroups: []string{"default"}}}
					if tc.unknownPort {
						ports = nil
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ports": ports})
				case "/v2.0/security-groups":
					groupGets.Add(1)
					if got := r.URL.Query().Get("name"); got != name {
						t.Errorf("name = %q", got)
					}
					if tc.failedLookup {
						http.Error(w, "lookup failed", http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"security_groups": tc.groups})
				default:
					t.Errorf("unexpected lookup: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
			policy, found, err := client.FindManagedTCPExposure(context.Background(), ManagedTCPExposureRequest{ServerID: "server-1", Name: name, Ownership: ownership, Port: 8080})
			if policy != (SecurityPolicy{}) || found || (err != nil) != tc.wantError {
				t.Fatalf("lookup = %+v, %t, %v", policy, found, err)
			}
			var ambiguous *AmbiguousManagedResourceError
			isAmbiguous := errors.As(err, &ambiguous)
			if tc.wantOwned != nil {
				if !isAmbiguous || ambiguous.OwnedPolicy == nil || *ambiguous.OwnedPolicy != *tc.wantOwned {
					t.Fatalf("ownership evidence = %+v, want %+v (err=%v)", ambiguous, tc.wantOwned, err)
				}
				if !IsAmbiguousManagedResource(err) || errors.Unwrap(ambiguous) != ambiguous.Cause {
					t.Fatalf("ambiguous classification/unwrap changed: %v", err)
				}
			} else if isAmbiguous && ambiguous.OwnedPolicy != nil {
				t.Fatalf("unsafe ownership evidence: %+v", ambiguous.OwnedPolicy)
			}
			if tc.duplicate && !IsDuplicateManagedResource(err) {
				t.Fatalf("expected duplicate error, got %v", err)
			}
			wantGroupGets := int32(1)
			if tc.unknownPort {
				wantGroupGets = 0
			}
			if mutations.Load() != 0 || portGets.Load() != 1 || groupGets.Load() != wantGroupGets {
				t.Fatalf("requests: mutations=%d port GETs=%d group GETs=%d", mutations.Load(), portGets.Load(), groupGets.Load())
			}
		})
	}
}
