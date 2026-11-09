package controlplane

import (
	"context"
)

const defaultWrenModel = "openai/gpt-6-luna"

// Wrens use one operator-configured model. Caller preferences, old picker
// choices and forged requests cannot change the model or its reasoning budget.
func (c OperatorConfig) wrenModel() string {
	if c.WrenModel != "" {
		return c.WrenModel
	}
	return defaultWrenModel
}
func (s *Store) managedWrenConversation(ctx context.Context, workspace, id string) (bool, error) {
	var managed bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wrens WHERE workspace_id=$1 AND conversation_id=$2 UNION ALL SELECT 1 FROM wren_rooms WHERE workspace_id=$1 AND conversation_id=$2 UNION ALL SELECT 1 FROM wren_jobs WHERE workspace_id=$1 AND child_conversation_id=$2)`, workspace, id).Scan(&managed)
	return managed, err
}
func (h *HTTPServer) conversationModel(ctx context.Context, workspace, id, requested string, options OperatorModelOptions) (string, OperatorModelOptions, error) {
	// All chat entry points, including legacy Canter conversations, share the
	// managed policy. A stale dashboard cannot reintroduce user model selection.
	return h.config.Operator.wrenModel(), OperatorModelOptions{}, nil
}

func (s *Store) enforceWrenRunModel(ctx context.Context, c Conversation, run *OperatorRun, config OperatorConfig) error {
	model := config.wrenModel()
	if run.Model == model && run.ModelOptions.ReasoningEffort == "" {
		return nil
	}
	result, err := s.pool.Exec(ctx, `UPDATE operator_runs SET model=$3,model_options='{}'::jsonb WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now()`, run.ID, run.Lease, model)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	run.Model = model
	run.ModelOptions = OperatorModelOptions{}
	return nil
}
