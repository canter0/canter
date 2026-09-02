package controlplane

import (
	"encoding/json"
	"time"

	"github.com/canter0/canter/sdk"
)

type Account struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"createdAt"`
}

type Workspace struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
	Role     string `json:"role,omitempty"`
}

type Authority struct {
	Inspect   bool   `json:"inspect"`
	Draft     bool   `json:"draft"`
	ApplyMode string `json:"applyMode"`
}

type Installation struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspaceId"`
	Name        string     `json:"name"`
	Harness     string     `json:"harness"`
	Authority   Authority  `json:"authority"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastSeenAt  *time.Time `json:"lastSeenAt,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

type AgentSession struct {
	ID             string     `json:"id"`
	InstallationID string     `json:"installationId"`
	ClientInstance string     `json:"clientInstance,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastSeenAt     time.Time  `json:"lastSeenAt"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	EndedAt        *time.Time `json:"endedAt,omitempty"`
}

type DeviceAuthorization struct {
	DeviceCode      string    `json:"deviceCode,omitempty"`
	UserCode        string    `json:"userCode"`
	VerificationURI string    `json:"verificationUri"`
	ExpiresAt       time.Time `json:"expiresAt"`
	IntervalSeconds int       `json:"intervalSeconds"`
}

type TokenPair struct {
	AccessToken  string       `json:"accessToken"`
	TokenType    string       `json:"tokenType"`
	ExpiresIn    int          `json:"expiresIn"`
	RefreshToken string       `json:"refreshToken"`
	Installation Installation `json:"installation"`
	Session      AgentSession `json:"session"`
}

