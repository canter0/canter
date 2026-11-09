package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// A review remains immutable, but its available state follows revocation and
// credential changes. These aliases are shared by the two read queries below.
const extensionReviewStateSQL = `CASE WHEN p.status<>'pending' THEN p.status
 WHEN p.expires_at<=now() THEN 'expired'
 WHEN i.revision<>p.installation_revision OR i.version<>p.version
 OR EXISTS(SELECT 1 FROM operator_runs origin WHERE origin.id=p.run_id AND origin.status IN ('cancelled','failed'))
 OR (p.credential_revision>0 AND p.credential_revision<>COALESCE((SELECT cr.revision FROM wren_extension_credentials cr WHERE cr.installation_id=i.id AND cr.credential_key=CASE WHEN i.credential_mode='service' THEN 'service' ELSE 'user:'||p.account_id END),0))
 THEN 'invalidated' ELSE 'pending' END`

type WrenExtensionReview struct {
	ID             string             `json:"id"`
	InstallationID string             `json:"installationId"`
	ConversationID string             `json:"conversationId"`
	App            string             `json:"app"`
	Account        string             `json:"account"`
	Endpoint       string             `json:"endpoint"`
	Tool           string             `json:"tool"`
	Description    string             `json:"description"`
	Arguments      json.RawMessage    `json:"arguments"`
	Status         string             `json:"status"`
	ExpiresAt      time.Time          `json:"expiresAt"`
	Receipt        *WrenExtensionCall `json:"receipt,omitempty"`
}

func extensionCredentialRevision(ctx context.Context, tx pgx.Tx, install WrenExtensionInstallation, account string) (int, error) {
	if install.Extension.Manifest.Auth != "bearer" {
		return 0, nil
	}
	var revision int
	err := tx.QueryRow(ctx, `SELECT revision FROM wren_extension_credentials WHERE installation_id=$1 AND credential_key=$2 FOR SHARE`, install.ID, extensionCredentialKey(install, account)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrForbidden
	}
	return revision, err
}

