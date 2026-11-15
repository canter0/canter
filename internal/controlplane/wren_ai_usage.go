package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"math"
	"time"
)

// All amounts are integer micro-USD, separate from compute allowances.
type WrenAIAllowance struct {
	ClosedAt      *time.Time `json:"closedAt,omitempty"`
	WorkspaceID   string     `json:"workspaceId"`
	LimitMicroUSD int64      `json:"limitMicroUsd"`
	HeldMicroUSD  int64      `json:"heldMicroUsd"`
	SpentMicroUSD int64      `json:"spentMicroUsd"`
}
type WrenAIReservationInput struct {
	WorkspaceID, AccountID, ConversationID, RunID, RequestKey, Kind, Model string
	Purpose                                                                string
	AmountMicroUSD                                                         int64
	Estimate                                                               json.RawMessage
	RunLease                                                               string
}
type WrenAIReservation struct {
	ID             string `json:"id"`
	WorkspaceID    string `json:"workspaceId"`
	State          string `json:"state"`
	AmountMicroUSD int64  `json:"amountMicroUsd"`
	ActualMicroUSD *int64 `json:"actualMicroUsd,omitempty"`
}

// ConfigureWrenAIAllowance is an operator-only primitive, never a model tool.
// Changing a limit does not reset accumulated spending or outstanding holds.
func (s *Store) ConfigureWrenAIAllowance(ctx context.Context, workspace string, limit int64) error {
	if workspace == "" || limit < 0 || limit > 1_000_000_000_000 {
		return fmt.Errorf("invalid AI allowance")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO wren_ai_allowances(workspace_id,limit_micro_usd)VALUES($1,$2) ON CONFLICT(workspace_id)DO UPDATE SET limit_micro_usd=$2,updated_at=now() WHERE wren_ai_allowances.closed_at IS NULL`, workspace, limit)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrForbidden
	}
	return err
}
func (s *Store) WrenAIAllowance(ctx context.Context, workspace string) (WrenAIAllowance, error) {
	var v WrenAIAllowance
	err := s.pool.QueryRow(ctx, `SELECT workspace_id,limit_micro_usd,held_micro_usd,spent_micro_usd,closed_at FROM wren_ai_allowances WHERE workspace_id=$1`, workspace).Scan(&v.WorkspaceID, &v.LimitMicroUSD, &v.HeldMicroUSD, &v.SpentMicroUSD, &v.ClosedAt)
	return v, wrenAIStoreError(err)
}

func (s *Store) ReserveWrenAIUsage(ctx context.Context, in WrenAIReservationInput) (WrenAIReservation, error) {
	var out WrenAIReservation
	if len(in.Estimate) > 8192 || (len(in.Estimate) > 0 && !json.Valid(in.Estimate)) {
		return out, fmt.Errorf("invalid AI estimate metadata")
	}
	if len(in.Estimate) == 0 {
		in.Estimate = json.RawMessage(`{}`)
	}
	if in.Purpose == "title" && (in.Kind != "operator" || in.RunID == "" || in.RunLease == "") {
		return out, ErrForbidden
	}
	if in.RunID != "" && in.RunLease == "" {
		return out, ErrForbidden
	}
	if in.Purpose == "welcome" && (in.ConversationID != "" || in.RunID != "") {
		return out, ErrForbidden
	}
	if in.Purpose != "" && in.Purpose != "welcome" && in.Purpose != "title" {
		return out, fmt.Errorf("invalid AI purpose")
	}
	if in.ConversationID == "" && (in.Kind != "operator" || in.Purpose != "welcome" || in.RunID != "") {
		return out, ErrForbidden
	}
	if in.AmountMicroUSD <= 0 || in.AmountMicroUSD > 1_000_000_000_000 || len(in.RequestKey) < 1 || len(in.RequestKey) > 256 || len(in.Model) < 1 || len(in.Model) > 160 || (in.Kind != "operator" && in.Kind != "acp" && in.Kind != "dictation" && in.Kind != "media") {
		return out, fmt.Errorf("invalid AI reservation")
	}
	if err := s.RequireWorkspaceWriter(ctx, in.WorkspaceID, in.AccountID); err != nil {
		return out, err
	}
	if in.ConversationID != "" {
		if _, err := s.Conversation(ctx, in.WorkspaceID, in.AccountID, in.ConversationID); err != nil {
			return out, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = requireWorkspaceWriter(ctx, tx, in.WorkspaceID, in.AccountID, true); err != nil {
		return out, err
	}
	var authorized string
	if in.ConversationID != "" {
		if err = tx.QueryRow(ctx, `SELECT c.id FROM operator_conversations c WHERE c.id=$1 AND c.workspace_id=$2 AND `+conversationAccess+` FOR SHARE OF c`, in.ConversationID, in.WorkspaceID, in.AccountID).Scan(&authorized); err != nil {
			return out, wrenAIStoreError(err)
		}
	}
	var limit, held, spent int64
	var closed *time.Time
	if err = tx.QueryRow(ctx, `SELECT limit_micro_usd,held_micro_usd,spent_micro_usd,closed_at FROM wren_ai_allowances WHERE workspace_id=$1 FOR UPDATE`, in.WorkspaceID).Scan(&limit, &held, &spent, &closed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, fmt.Errorf("%w: workspace AI allowance is not configured", ErrCapacity)
		}
		return out, err
	}
	if closed != nil {
		return out, ErrForbidden
	}
	var kind, model, account, conversation, run, purpose, runLease string
	err = tx.QueryRow(ctx, `SELECT id,workspace_id,state,amount_micro_usd,actual_micro_usd,kind,model,account_id,conversation_id,run_id,purpose,run_lease FROM wren_ai_reservations WHERE workspace_id=$1 AND request_key=$2`, in.WorkspaceID, in.RequestKey).Scan(&out.ID, &out.WorkspaceID, &out.State, &out.AmountMicroUSD, &out.ActualMicroUSD, &kind, &model, &account, &conversation, &run, &purpose, &runLease)
	if err == nil {
		if out.AmountMicroUSD != in.AmountMicroUSD || kind != in.Kind || model != in.Model || account != in.AccountID || conversation != in.ConversationID || run != in.RunID || purpose != in.Purpose || runLease != wrenAIRunLeaseHash(in.RunLease) {
			return WrenAIReservation{}, ErrConflict
		}
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if spent > limit || held > limit-spent || in.AmountMicroUSD > limit-spent-held {
		return out, fmt.Errorf("%w: shared workspace AI allowance is exhausted", ErrCapacity)
	}
	out.ID, err = newID("ai_res_")
	if err != nil {
		return out, err
	}
	out.WorkspaceID = in.WorkspaceID
	out.State = "reserved"
	out.AmountMicroUSD = in.AmountMicroUSD
	if _, err = tx.Exec(ctx, `INSERT INTO wren_ai_reservations(id,workspace_id,request_key,account_id,conversation_id,run_id,kind,model,state,amount_micro_usd,estimate,purpose,run_lease)VALUES($1,$2,$3,$4,$5,$6,$7,$8,'reserved',$9,$10,$11,$12)`, out.ID, in.WorkspaceID, in.RequestKey, in.AccountID, in.ConversationID, in.RunID, in.Kind, in.Model, in.AmountMicroUSD, in.Estimate, in.Purpose, wrenAIRunLeaseHash(in.RunLease)); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_ai_allowances SET held_micro_usd=held_micro_usd+$2,updated_at=now() WHERE workspace_id=$1`, in.WorkspaceID, in.AmountMicroUSD); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// DispatchWrenAIUsage is a once-only fence before network delivery. A crash
// after this durable marker is uncertain, even if no HTTP response was saved.
func (s *Store) DispatchWrenAIUsage(ctx context.Context, workspace, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var account, conversation, runID, lease, purpose, kind string
	var estimate json.RawMessage
	if err = tx.QueryRow(ctx, `SELECT account_id,conversation_id,run_id,run_lease,purpose,kind,estimate FROM wren_ai_reservations WHERE workspace_id=$1 AND id=$2`, workspace, id).Scan(&account, &conversation, &runID, &lease, &purpose, &kind, &estimate); err != nil {
		return wrenAIStoreError(err)
	}
	if err = s.lockWrenAIDispatchAuthority(ctx, tx, workspace, account, conversation, runID, lease, purpose, kind, estimate); err != nil {
		return err
	}
	var closed *time.Time
	if err = tx.QueryRow(ctx, `SELECT closed_at FROM wren_ai_allowances WHERE workspace_id=$1 FOR SHARE`, workspace).Scan(&closed); err != nil {
		return wrenAIStoreError(err)
	}
	if closed != nil {
		return ErrForbidden
	}
	tag, err := tx.Exec(ctx, `UPDATE wren_ai_reservations SET state='dispatched',dispatched_at=now(),updated_at=now() WHERE workspace_id=$1 AND id=$2 AND state='reserved'`, workspace, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *Store) MarkWrenAIUsageUncertain(ctx context.Context, workspace, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE wren_ai_reservations SET state='uncertain',updated_at=now() WHERE workspace_id=$1 AND id=$2 AND state IN ('dispatched','uncertain')`, workspace, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// ReleaseWrenAIUsage only refunds an undispatched reservation. Supplier zero
// cost evidence after dispatch is settled as actual zero, never guessed.
func (s *Store) ReleaseWrenAIUsage(ctx context.Context, workspace, id string) error {
	return s.finishWrenAIUsage(ctx, workspace, id, nil, "", nil)
}
func (s *Store) SettleWrenAIUsage(ctx context.Context, workspace, id string, actual int64, providerID string, usage json.RawMessage) error {
	if actual < 0 || actual > 1_000_000_000_000 || len(providerID) > 256 || len(usage) > 64<<10 || (len(usage) > 0 && !json.Valid(usage)) {
		return fmt.Errorf("invalid AI usage receipt")
	}
	usage = wrenAISafeUsage(usage)
	return s.finishWrenAIUsage(ctx, workspace, id, &actual, providerID, usage)
}
func (s *Store) finishWrenAIUsage(ctx context.Context, workspace, id string, actual *int64, providerID string, usage json.RawMessage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var cap int64
	if err = tx.QueryRow(ctx, `SELECT limit_micro_usd FROM wren_ai_allowances WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&cap); err != nil {
		return wrenAIStoreError(err)
	}
	var state string
	var amount int64
	var recorded *int64
	var oldProvider string
	if err = tx.QueryRow(ctx, `SELECT state,amount_micro_usd,actual_micro_usd,provider_id FROM wren_ai_reservations WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id).Scan(&state, &amount, &recorded, &oldProvider); err != nil {
		return wrenAIStoreError(err)
	}
	if actual == nil {
		if state == "released" {
			return tx.Commit(ctx)
		}
		if state != "reserved" {
			return ErrConflict
		}
	} else {
		if state == "settled" {
			if recorded != nil && *recorded == *actual && oldProvider == providerID {
				return tx.Commit(ctx)
			}
			return ErrConflict
		}
		if state != "dispatched" && state != "uncertain" {
			return ErrConflict
		}
	}
	target := "released"
	spent := int64(0)
	if actual != nil {
		target = "settled"
		spent = *actual
	}
	if len(usage) == 0 {
		usage = json.RawMessage(`{}`)
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_ai_allowances SET held_micro_usd=held_micro_usd-$2,spent_micro_usd=spent_micro_usd+$3,updated_at=now() WHERE workspace_id=$1`, workspace, amount, spent); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_ai_reservations SET state=$3,actual_micro_usd=$4,provider_id=$5,usage=$6,settled_at=now(),updated_at=now() WHERE workspace_id=$1 AND id=$2`, workspace, id, target, actual, providerID, usage); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WrenAICostMicroUSD rounds provider USD cost upward, never silently to zero.
func WrenAICostMicroUSD(cost float64) (int64, error) {
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 || cost > 1_000_000 {
		return 0, fmt.Errorf("invalid provider AI cost")
	}
	return int64(math.Ceil(cost * 1_000_000)), nil
}

func wrenAIStoreError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// CloseWrenAIAllowanceTx is called before removing workspace membership. The
// existing financial workspace shell retains opaque receipts and uncertain
// holds; neither closure nor account deletion invents a supplier refund.
func (s *Store) CloseWrenAIAllowanceTx(ctx context.Context, tx pgx.Tx, workspace string) error {
	return CloseWrenAIAllowanceTx(ctx, tx, workspace)
}

// Financial receipts retain numeric usage only, never supplier echo text,
// prompts, image data, recordings, or arbitrary provider diagnostic payloads.
func wrenAISafeUsage(raw json.RawMessage) json.RawMessage {
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil {
		return json.RawMessage(`{}`)
	}
	out := map[string]json.RawMessage{}
	for _, key := range []string{"cost", "prompt_tokens", "completion_tokens", "total_tokens", "input_tokens", "output_tokens", "seconds"} {
		var n float64
		if v := in[key]; len(v) > 0 && json.Unmarshal(v, &n) == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			out[key] = v
		}
	}
	data, _ := json.Marshal(out)
	return data
}

// CloseWrenAIAllowanceTx also supports callers already inside a deletion tx.
func CloseWrenAIAllowanceTx(ctx context.Context, tx pgx.Tx, workspace string) error {
	_, err := tx.Exec(ctx, `UPDATE wren_ai_allowances SET closed_at=COALESCE(closed_at,now()),updated_at=now() WHERE workspace_id=$1`, workspace)
	return err
}

// Financial rows keep a one-way fence digest, never a reusable worker lease.
func wrenAIRunLeaseHash(lease string) string {
	if lease == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(lease))
	return hex.EncodeToString(sum[:])
}
