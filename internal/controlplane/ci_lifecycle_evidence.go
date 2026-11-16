package controlplane

import (
	"fmt"
	"regexp"
)

var ciLifecycleID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type ciOwnedResource struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	OperationID string `json:"operationId"`
}

type ciResourceRead struct {
	Resource    ciOwnedResource
	HTTPStatus  int
	ServerState string
}

type ciServerPorts struct {
	ServerID string
	Complete bool
	PortIDs  []string
}

// Observations must be built by a trusted provider adapter from fresh direct
// reads and complete discovery, never from a guest receipt or DELETE response.
type ciResourceObservation struct {
	OperationID string
	// Retained comes from an authenticated creator's durable ownership
	// inventory, not a guest hint or a new discovery page.
	Retained          []ciOwnedResource
	Discovered        []ciOwnedResource
	Reads             []ciResourceRead
	ServerPorts       []ciServerPorts
	DiscoveryComplete bool
	CreateSettled     bool
	EnforcerSettled   bool
}

func ciResourceKey(r ciOwnedResource) string { return r.Kind + "/" + r.ID }

func ciResourceValid(r ciOwnedResource, operation string) bool {
	return (r.Kind == "server" || r.Kind == "port" || r.Kind == "security_group") &&
		ciLifecycleID.MatchString(r.ID) && r.OperationID == operation
}

// ciResourceEvidence retains verified IDs even when a discovery page is
// incomplete. Incomplete discovery can never establish stop or deletion.
func ciResourceEvidence(operation string, retained []ciOwnedResource, obs ciResourceObservation) ([]ciOwnedResource, bool, bool, error) {
	if !ciLifecycleID.MatchString(operation) || obs.OperationID != operation {
		return nil, false, false, ErrConflict
	}
	known := make(map[string]ciOwnedResource)
	combined := make([]ciOwnedResource, 0, len(retained)+len(obs.Retained)+len(obs.Discovered))
	for _, list := range [][]ciOwnedResource{retained, obs.Retained, obs.Discovered} {
		seen := make(map[string]bool)
		for _, r := range list {
			key := ciResourceKey(r)
			if !ciResourceValid(r, operation) || seen[key] {
				return nil, false, false, ErrConflict
			}
			seen[key] = true
			if _, exists := known[key]; !exists {
				known[key] = r
				combined = append(combined, r)
			}
		}
	}
	reads := make(map[string]ciResourceRead)
	for _, read := range obs.Reads {
		if !ciResourceValid(read.Resource, operation) {
			return nil, false, false, ErrConflict
		}
		key := ciResourceKey(read.Resource)
		if _, exists := known[key]; !exists {
			return nil, false, false, ErrConflict
		}
		if _, duplicate := reads[key]; duplicate ||
			(read.HTTPStatus != 200 && read.HTTPStatus != 404) {
			return nil, false, false, ErrConflict
		}
		if read.HTTPStatus == 200 && !ciResourceValid(read.Resource, operation) {
			return nil, false, false, fmt.Errorf("%w: resource ownership changed", ErrConflict)
		}
		reads[key] = read
	}
	for _, r := range obs.Retained {
		if _, exists := reads[ciResourceKey(r)]; !exists {
			return nil, false, false, ErrConflict
		}
	}
	for _, r := range obs.Discovered {
		read, exists := reads[ciResourceKey(r)]
		if !exists || read.HTTPStatus != 200 {
			return nil, false, false, ErrConflict
		}
	}
	stopped := obs.DiscoveryComplete && obs.CreateSettled
	absent := stopped && obs.EnforcerSettled && len(obs.Discovered) == 0
	servers := make(map[string]bool)
	for _, r := range combined {
		read, exists := reads[ciResourceKey(r)]
		if !exists {
			stopped, absent = false, false
			continue
		}
		if read.HTTPStatus != 404 {
			absent = false
		}
		if r.Kind == "server" {
			servers[r.ID] = true
			if read.HTTPStatus != 404 && read.ServerState != "SHUTOFF" {
				stopped = false
			}
		}
	}
	portLists := make(map[string]bool)
	for _, ports := range obs.ServerPorts {
		if !servers[ports.ServerID] || portLists[ports.ServerID] {
			return nil, false, false, ErrConflict
		}
		portLists[ports.ServerID] = true
		if !ports.Complete || ports.PortIDs == nil || len(ports.PortIDs) != 0 {
			absent = false
		}
	}
	for id := range servers {
		if !portLists[id] {
			absent = false
		}
	}
	return combined, stopped, absent && stopped, nil
}
