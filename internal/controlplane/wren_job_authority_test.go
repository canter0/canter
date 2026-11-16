package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWrenJobSteeringSerializesAgainstOtherMemberDowngrade(t *testing.T) {
	s, _, _, shared, member := knowledgeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := s.EnqueueOperator(ctx, shared, "owner-job", "Prepare the team outline", "test", nil); err != nil {
		t.Fatal(err)
	}
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	job := requireWrenJob(t, s, shared, parent, "owner-worker")
	// The job belongs to A; downgrading B does not cancel A's worker and cannot
	// accidentally mask a stale writer authorization in the steering path.
	if job.RequesterAccountID == member {
		t.Fatal("test requires a different requester")
	}
	revoke, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer revoke.Rollback(context.Background())
	var id string
	if err = revoke.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, shared.WorkspaceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := s.SteerWrenJob(ctx, shared.WorkspaceID, member, job.ID, "role-change-correction", "Change the outline")
		done <- e
	}()
	deadline := time.Now().Add(2 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM workspaces WHERE id=$1 FOR KEY SHARE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("steering did not wait for workspace authority serialization")
	}
	if _, err = revoke.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, shared.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	if err = revoke.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrForbidden) {
		t.Fatal("downgraded member corrected another member's worker", err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_job_inputs WHERE job_id=$1`, job.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("correction committed after downgrade", count, err)
	}
}