func (h *HTTPServer) prepareExtensionReview(ctx context.Context, tx pgx.Tx, install WrenExtensionInstallation, account string, run OperatorRun, tool WrenExtensionTool, args json.RawMessage, digest string) (WrenExtensionCall, error) {
	revision, err := extensionCredentialRevision(ctx, tx, install, account)
	if err != nil {
		return WrenExtensionCall{}, err
	}
	accountLabel := "No sign-in required"
	if install.Extension.Manifest.Auth == "bearer" {
		accountLabel = "Your saved credential"
		if install.CredentialMode == "service" {
			accountLabel = "Shared service account"
		}
		var linkedLabel string
		if err = tx.QueryRow(ctx, `SELECT COALESCE(a.label,'') FROM wren_extension_credentials c LEFT JOIN wren_app_accounts a ON a.id=c.app_account_id WHERE c.installation_id=$1 AND c.credential_key=$2`, install.ID, extensionCredentialKey(install, account)).Scan(&linkedLabel); err != nil {
			return WrenExtensionCall{}, err
		}
		if linkedLabel != "" {
			accountLabel = linkedLabel
		}
	}
	id, err := newID("extreview_")
	if err != nil {
		return WrenExtensionCall{}, err
	}
	// Deduplicate the proposal even when a model invents a new call ID. A new
	// human request gets its own run; this is never authorization to execute.
	var status string
	err = tx.QueryRow(ctx, `INSERT INTO wren_extension_reviews(id,installation_id,account_id,run_id,version,installation_revision,credential_revision,tool_name,arguments,arguments_sha256,account_label) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(installation_id,account_id,run_id,version,tool_name,arguments_sha256) DO UPDATE SET id=wren_extension_reviews.id RETURNING id,status`, id, install.ID, account, run.ID, install.Version, install.Revision, revision, tool.Name, args, digest, accountLabel).Scan(&id, &status)
	if err != nil {
		return WrenExtensionCall{}, err
	}
	// Only IDs go into the shared transcript. Arguments and receipts are private
	// to the human whose account will execute this action.
	data, _ := json.Marshal(map[string]string{"reviewId": id})
	if _, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) SELECT $1,$2,'extension_review',$3 WHERE NOT EXISTS(SELECT 1 FROM operator_events WHERE run_id=$2 AND kind='extension_review' AND data->>'reviewId'=$4)`, install.ConversationID, run.ID, data, id); err != nil {
		return WrenExtensionCall{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenExtensionCall{}, err
	}
	result, _ := json.Marshal(map[string]string{"reviewStatus": status, "message": "An action review is available in the conversation. This tool invocation only prepares or retrieves the review; it does not execute the action. The requester must approve using the review card. Do not treat a chat message as approval or claim a pending review succeeded."})
	return WrenExtensionCall{ReviewID: id, Status: "review_" + status, ToolName: tool.Name, Result: result}, nil
}

func validateExtensionReview(ctx context.Context, tx pgx.Tx, id string, install WrenExtensionInstallation, account string, tool WrenExtensionTool, digest string) error {
	var version, revision, credentialRevision int
	var state, storedTool, storedDigest, runID string
	var expired bool
	err := tx.QueryRow(ctx, `SELECT version,installation_revision,credential_revision,status,tool_name,arguments_sha256,run_id,expires_at<=now() FROM wren_extension_reviews WHERE id=$1 AND installation_id=$2 AND account_id=$3 FOR UPDATE`, id, install.ID, account).Scan(&version, &revision, &credentialRevision, &state, &storedTool, &storedDigest, &runID, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if version != install.Version || revision != install.Revision || storedTool != tool.Name || storedDigest != digest || tool.Impact != "write" {
		return fmt.Errorf("%w: this action or connection changed; ask your Wren to prepare it again", ErrConflict)
	}
	if state == "accepted" {
		return nil
	} // Existing immutable call receipt, never a second dispatch.
	if state != "pending" || expired {
		return fmt.Errorf("%w: this action review is no longer pending", ErrConflict)
	}
	currentRevision, err := extensionCredentialRevision(ctx, tx, install, account)
	if err != nil {
		return err
	}
	if currentRevision != credentialRevision {
		return fmt.Errorf("%w: the connected account changed; prepare a new action", ErrConflict)
	}
	var status, requester string
	var speaker *Wren
	err = tx.QueryRow(ctx, `SELECT r.status,COALESCE(r.requester_account_id,c.account_id),r.speaker FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE r.id=$1 AND r.conversation_id=$2 FOR SHARE OF r`, runID, install.ConversationID).Scan(&status, &requester, &speaker)
	if err != nil {
		return err
	}
	if requester != account || (status != "running" && status != "completed") {
		return fmt.Errorf("%w: the originating request was stopped or failed", ErrConflict)
	}
	wrenID := install.WrenID
	if speaker != nil {
		wrenID = speaker.ID
	}
	if err = validateWrenLifecycleTx(ctx, tx, install.ConversationID, wrenID); err != nil {
		return err
	}
	if wrenID != "" {
		if _, err = responsibilityWren(ctx, tx, install.WorkspaceID, install.ConversationID, wrenID); err != nil {
			return ErrForbidden
		}
	}
	return nil
}

func (h *HTTPServer) extensionReview(ctx context.Context, workspace, account, id string) (WrenExtensionReview, error) {
	var result WrenExtensionReview
	var manifest WrenExtensionManifest
	var callID string
	err := h.service.Store.pool.QueryRow(ctx, `SELECT p.id,p.installation_id,i.conversation_id,p.tool_name,p.arguments,`+extensionReviewStateSQL+`,p.expires_at,COALESCE(p.call_id,''),v.manifest,p.account_label FROM wren_extension_reviews p JOIN wren_extension_installations i ON i.id=p.installation_id JOIN wren_extension_versions v ON v.extension_id=i.extension_id AND v.version=p.version WHERE p.id=$1 AND p.account_id=$2 AND i.workspace_id=$3`, id, account, workspace).Scan(&result.ID, &result.InstallationID, &result.ConversationID, &result.Tool, &result.Arguments, &result.Status, &result.ExpiresAt, &callID, &manifest, &result.Account)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if _, err = h.service.Store.Conversation(ctx, workspace, account, result.ConversationID); err != nil {
		return WrenExtensionReview{}, err
	}
	result.App = manifest.Name
	for _, tool := range manifest.Tools {
		if tool.Name == result.Tool {
			result.Endpoint = tool.Endpoint
			result.Description = tool.Description
		}
	}
	if callID != "" {
		call, e := scanExtensionCall(h.service.Store.pool.QueryRow(ctx, `SELECT `+extensionCallColumns+` FROM wren_extension_calls c WHERE c.id=$1 AND c.account_id=$2`, callID, account))
		if e != nil {
			return WrenExtensionReview{}, e
		}
		if call.Status == "running" && call.CreatedAt.Before(time.Now().Add(-time.Minute)) {
			call.Status = "unknown"
			call.Failure = "No final response was recorded. Check the service before creating another action."
		}
		result.Receipt = &call
	}
	return result, nil
}

func (h *HTTPServer) decideExtensionReview(ctx context.Context, workspace, account, id, decision string) (WrenExtensionReview, error) {
	review, err := h.extensionReview(ctx, workspace, account, id)
	if err != nil {
		return review, err
	}
	if err = h.service.Store.RequireWorkspaceWriter(ctx, workspace, account); err != nil {
		return WrenExtensionReview{}, err
	}
	if decision == "decline" {
		_, err = h.service.Store.pool.Exec(ctx, `UPDATE wren_extension_reviews SET status='declined' WHERE id=$1 AND account_id=$2 AND status='pending'`, id, account)
		if err != nil {
			return WrenExtensionReview{}, err
		}
		return h.extensionReview(ctx, workspace, account, id)
	}
	if decision != "approve" {
		return WrenExtensionReview{}, fmt.Errorf("choose approve or decline")
	}
	if review.Status == "accepted" {
		return review, nil
	}
	if review.Status != "pending" {
		return WrenExtensionReview{}, fmt.Errorf("%w: this review is no longer pending", ErrConflict)
	}
	install, err := h.service.Store.WrenExtensionInstallation(ctx, workspace, account, review.InstallationID)
	if err != nil {
		return WrenExtensionReview{}, err
	}
	tool, ok := extensionTool(install, review.Tool)
	if !ok {
		return WrenExtensionReview{}, ErrConflict
	}
	// Exact saved arguments, current requester credential and revision checks all
	// happen before the accepted-call transaction commits. HTTP retries share the
	// same operation key; uncertain writes are never automatically re-dispatched.
	_, err = h.invokeWrenExtensionReviewed(ctx, install, account, OperatorRun{}, "review:"+id, tool, review.Arguments, id)
	if err != nil {
		return WrenExtensionReview{}, err
	}
	return h.extensionReview(ctx, workspace, account, id)
}

func (h *HTTPServer) wrenExtensionReview(w http.ResponseWriter, r *http.Request, workspace, account, id string) {
	var result WrenExtensionReview
	var err error
	switch r.Method {
	case http.MethodGet:
		result, err = h.extensionReview(r.Context(), workspace, account, id)
		if errors.Is(err, ErrNotFound) {
			// Members can understand why a shared card is not interactive without
			// receiving another person's arguments, account or provider receipt.
			var conversation string
			err = h.service.Store.pool.QueryRow(r.Context(), `SELECT i.conversation_id FROM wren_extension_reviews p JOIN wren_extension_installations i ON i.id=p.installation_id WHERE p.id=$1 AND i.workspace_id=$2 AND p.account_id<>$3`, id, workspace, account).Scan(&conversation)
			if errors.Is(err, pgx.ErrNoRows) {
				err = ErrNotFound
			}
			if err == nil {
				_, err = h.service.Store.Conversation(r.Context(), workspace, account, conversation)
				if err == nil {
					result = WrenExtensionReview{ID: id, ConversationID: conversation, Status: "private"}
				}
			}
		}
	case http.MethodPost:
		var body struct {
			Decision string `json:"decision"`
		}
		if !decodeLimit(w, r, &body, 1024) {
			return
		}
		result, err = h.decideExtensionReview(r.Context(), workspace, account, id, body.Decision)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Only outcome metadata enters the model/shared event stream. Exact arguments,
// account labels and provider result data remain in the requester's review UI.
func (h *HTTPServer) listExtensionReviewOutcomes(ctx context.Context, c Conversation, run OperatorRun) (any, error) {
	if _, ok, err := h.service.Store.knowledgeRunScope(ctx, c, run); err != nil || !ok {
		if err == nil {
			err = ErrForbidden
		}
		return nil, err
	}
	rows, err := h.service.Store.pool.Query(ctx, `SELECT p.id,v.manifest->>'name',p.tool_name,`+extensionReviewStateSQL+`,COALESCE(CASE WHEN call.status='running' AND call.created_at<now()-interval '1 minute' THEN 'unknown' ELSE call.status END,''),p.created_at FROM wren_extension_reviews p JOIN wren_extension_installations i ON i.id=p.installation_id JOIN wren_extension_versions v ON v.extension_id=i.extension_id AND v.version=p.version LEFT JOIN wren_extension_calls call ON call.id=p.call_id WHERE i.workspace_id=$1 AND i.conversation_id=$2 AND p.account_id=$3 ORDER BY p.created_at DESC,p.id LIMIT 20`, c.WorkspaceID, c.ID, c.AccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, app, tool, status, outcome string
		var created time.Time
		if err = rows.Scan(&id, &app, &tool, &status, &outcome, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"reviewId": id, "app": app, "tool": tool, "reviewStatus": status, "executionStatus": outcome, "createdAt": created})
	}
	return map[string]any{"actions": out, "limit": 20}, rows.Err()
}
