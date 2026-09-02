package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestNodeEnrollmentIsOneTimeHashedAndRevocable(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	_, workspace, _, err := store.Signup(ctx, "node-owner@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("node-api", "run an api").OnHost("c1", 1, 1024, 256).WithM1("ignored/client/prefix").Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).Build()
	if err != nil {
		t.Fatal(err)
	}
	system, err = canonicalizeSystemForWorkspace(workspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.PutSystem(ctx, workspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := store.CreateNodeEnrollment(ctx, workspace.ID, record.Contract.Metadata.Name, record.Contract.Spec.M1.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	var plaintextMatches int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM node_enrollments WHERE token_hash=$1::bytea`, []byte(enrollment.EnrollmentToken)).Scan(&plaintextMatches); err != nil {
		t.Fatal(err)
	}
	if plaintextMatches != 0 {
		t.Fatal("plaintext enrollment token was persisted")
	}
	retried, err := store.CreateNodeEnrollment(ctx, workspace.ID, record.Contract.Metadata.Name, record.Contract.Spec.M1.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Node.ID != enrollment.Node.ID || retried.ID != enrollment.ID || retried.EnrollmentToken == enrollment.EnrollmentToken {
		t.Fatalf("fenced retry did not reuse node/enrollment and rotate token: first=%#v retry=%#v", enrollment, retried)
	}
	var nodeCount, activeEnrollmentCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM node_installations WHERE workspace_id=$1 AND system_name=$2 AND revoked_at IS NULL`, workspace.ID, system.Metadata.Name).Scan(&nodeCount); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM node_enrollments WHERE node_id=$1 AND consumed_at IS NULL`, retried.Node.ID).Scan(&activeEnrollmentCount); err != nil {
		t.Fatal(err)
	}
	if nodeCount != 1 || activeEnrollmentCount != 1 {
		t.Fatalf("retry created duplicate durable state: nodes=%d activeEnrollments=%d", nodeCount, activeEnrollmentCount)
	}
	if _, err := store.ExchangeNodeEnrollment(ctx, enrollment.ID, enrollment.EnrollmentToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("rotated enrollment token remained valid: %v", err)
	}
	credential, err := store.ExchangeNodeEnrollment(ctx, retried.ID, retried.EnrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExchangeNodeEnrollment(ctx, retried.ID, retried.EnrollmentToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("enrollment replay err=%v", err)
	}
	node, err := store.ResolveNode(ctx, credential.NodeToken)
	if err != nil || node.WorkspaceID != workspace.ID || node.System != "node-api" || node.M1Prefix != record.Contract.Spec.M1.Prefix {
		t.Fatalf("node=%#v err=%v", node, err)
	}
	replacementEnrollment, err := store.CreateNodeEnrollment(ctx, workspace.ID, record.Contract.Metadata.Name, record.Contract.Spec.M1.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := store.ExchangeNodeEnrollment(ctx, replacementEnrollment.ID, replacementEnrollment.EnrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveNode(ctx, credential.NodeToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replacement left the previous node credential active: %v", err)
	}
	if _, err := store.ResolveNode(ctx, replacement.NodeToken); err != nil {
		t.Fatalf("replacement node credential was not active: %v", err)
	}
	if err := store.RevokeNodeInstallation(ctx, workspace.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveNode(ctx, replacement.NodeToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked credential err=%v", err)
	}
}
