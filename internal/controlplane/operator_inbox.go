package controlplane

import "context"

func (s *Store) operatorHasFollowup(ctx context.Context, run OperatorRun) (bool, error) {
	var pending bool
	// A new room message is not necessarily a correction. Only a later human
	// request from the same person to the same Wren can supersede this work.
	// Another member, another Wren, or an automated wake keeps its own turn.
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
        SELECT 1 FROM operator_runs next JOIN operator_runs current ON current.id=$2
        WHERE next.conversation_id=$1 AND next.status='queued'
        AND (next.created_at,next.id)>(current.created_at,current.id)
        AND (next.turn_id='' OR next.turn_id<>current.turn_id)
        AND next.origin_kind='human' AND current.origin_kind='human'
        AND next.requester_account_id IS NOT DISTINCT FROM current.requester_account_id
        AND (next.speaker->>'id') IS NOT DISTINCT FROM (current.speaker->>'id')
    )`, run.ConversationID, run.ID).Scan(&pending)
	return pending, err
}

func (s *Store) yieldOperator(ctx context.Context, run OperatorRun) error {
	return s.finishOperator(ctx, run, "completed", "I’ll continue with your latest message. Any operation already submitted keeps its own status.")
}
