package controlplane

import "context"

func (s *Store) operatorHasFollowup(ctx context.Context, run OperatorRun) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_runs WHERE conversation_id=$1 AND id<>$2 AND status='queued')`, run.ConversationID, run.ID).Scan(&pending)
	return pending, err
}

func (s *Store) yieldOperator(ctx context.Context, run OperatorRun) error {
	return s.finishOperator(ctx, run, "completed", "I’ll continue with your latest message. Any operation already submitted keeps its own status.")
}
