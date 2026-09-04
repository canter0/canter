package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// NodeDesiredRelease is the provider-neutral desired state returned to a node.
// ArtifactKey intentionally never crosses the control-plane boundary.
type NodeDesiredRelease struct {
	SchemaVersion string            `json:"schemaVersion"`
	System        string            `json:"system"`
	Version       string            `json:"version"`
	ArtifactSHA   string            `json:"artifactSha256"`
	Command       []string          `json:"command"`
	Environment   map[string]string `json:"environment,omitempty"`
	HealthPath    string            `json:"healthPath"`
	PublicPort    int               `json:"publicPort"`
	Replicas      int               `json:"replicas"`
	CapacityLease *CapacityLease    `json:"capacityLease,omitempty"`
	RequestedAt   time.Time         `json:"requestedAt"`
}

type NodeRuntimeAction struct {
	Action RuntimeAction `json:"action"`
	Lease  ChangeLease   `json:"lease"`
}

type NodeSnapshot struct {
	SchemaVersion string              `json:"schemaVersion"`
	Generation    string              `json:"generation"`
	System        string              `json:"system"`
	RuntimePlan   *RuntimePlan        `json:"runtimePlan,omitempty"`
	Desired       *NodeDesiredRelease `json:"desired,omitempty"`
	Control       *RuntimeControl     `json:"control,omitempty"`
	RuntimeAction *NodeRuntimeAction  `json:"runtimeAction,omitempty"`
}

func (c *Client) NodeSnapshot(ctx context.Context, system System) (NodeSnapshot, error) {
	if err := system.Validate(); err != nil {
		return NodeSnapshot{}, err
	}
	prefix := strings.TrimRight(system.Spec.M1.Prefix, "/")
	out := NodeSnapshot{SchemaVersion: "v1", System: system.Metadata.Name}
	var plan RuntimePlan
	if found, err := c.m1.GetOptional(ctx, prefix+"/runtime-plan.json", &plan); err != nil {
		return out, err
	} else if found {
		if err := plan.Validate(system.Metadata.Name); err != nil {
			return out, err
		}
		out.RuntimePlan = &plan
	}
	var desired ReleaseManifest
	if found, err := c.m1.GetOptional(ctx, desiredKey(system), &desired); err != nil {
		return out, err
	} else if found {
		if desired.SchemaVersion != "v1" || desired.System != system.Metadata.Name {
			return out, fmt.Errorf("invalid desired release for system")
		}
		replicas := desired.Replicas
		if replicas < 1 {
			replicas = 1
		}
		out.Desired = &NodeDesiredRelease{SchemaVersion: desired.SchemaVersion, System: desired.System, Version: desired.Version, ArtifactSHA: desired.ArtifactSHA, Command: append([]string(nil), desired.Command...), Environment: cloneStrings(desired.Environment), HealthPath: desired.HealthPath, PublicPort: desired.PublicPort, Replicas: replicas, CapacityLease: desired.CapacityLease, RequestedAt: desired.RequestedAt}
	}
	var control RuntimeControl
	if found, err := c.m1.GetOptional(ctx, controlKey(system), &control); err != nil {
		return out, err
	} else if found && control.ID != "" {
		var ack struct {
			ID string `json:"id"`
		}
		acked, ackErr := c.m1.GetOptional(ctx, prefix+"/control-ack.json", &ack)
		if ackErr != nil {
			return out, ackErr
		}
		if !acked || ack.ID != control.ID {
			out.Control = &control
		}
	}
	var action RuntimeAction
	if found, err := c.m1.GetOptional(ctx, runtimeActionRequestKey(system), &action); err != nil {
		return out, err
	} else if found {
		var result RuntimeActionResult
		completed, err := c.m1.GetOptional(ctx, runtimeActionResultKey(system, action.ID), &result)
		if err != nil {
			return out, err
		}
		var lease ChangeLease
		leaseFound := false
		if !completed && strings.HasPrefix(action.LeaseKey, prefix+"/changes/") {
			leaseFound, err = c.m1.GetOptional(ctx, action.LeaseKey, &lease)
			if err != nil {
				return out, err
			}
		}
		if !completed && leaseFound && lease.FencingToken == action.FencingToken && lease.ExpiresAt.After(time.Now().UTC()) && strings.HasPrefix(action.ID, lease.ChangeID+"-") {
			action.LeaseKey = ""
			out.RuntimeAction = &NodeRuntimeAction{Action: action, Lease: lease}
		}
	}
	b, err := json.Marshal(struct {
		Plan    *RuntimePlan        `json:"runtimePlan,omitempty"`
		Desired *NodeDesiredRelease `json:"desired,omitempty"`
		Control *RuntimeControl     `json:"control,omitempty"`
		Action  *NodeRuntimeAction  `json:"runtimeAction,omitempty"`
	}{out.RuntimePlan, out.Desired, out.Control, out.RuntimeAction})
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(b)
	out.Generation = hex.EncodeToString(sum[:])
	return out, nil
}

