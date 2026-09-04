package sdk

import "time"

type RuntimeAction struct {
	SchemaVersion string            `json:"schemaVersion"`
	ID            string            `json:"id"`
	System        string            `json:"system"`
	Service       string            `json:"service"`
	Kind          string            `json:"kind"`
	Parameters    map[string]string `json:"parameters"`
	LeaseKey      string            `json:"leaseKey"`
	FencingToken  string            `json:"fencingToken"`
	RequestedAt   time.Time         `json:"requestedAt"`
}

type RuntimeActionResult struct {
	SchemaVersion string    `json:"schemaVersion"`
	ID            string    `json:"id"`
	System        string    `json:"system"`
	Service       string    `json:"service"`
	Kind          string    `json:"kind"`
	Phase         string    `json:"phase"`
	Message       string    `json:"message,omitempty"`
	Duplicate     bool      `json:"duplicate,omitempty"`
	CompletedAt   time.Time `json:"completedAt"`
}

type ChangeLease struct {
	SchemaVersion string    `json:"schemaVersion"`
	ChangeID      string    `json:"changeId"`
	Holder        string    `json:"holder"`
	FencingToken  string    `json:"fencingToken"`
	ExpiresAt     time.Time `json:"expiresAt"`
}
