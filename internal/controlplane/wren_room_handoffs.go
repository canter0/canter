package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

type roomHandoffSource struct {
	Parent   OperatorRun
	TargetID string
	From     *Wren
	TurnID   string
}

// The conversation lock in enqueueOperatorWithSource serializes handoffs with
// human sends, participant changes and request cancellation.
func (s *Store) validateRoomHandoffTx(ctx context.Context, tx pgx.Tx, c Conversation, source *roomHandoffSource) error {
	var kind, requester string
	err := tx.QueryRow(ctx, `SELECT r.speaker,r.turn_id,r.origin_kind,COALESCE(r.requester_account_id,'') FROM operator_runs r JOIN wren_rooms room ON room.conversation_id=r.conversation_id WHERE r.id=$1 AND r.conversation_id=$2 AND room.workspace_id=$3 AND r.status='running' AND r.lease_token=$4 AND r.lease_expires_at>now() FOR UPDATE OF r`, source.Parent.ID, c.ID, c.WorkspaceID, source.Parent.Lease).Scan(&source.From, &source.TurnID, &kind, &requester)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if requester != c.AccountID || source.From == nil || source.TurnID == "" || (kind != "human" && kind != "handoff") {
		return ErrForbidden
	}
	if source.TargetID == source.From.ID {
		return fmt.Errorf("choose another Wren in this room")
	}
	if err = validateWrenLifecycleTx(ctx, tx, c.ID, source.From.ID); err != nil {
		return err
	}
	var fromPresent, humanRoot, assigned bool
	var count int
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_room_wrens rw JOIN wren_rooms room ON room.id=rw.room_id WHERE room.conversation_id=$1 AND rw.wren_id=$2),EXISTS(SELECT 1 FROM operator_runs WHERE id=$3 AND conversation_id=$1 AND origin_kind='human' AND requester_account_id=$4),(SELECT count(*) FROM operator_runs WHERE conversation_id=$1 AND turn_id=$3),EXISTS(SELECT 1 FROM operator_runs WHERE conversation_id=$1 AND turn_id=$3 AND speaker->>'id'=$5)`, c.ID, source.From.ID, source.TurnID, c.AccountID, source.TargetID).Scan(&fromPresent, &humanRoot, &count, &assigned)
	if err != nil {
		return err
	}
	if !fromPresent || !humanRoot {
		return ErrForbidden
	}
	if assigned {
		return fmt.Errorf("that Wren already has a contribution in this request; do not duplicate it")
	}
	if count >= 4 {
		return fmt.Errorf("this request already has four Wren contributions; ask the person before starting another request")
	}
	return nil
}

func (s *Store) AskRoomWren(ctx context.Context, parent OperatorRun, c Conversation, operationKey, target, prompt, model string) (OperatorRun, error) {
	if len(operationKey) == 0 || len(operationKey) > 200 || !strings.HasPrefix(target, "wren_") {
		return OperatorRun{}, fmt.Errorf("choose a room Wren and a valid operation key")
	}
	request := fmt.Sprintf("handoff_%x", sha256.Sum256([]byte(operationKey)))
	return s.enqueueOperatorWithSource(ctx, c, request, prompt, model, OperatorModelOptions{}, nil, []string{target}, false, "", &roomHandoffSource{Parent: parent, TargetID: target})
}

func (o *OperatorRuntime) askRoomWrenTool(ctx context.Context, run OperatorRun, c Conversation, raw json.RawMessage) (any, error) {
	var input struct {
		WrenID       string `json:"wrenId"`
		Prompt       string `json:"prompt"`
		OperationKey string `json:"operationKey"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	child, err := o.Server.service.Store.AskRoomWren(ctx, run, c, input.OperationKey, input.WrenID, input.Prompt, o.Config.wrenModel())
	if err != nil {
		return nil, err
	}
	return map[string]any{"runId": child.ID, "wren": child.Speaker.Name, "status": child.Status, "next": "The handoff is saved in this room. Finish your contribution; the teammate starts after earlier contributions in this request finish and posts its own answer here. Do not poll, simulate its answer, or claim queued work completed."}, nil
}