// NodeArtifact returns bytes only for the currently desired digest and only
// from either the System's immutable prefix or the exact workspace-owned key.
func (c *Client) NodeArtifact(ctx context.Context, system System, digest, ownedKey string) ([]byte, error) {
	if len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return nil, fmt.Errorf("invalid artifact digest")
	}
	var desired ReleaseManifest
	if err := c.m1.Get(ctx, desiredKey(system), &desired); err != nil {
		return nil, err
	}
	if desired.System != system.Metadata.Name || desired.ArtifactSHA != digest {
		return nil, fmt.Errorf("artifact is not current desired state")
	}
	prefix := strings.TrimRight(system.Spec.M1.Prefix, "/") + "/artifacts/"
	if !strings.HasPrefix(desired.ArtifactKey, prefix) && desired.ArtifactKey != ownedKey {
		return nil, fmt.Errorf("artifact key is outside system capability")
	}
	b, err := c.m1.GetBytes(ctx, desired.ArtifactKey)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("artifact digest mismatch")
	}
	return b, nil
}

func (c *Client) PutNodeObserved(ctx context.Context, system System, observed ObservedRelease) error {
	if observed.SchemaVersion != "v1" || observed.System != system.Metadata.Name {
		return fmt.Errorf("invalid observed state")
	}
	return c.m1.PutJSON(ctx, observedKey(system), observed)
}

func (c *Client) PutNodeControlAck(ctx context.Context, system System, controlID string, completedAt time.Time) error {
	var current RuntimeControl
	if err := c.m1.Get(ctx, controlKey(system), &current); err != nil {
		return err
	}
	if current.ID != controlID {
		return fmt.Errorf("control is not current")
	}
	return c.m1.PutJSON(ctx, strings.TrimRight(system.Spec.M1.Prefix, "/")+"/control-ack.json", map[string]any{"id": current.ID, "action": current.Action, "completedAt": completedAt})
}

func (c *Client) PutNodeRuntimeActionResult(ctx context.Context, system System, result RuntimeActionResult) error {
	var current RuntimeAction
	if err := c.m1.Get(ctx, runtimeActionRequestKey(system), &current); err != nil {
		return err
	}
	if result.SchemaVersion != "v1" || result.System != system.Metadata.Name || result.ID != current.ID {
		return fmt.Errorf("runtime result does not match current action")
	}
	prefix := strings.TrimRight(system.Spec.M1.Prefix, "/")
	if !strings.HasPrefix(current.LeaseKey, prefix+"/changes/") {
		return fmt.Errorf("runtime action lease is outside system capability")
	}
	var lease ChangeLease
	if err := c.m1.Get(ctx, current.LeaseKey, &lease); err != nil {
		return err
	}
	if lease.FencingToken != current.FencingToken || !lease.ExpiresAt.After(time.Now().UTC()) || !strings.HasPrefix(current.ID, lease.ChangeID+"-") {
		return fmt.Errorf("runtime action lease is no longer valid")
	}
	return c.m1.PutJSON(ctx, runtimeActionResultKey(system, result.ID), result)
}