type SystemRecord struct {
	WorkspaceID string     `json:"workspaceId"`
	Revision    int64      `json:"revision"`
	Contract    sdk.System `json:"contract"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

type Bootstrap struct {
	ProtocolVersion    string                   `json:"protocolVersion"`
	Installation       Installation             `json:"installation"`
	Session            AgentSession             `json:"session"`
	Workspace          Workspace                `json:"workspace"`
	Systems            []SystemRecord           `json:"systems"`
	Changes            []ChangeIndex            `json:"changes"`
	PendingChanges     []ChangeIndex            `json:"pendingChanges"`
	InitialDeployments []InitialDeploymentIndex `json:"initialDeployments"`
	Capabilities       map[string]any           `json:"capabilities"`
	Incidents          []any                    `json:"incidents"`
}

// SystemView is the provider-neutral read model exposed through HTTP and MCP.
// Raw SDK host state remains available only inside the execution engine.
type SystemView struct {
	SchemaVersion       string                          `json:"schemaVersion"`
	Contract            sdk.System                      `json:"contract"`
	Graph               sdk.ExecutionGraph              `json:"graph"`
	Bindings            []sdk.ServiceBinding            `json:"bindings"`
	Host                *HostObservation                `json:"host,omitempty"`
	Release             *sdk.ReleaseView                `json:"release,omitempty"`
	ApplicationCapacity *ApplicationCapacityObservation `json:"applicationCapacity,omitempty"`
	Issues              []string                        `json:"issues,omitempty"`
}

type ApplicationCapacityObservation struct {
	Service          string `json:"service"`
	Mode             string `json:"mode"`
	DeclaredBaseline int    `json:"declaredBaseline"`
	MaximumReplicas  int    `json:"maximumReplicas"`
	DesiredReplicas  int    `json:"desiredReplicas"`
	ReadyReplicas    int    `json:"readyReplicas"`
}

type HostObservation struct {
	Phase            string                       `json:"phase"`
	Class            string                       `json:"class"`
	Count            int                          `json:"count"`
	Resources        []ComputeResourceObservation `json:"resources,omitempty"`
	Exposure         *ExposureObservation         `json:"exposure,omitempty"`
	RequiresOperator bool                         `json:"requiresOperator,omitempty"`
}

type ComputeResourceObservation struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type ExposureObservation struct {
	Phase              string `json:"phase"`
	Protocol           string `json:"protocol"`
	Port               int    `json:"port"`
	Managed            bool   `json:"managed"`
	MutationUnresolved bool   `json:"mutationUnresolved,omitempty"`
}

type ChangeIndex struct {
	ID             string `json:"id"`
	System         string `json:"system"`
	Phase          string `json:"phase"`
	Summary        string `json:"summary"`
	Digest         string `json:"digest"`
	ExecutionID    string `json:"executionId,omitempty"`
	ExecutionPhase string `json:"executionPhase,omitempty"`
}

// ChangeInspection keeps the provider-neutral Change document compatible with
// existing clients while attaching the durable control-plane execution that
// actually carried it out. The execution is absent until a human-authorized
// apply has been enqueued.
type ChangeInspection struct {
	sdk.Change
	Execution *Execution `json:"execution,omitempty"`
}

type StandingPolicyEnvelope struct {
	AllowedInstallationIDs        []string                `json:"allowedInstallationIds"`
	AffectedServices              []string                `json:"affectedServices"`
	OperationKinds                []string                `json:"operationKinds"`
	Availability                  []string                `json:"availability"`
	Data                          []string                `json:"data"`
	AllowedReversibility          []string                `json:"allowedReversibility"`
	MaxAdditionalMonthlyCostCents int64                   `json:"maxAdditionalMonthlyCostCents"`
	MaxOperations                 int                     `json:"maxOperations"`
	ScaleLimits                   map[string]ReplicaRange `json:"scaleLimits,omitempty"`
	MaxScaleDurationSeconds       int                     `json:"maxScaleDurationSeconds,omitempty"`
	AllowPermanentScale           bool                    `json:"allowPermanentScale,omitempty"`
}

type ReplicaRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type CreateStandingPolicyInput struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Envelope    StandingPolicyEnvelope `json:"envelope"`
	ExpiresAt   time.Time              `json:"expiresAt"`
}

type StandingPolicy struct {
	ID                string                 `json:"id"`
	WorkspaceID       string                 `json:"workspaceId"`
	System            string                 `json:"system"`
	Name              string                 `json:"name"`
	Description       string                 `json:"description"`
	Digest            string                 `json:"digest"`
	Envelope          StandingPolicyEnvelope `json:"envelope"`
	WorkspaceRevision int64                  `json:"workspaceRevision"`
	SystemRevision    int64                  `json:"systemRevision"`
	CreatedByAccount  string                 `json:"createdByAccount"`
	CreatedAt         time.Time              `json:"createdAt"`
	ExpiresAt         time.Time              `json:"expiresAt"`
	RevokedAt         *time.Time             `json:"revokedAt,omitempty"`
	RevokedByAccount  string                 `json:"revokedByAccount,omitempty"`
}

type PolicyDecision struct {
	ID                      string    `json:"id"`
	WorkspaceID             string    `json:"workspaceId"`
	System                  string    `json:"system"`
	ChangeID                string    `json:"changeId"`
	ChangeDigest            string    `json:"changeDigest"`
	Outcome                 string    `json:"outcome"`
	Phase                   string    `json:"phase"`
	PolicyID                string    `json:"policyId,omitempty"`
	PolicyDigest            string    `json:"policyDigest,omitempty"`
	EvaluatedByInstallation string    `json:"evaluatedByInstallation"`
	Reason                  string    `json:"reason"`
	ExecutionID             string    `json:"executionId,omitempty"`
	Failure                 string    `json:"failure,omitempty"`
	CreatedAt               time.Time `json:"createdAt"`
	UpdatedAt               time.Time `json:"updatedAt"`
}

type PolicyApplyResult struct {
	Decision  PolicyDecision  `json:"decision"`
	Policy    *StandingPolicy `json:"policy,omitempty"`
	Change    sdk.Change      `json:"change"`
	Execution *Execution      `json:"execution,omitempty"`
}

// ChangeApprovalCapability is a short-lived, human-gated route to one exact
// immutable Change. ReviewURL is returned only when the capability is created;
// the durable record stores a hash of its bearer token.
type ChangeApprovalCapability struct {
	ID          string       `json:"id"`
	WorkspaceID string       `json:"workspaceId"`
	System      string       `json:"system"`
	ChangeID    string       `json:"changeId"`
	Digest      string       `json:"digest"`
	Action      string       `json:"action"`
	RequestedBy Installation `json:"requestedBy"`
	CreatedAt   time.Time    `json:"createdAt"`
	ExpiresAt   time.Time    `json:"expiresAt"`
	ConsumedAt  *time.Time   `json:"consumedAt,omitempty"`
	ConsumedBy  string       `json:"consumedBy,omitempty"`
	ExecutionID string       `json:"executionId,omitempty"`
	ReviewURL   string       `json:"reviewUrl,omitempty"`
}

type ChangeApprovalReview struct {
	Capability ChangeApprovalCapability `json:"capability"`
	Change     sdk.Change               `json:"change"`
	CanApprove bool                     `json:"canApprove"`
}

type ChangeApprovalResult struct {
	Capability ChangeApprovalCapability `json:"capability"`
	Change     sdk.Change               `json:"change"`
	Execution  Execution                `json:"execution"`
}

type Execution struct {
	ID             string       `json:"id"`
	WorkspaceID    string       `json:"workspaceId"`
	SystemName     string       `json:"system"`
	ChangeID       string       `json:"changeId"`
	Phase          string       `json:"phase"`
	RequestedBy    sdk.ActorRef `json:"requestedBy"`
	Attempts       int          `json:"attempts"`
	AvailableAt    time.Time    `json:"availableAt"`
	ClaimedBy      string       `json:"claimedBy,omitempty"`
	LeaseExpiresAt *time.Time   `json:"leaseExpiresAt,omitempty"`
	Failure        string       `json:"failure,omitempty"`
	CreatedAt      time.Time    `json:"createdAt"`
	StartedAt      *time.Time   `json:"startedAt,omitempty"`
	CompletedAt    *time.Time   `json:"completedAt,omitempty"`
}

type Principal struct {
	Actor        sdk.ActorRef
	Account      *Account
	Installation *Installation
	Session      *AgentSession
	WorkspaceID  string
	Role         string
}

type NodeInstallation struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspaceId"`
	System      string     `json:"system"`
	M1Prefix    string     `json:"-"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastSeenAt  *time.Time `json:"lastSeenAt,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

type NodeEnrollment struct {
	ID              string           `json:"id"`
	EnrollmentToken string           `json:"enrollmentToken,omitempty"`
	ExpiresAt       time.Time        `json:"expiresAt"`
	Node            NodeInstallation `json:"node"`
}

type NodeCredential struct {
	NodeToken string           `json:"nodeToken,omitempty"`
	ExpiresAt time.Time        `json:"expiresAt"`
	Node      NodeInstallation `json:"node"`
}

type AuditEvent struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspaceId"`
	Actor       sdk.ActorRef    `json:"actor"`
	Action      string          `json:"action"`
	Subject     string          `json:"subject"`
	Metadata    json.RawMessage `json:"metadata"`
	OccurredAt  time.Time       `json:"occurredAt"`
}

