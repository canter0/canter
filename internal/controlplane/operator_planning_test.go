package controlplane

import (
	"context"
	"encoding/json"
	"testing"
)

// Planning must remain usable without a store: these tools must not emit a
// setup surface, create a resource, or persist an authorization.
func TestOperatorResourcePlanningStaysInConversation(t *testing.T) {
	operator := &OperatorRuntime{Server: &HTTPServer{service: &Service{}}}
	for _, name := range []string{"canter_show_compute", "canter_show_storage"} {
		t.Run(name, func(t *testing.T) {
			result, handled, err := operator.localTool(context.Background(), OperatorRun{}, Conversation{}, Principal{}, name, json.RawMessage(`{}`))
			if err != nil || !handled {
				t.Fatalf("planning tool: handled=%v err=%v", handled, err)
			}
			info := result.(map[string]any)
			if name == "canter_show_compute" {
				if info["status"] != "planning_available" || info["planningAvailable"] != true || info["canterComputeRateAvailable"] != true {
					t.Fatalf("compute planning and its published estimate must be available: %v", info)
				}
			} else if info["standaloneProvisioning"] != false || info["status"] != "planning_only" {
				t.Fatalf("unexpected storage capabilities: %v", info)
			}
			if info["standaloneProvisioning"] == true {
				t.Fatal("planning must not claim standalone provisioning")
			}
			if _, exists := info["view"]; exists {
				t.Fatal("planning must not open a view")
			}
		})
	}
}
