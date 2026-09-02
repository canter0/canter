package sdk

import "context"

// ActorRef identifies the durable principal responsible for an action. SessionID
// is intentionally optional: installations and humans outlive individual
// conversations and browser sessions.
type ActorRef struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	SessionID   string `json:"sessionId,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

type actorContextKey struct{}
type baseRevisionContextKey struct{}

// ChangeBaseRevision binds a proposal to the semantic workspace and System
// state the drafting agent inspected. A zero value preserves standalone SDK
// compatibility for callers that do not use the control plane.
type ChangeBaseRevision struct {
	WorkspaceID       string `json:"workspaceId,omitempty"`
	WorkspaceRevision int64  `json:"workspaceRevision,omitempty"`
	SystemRevision    int64  `json:"systemRevision,omitempty"`
}

// WithActor attaches authenticated provenance to SDK operations without
// coupling the transport-neutral SDK to the control-plane authentication layer.
func WithActor(ctx context.Context, actor ActorRef) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func ActorFromContext(ctx context.Context) (ActorRef, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(ActorRef)
	return actor, ok
}

func WithChangeBaseRevision(ctx context.Context, revision ChangeBaseRevision) context.Context {
	return context.WithValue(ctx, baseRevisionContextKey{}, revision)
}

func ChangeBaseRevisionFromContext(ctx context.Context) (ChangeBaseRevision, bool) {
	revision, ok := ctx.Value(baseRevisionContextKey{}).(ChangeBaseRevision)
	return revision, ok
}
