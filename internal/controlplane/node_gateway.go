package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
)

type NodeGatewayEngine interface {
	NodeSnapshot(context.Context, sdk.System) (sdk.NodeSnapshot, error)
	NodeArtifact(context.Context, sdk.System, string, string) ([]byte, error)
	PutNodeObserved(context.Context, sdk.System, sdk.ObservedRelease) error
	PutNodeControlAck(context.Context, sdk.System, string, time.Time) error
	PutNodeRuntimeActionResult(context.Context, sdk.System, sdk.RuntimeActionResult) error
}

func (s *Service) CreateNodeEnrollment(ctx context.Context, workspaceID, systemName string) (NodeEnrollment, error) {
	record, err := s.Store.GetSystem(ctx, workspaceID, systemName)
	if err != nil {
		return NodeEnrollment{}, err
	}
	prefix := strings.TrimRight(record.Contract.Spec.M1.Prefix, "/")
	if prefix == "" {
		return NodeEnrollment{}, fmt.Errorf("system has no durable state prefix")
	}
	return s.Store.CreateNodeEnrollment(ctx, workspaceID, systemName, prefix)
}

// PrepareNodeBootstrap is the dispatcher boundary: it creates the scoped,
// one-time enrollment and returns exactly the SDK input needed for cloud-init.
func (s *Service) PrepareNodeBootstrap(ctx context.Context, workspaceID, systemName string) (NodeEnrollment, sdk.NodeBootstrapConfig, error) {
	gatewayURL := strings.TrimSpace(s.NodeGatewayURL)
	parsed, err := url.Parse(gatewayURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return NodeEnrollment{}, sdk.NodeBootstrapConfig{}, fmt.Errorf("node gateway URL must be an absolute HTTPS URL")
	}
	enrollment, err := s.CreateNodeEnrollment(ctx, workspaceID, systemName)
	if err != nil {
		return NodeEnrollment{}, sdk.NodeBootstrapConfig{}, err
	}
	config := sdk.NodeBootstrapConfig{GatewayURL: strings.TrimRight(gatewayURL, "/"), EnrollmentID: enrollment.ID, EnrollmentToken: enrollment.EnrollmentToken}
	return enrollment, config, nil
}

func (s *Service) nodeSystem(ctx context.Context, node NodeInstallation) (sdk.System, error) {
	record, err := s.Store.GetSystem(ctx, node.WorkspaceID, node.System)
	if err != nil {
		return sdk.System{}, err
	}
	if strings.TrimRight(record.Contract.Spec.M1.Prefix, "/") != node.M1Prefix {
		return sdk.System{}, fmt.Errorf("%w: node scope no longer matches system", ErrForbidden)
	}
	return record.Contract, nil
}

func (s *Service) NodeSnapshot(ctx context.Context, node NodeInstallation) (sdk.NodeSnapshot, error) {
	if s.NodeGateway == nil {
		return sdk.NodeSnapshot{}, fmt.Errorf("node gateway engine is unavailable")
	}
	system, err := s.nodeSystem(ctx, node)
	if err != nil {
		return sdk.NodeSnapshot{}, err
	}
	return s.NodeGateway.NodeSnapshot(ctx, system)
}

func (s *Service) NodeArtifact(ctx context.Context, node NodeInstallation, digest string) ([]byte, error) {
	if s.NodeGateway == nil {
		return nil, fmt.Errorf("node gateway engine is unavailable")
	}
	if len(digest) != sha256.Size*2 {
		return nil, fmt.Errorf("invalid artifact digest")
	}
	if _, err := hex.DecodeString(digest); err != nil || digest != strings.ToLower(digest) {
		return nil, fmt.Errorf("invalid artifact digest")
	}
	system, err := s.nodeSystem(ctx, node)
	if err != nil {
		return nil, err
	}
	ownedKey := ""
	if _, staged, lookupErr := s.Store.DeploymentArtifact(ctx, node.WorkspaceID, digest); lookupErr == nil {
		ownedKey = staged.Key
	} else if !errors.Is(lookupErr, ErrNotFound) {
		return nil, lookupErr
	}
	return s.NodeGateway.NodeArtifact(ctx, system, digest, ownedKey)
}

func (s *Service) PutNodeObserved(ctx context.Context, node NodeInstallation, observed sdk.ObservedRelease) error {
	if s.NodeGateway == nil {
		return fmt.Errorf("node gateway engine is unavailable")
	}
	system, err := s.nodeSystem(ctx, node)
	if err != nil {
		return err
	}
	if observed.System != node.System || observed.SchemaVersion != "v1" {
		return fmt.Errorf("invalid observed state")
	}
	return s.NodeGateway.PutNodeObserved(ctx, system, observed)
}

func (s *Service) AckNodeControl(ctx context.Context, node NodeInstallation, id string) error {
	if s.NodeGateway == nil {
		return fmt.Errorf("node gateway engine is unavailable")
	}
	system, err := s.nodeSystem(ctx, node)
	if err != nil {
		return err
	}
	snapshot, err := s.NodeGateway.NodeSnapshot(ctx, system)
	if err != nil {
		return err
	}
	if snapshot.Control == nil || snapshot.Control.ID != id {
		return fmt.Errorf("%w: control is not current", ErrConflict)
	}
	return s.NodeGateway.PutNodeControlAck(ctx, system, id, time.Now().UTC())
}

func (s *Service) PutNodeRuntimeResult(ctx context.Context, node NodeInstallation, actionID string, result sdk.RuntimeActionResult) error {
	if s.NodeGateway == nil {
		return fmt.Errorf("node gateway engine is unavailable")
	}
	system, err := s.nodeSystem(ctx, node)
	if err != nil {
		return err
	}
	snapshot, err := s.NodeGateway.NodeSnapshot(ctx, system)
	if err != nil {
		return err
	}
	if snapshot.RuntimeAction == nil || snapshot.RuntimeAction.Action.ID != actionID || result.ID != actionID || !snapshot.RuntimeAction.Lease.ExpiresAt.After(time.Now().UTC()) {
		return fmt.Errorf("%w: runtime action is not currently authorized", ErrConflict)
	}
	return s.NodeGateway.PutNodeRuntimeActionResult(ctx, system, result)
}
