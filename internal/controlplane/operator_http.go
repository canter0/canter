package controlplane

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *HTTPServer) conversations(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodGet {
		items, err := h.service.Store.Conversations(r.Context(), workspace, p.Account.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"conversations": items, "agent": map[string]any{"available": h.workspaceOperatorReady(r.Context(), workspace), "model": h.config.Operator.Model}})
		return
	}
	if len(parts) == 0 && r.Method == http.MethodPost {
		if !h.workspaceOperatorReady(r.Context(), workspace) && h.config.Secrets == nil {
			writeError(w, http.StatusServiceUnavailable, errOperatorUnavailable)
			return
		}
		var input struct {
			ID           string               `json:"id"`
			Model        string               `json:"model"`
			ModelOptions OperatorModelOptions `json:"modelOptions"`
			RequestID    string               `json:"requestId"`
			Message      string               `json:"message"`
			Surface      *OperatorSurface     `json:"surface"`
			Attachments  []OperatorAttachment `json:"attachments"`
		}
		if !decodeLimit(w, r, &input, 8<<20) {
			return
		}
		if strings.HasPrefix(input.ID, "conv_job_") {
			writeStoreError(w, ErrForbidden)
			return
		}
		model, options, err := h.conversationModel(r.Context(), workspace, input.ID, input.Model, input.ModelOptions)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := validateOperatorAttachments(input.Attachments); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		c, err := h.service.Store.CreateConversation(r.Context(), workspace, p.Account.ID, input.ID, input.Message)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		run, err := h.service.Store.EnqueueOperatorWithOptions(r.Context(), c, input.RequestID, input.Message, model, options, input.Surface, input.Attachments...)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"conversation": c, "run": run})
		return
	}
	if len(parts) == 0 {
		methodNotAllowed(w)
		return
	}
	c, err := h.service.Store.Conversation(r.Context(), workspace, p.Account.ID, parts[0])
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if strings.HasPrefix(c.ID, "conv_job_") && r.Method != http.MethodGet && !(len(parts) == 2 && parts[1] == "stop") {
		writeStoreError(w, ErrForbidden)
		return
	}
	if len(parts) == 2 && parts[1] == "dictation" {
		h.wrenDictation(w, r, p, c)
		return
	}
	if len(parts) == 2 && parts[1] == "share" && (r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		share := r.Method == http.MethodPost
		changed, err := h.service.Store.ShareConversation(r.Context(), workspace, p.Account.ID, c.ID, share)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if changed {
			action := "conversation.unshared"
			if share {
				action = "conversation.shared"
			}
			_ = h.service.Store.Audit(r.Context(), workspace, p.Actor, action, c.ID, map[string]any{})
		}
		writeJSON(w, http.StatusOK, map[string]any{"shared": share})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		messages, err := h.service.Store.OperatorMessages(r.Context(), c.ID, r.URL.Query().Get("before"))
		if err != nil {
			if errors.Is(err, errInvalidOperatorMessageCursor) {
				writeError(w, http.StatusBadRequest, err)
			} else if errors.Is(err, errOperatorMessageTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, err)
			} else {
				writeStoreError(w, err)
			}
			return
		}
		run, err := h.service.Store.LatestOperatorRun(r.Context(), c.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		ownRun, err := h.service.Store.ownActiveOperatorRun(r.Context(), c.ID, p.Account.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		activeRuns, runSequence, err := h.service.Store.activeOperatorRuns(r.Context(), c.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"conversation": c, "messages": messages.Messages, "hasMore": messages.HasMore, "nextCursor": messages.NextCursor, "run": run, "ownRun": ownRun, "activeRuns": activeRuns, "runSequence": runSequence})
		return
	}
	if len(parts) == 1 && (r.Method == http.MethodPatch || r.Method == http.MethodDelete) {
		var room bool
		if err = h.service.Store.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM wren_rooms WHERE conversation_id=$1)`, c.ID).Scan(&room); err != nil {
			writeStoreError(w, err)
			return
		}
		if room {
			writeStoreError(w, ErrForbidden)
			return
		}
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var input struct {
			Title string `json:"title"`
		}
		if !decodeLimit(w, r, &input, 4096) {
			return
		}
		updated, err := h.service.Store.RenameConversation(r.Context(), workspace, p.Account.ID, c.ID, input.Title)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := h.service.Store.DeleteConversation(r.Context(), workspace, p.Account.ID, c.ID); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
		return
	}
	if len(parts) == 2 && parts[1] == "messages" && r.Method == http.MethodPost {
		var input struct {
			ReplyToMessageID string               `json:"replyToMessageId"`
			PostOnly         bool                 `json:"postOnly"`
			RecipientIDs     []string             `json:"recipientIds"`
			RequestID        string               `json:"requestId"`
			Model            string               `json:"model"`
			ModelOptions     OperatorModelOptions `json:"modelOptions"`
			Message          string               `json:"message"`
			Surface          *OperatorSurface     `json:"surface"`
			Attachments      []OperatorAttachment `json:"attachments"`
		}
		if !decodeLimit(w, r, &input, 8<<20) {
			return
		}
		if !input.PostOnly && !h.workspaceOperatorReady(r.Context(), workspace) && h.config.Secrets == nil {
			writeError(w, http.StatusServiceUnavailable, errOperatorUnavailable)
			return
		}
		model := ""
		options := OperatorModelOptions{}
		var err error
		if !input.PostOnly {
			model, options, err = h.conversationModel(r.Context(), workspace, c.ID, input.Model, input.ModelOptions)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		run, err := h.service.Store.enqueueOperatorWithRecipientsAndReply(r.Context(), c, input.RequestID, input.Message, model, options, input.Surface, input.RecipientIDs, input.PostOnly, input.ReplyToMessageID, input.Attachments...)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
		if err = h.service.Store.RequireWorkspaceWriter(r.Context(), workspace, p.Account.ID); err != nil {
			writeStoreError(w, err)
			return
		}
		if strings.HasPrefix(c.ID, "conv_job_") {
			var jobID string
			if err = h.service.Store.pool.QueryRow(r.Context(), `SELECT id FROM wren_jobs WHERE child_conversation_id=$1 AND workspace_id=$2 AND requester_account_id=$3`, c.ID, workspace, p.Account.ID).Scan(&jobID); err != nil {
				writeStoreError(w, ErrNotFound)
				return
			}
			job, err := h.service.Store.CancelWrenJob(r.Context(), workspace, p.Account.ID, jobID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"stopped": job.Status == "cancelled", "status": job.Status, "job": job})
			return
		}
		var input struct {
			RunID string `json:"runId"`
		}
		if r.ContentLength != 0 && !decodeLimit(w, r, &input, 4096) {
			return
		}
		if err = h.service.Store.cancelOperatorRequest(r.Context(), workspace, p.Account.ID, c.ID, input.RunID); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
		return
	}
	if len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet {
		after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if err != nil || after < 0 {
			after = 0
		}
		timeout := time.NewTimer(20 * time.Second)
		defer timeout.Stop()
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		cookie, cookieErr := h.humanCookie(r)
		if cookieErr != nil {
			writeStoreError(w, ErrUnauthorized)
			return
		}
		for {
			// The request principal is only a snapshot. Sessions can be revoked,
			// expire, or lose account eligibility while this long poll is open.
			active, sessionErr := h.service.Store.HumanSessionActive(r.Context(), p.Account.ID, p.Actor.SessionID, cookie.Value)
			if sessionErr != nil {
				writeStoreError(w, sessionErr)
				return
			}
			if !active {
				writeStoreError(w, ErrUnauthorized)
				return
			}
			// Membership may change while a long poll is waiting.
			if err = h.allowWorkspace(r, p, workspace, false); err != nil {
				writeStoreError(w, err)
				return
			}
			if _, err = h.service.Store.Conversation(r.Context(), workspace, p.Account.ID, c.ID); err != nil {
				writeStoreError(w, err)
				return
			}
			events, err := h.service.Store.OperatorEvents(r.Context(), c.ID, after)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			if len(events) > 0 {
				writeJSON(w, http.StatusOK, map[string]any{"events": events})
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-timeout.C:
				writeJSON(w, http.StatusOK, map[string]any{"events": []OperatorEvent{}})
				return
			case <-tick.C:
			}
		}
	}
	writeStoreError(w, ErrNotFound)
}