// DeploymentArtifact is the agent-visible record for an immutable application
// bundle. The m1 object key is intentionally kept in the Store and never
// serialized through HTTP or MCP.
type DeploymentArtifact struct {
	WorkspaceID string                    `json:"workspaceId"`
	SHA256      string                    `json:"sha256"`
	Size        int64                     `json:"size"`
	ContentType string                    `json:"contentType"`
	Filename    string                    `json:"filename,omitempty"`
	Entries     []DeploymentArtifactEntry `json:"entries"`
	UploadedBy  sdk.ActorRef              `json:"uploadedBy"`
	CreatedAt   time.Time                 `json:"createdAt"`
}

type DeploymentArtifactEntry struct {
	Path string `json:"path"`
	Mode int64  `json:"mode"`
	Size int64  `json:"size"`
}

type InitialDeploymentRelease struct {
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	HealthPath  string            `json:"healthPath"`
	PublicPort  int               `json:"publicPort"`
}

type InitialDeploymentPlan struct {
	System            sdk.System               `json:"system"`
	ArtifactSHA256    string                   `json:"artifactSha256"`
	Release           InitialDeploymentRelease `json:"release"`
	Verification      sdk.ChangeVerification   `json:"verification"`
	WorkspaceRevision int64                    `json:"workspaceRevision"`
}

type InitialDeploymentOperation struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Description string     `json:"description"`
	Phase       string     `json:"phase"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Failure     string     `json:"failure,omitempty"`
}

type InitialDeployment struct {
	SchemaVersion string                       `json:"schemaVersion"`
	ID            string                       `json:"id"`
	WorkspaceID   string                       `json:"workspaceId"`
	System        string                       `json:"system"`
	Summary       string                       `json:"summary"`
	Phase         string                       `json:"phase"`
	Digest        string                       `json:"digest"`
	DraftedBy     sdk.ActorRef                 `json:"draftedBy"`
	Authorization *sdk.Authorization           `json:"authorization,omitempty"`
	Plan          InitialDeploymentPlan        `json:"plan"`
	Operations    []InitialDeploymentOperation `json:"operations"`
	Evidence      []sdk.ChangeEvidence         `json:"evidence,omitempty"`
	Failure       string                       `json:"failure,omitempty"`
	CreatedAt     time.Time                    `json:"createdAt"`
	UpdatedAt     time.Time                    `json:"updatedAt"`
	CompletedAt   *time.Time                   `json:"completedAt,omitempty"`
}

type InitialDeploymentIndex struct {
	ID      string `json:"id"`
	System  string `json:"system"`
	Phase   string `json:"phase"`
	Summary string `json:"summary"`
	Digest  string `json:"digest"`
}

type InitialDeploymentExecution struct {
	ID             string       `json:"id"`
	WorkspaceID    string       `json:"workspaceId"`
	DeploymentID   string       `json:"deploymentId"`
	SystemName     string       `json:"system"`
	Phase          string       `json:"phase"`
	RequestedBy    sdk.ActorRef `json:"requestedBy"`
	Attempts       int          `json:"attempts"`
	AvailableAt    time.Time    `json:"availableAt"`
	ClaimedBy      string       `json:"claimedBy,omitempty"`
	ClaimToken     string       `json:"-"`
	LeaseExpiresAt *time.Time   `json:"leaseExpiresAt,omitempty"`
	Failure        string       `json:"failure,omitempty"`
	CreatedAt      time.Time    `json:"createdAt"`
	StartedAt      *time.Time   `json:"startedAt,omitempty"`
	CompletedAt    *time.Time   `json:"completedAt,omitempty"`
}
