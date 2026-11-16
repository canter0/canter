package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
)

// Raw tool evidence and virtual scratch are private to the Wren/requester
// pair. Room messages, saved memory and published artifacts are shared instead.
// Both aliases must be persisted runs; never derive this scope from tool input.
const operatorEvidenceScope = `(r.speaker->>'id') IS NOT DISTINCT FROM (current.speaker->>'id')
    AND r.requester_account_id IS NOT DISTINCT FROM current.requester_account_id`

func (s *Store) operatorWorkingData(ctx context.Context, run OperatorRun) (json.RawMessage, map[string]string, error) {
	var notes json.RawMessage
	scratch := map[string]string{}
	err := s.pool.QueryRow(ctx, `SELECT
        CASE WHEN COALESCE(r.speaker->>'id','')='' THEN COALESCE(legacy.notes,'{}') ELSE COALESCE(room.notes,'{}') END,
        CASE WHEN COALESCE(r.speaker->>'id','')='' THEN COALESCE(legacy.scratch,'{}') ELSE COALESCE(room.scratch,'{}') END
        FROM operator_runs r
        LEFT JOIN operator_working_context legacy ON legacy.conversation_id=r.conversation_id
        LEFT JOIN operator_room_working_context room ON room.conversation_id=r.conversation_id
            AND room.speaker_id=r.speaker->>'id' AND room.requester_account_id=r.requester_account_id
        WHERE r.id=$1 AND r.conversation_id=$2`, run.ID, run.ConversationID).Scan(&notes, &scratch)
	return notes, scratch, err
}

func (s *Store) saveOperatorWorkingData(ctx context.Context, run OperatorRun, field string, value any) error {
	if field != "notes" && field != "scratch" {
		return fmt.Errorf("invalid working context field")
	}
	// The field is an internal allowlisted SQL identifier. Row identity comes
	// exclusively from the live, fenced run, including on lease recovery.
	var saved int
	err := s.pool.QueryRow(ctx, `WITH live AS (
        SELECT conversation_id,speaker->>'id' AS speaker_id,requester_account_id FROM operator_runs
        WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now()
    ), private_saved AS (
        INSERT INTO operator_working_context(conversation_id,`+field+`)
        SELECT conversation_id,$3 FROM live WHERE COALESCE(speaker_id,'')=''
        ON CONFLICT(conversation_id) DO UPDATE SET `+field+`=EXCLUDED.`+field+`,updated_at=now() RETURNING 1
    ), room_saved AS (
        INSERT INTO operator_room_working_context(conversation_id,speaker_id,requester_account_id,`+field+`)
        SELECT conversation_id,speaker_id,requester_account_id,$3 FROM live WHERE COALESCE(speaker_id,'')<>''
        ON CONFLICT(conversation_id,speaker_id,requester_account_id) DO UPDATE SET `+field+`=EXCLUDED.`+field+`,updated_at=now() RETURNING 1
    ) SELECT (SELECT count(*) FROM private_saved)+(SELECT count(*) FROM room_saved)`, run.ID, run.Lease, value).Scan(&saved)
	if err == nil && saved != 1 {
		return ErrConflict
	}
	return err
}
