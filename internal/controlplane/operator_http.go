package controlplane

import (
	"net/http"
	"strconv"
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
		if !h.workspaceOperatorReady(r.Context(), workspace) {
			writeError(w, http.StatusServiceUnavailable, errOperatorUnavailable)
			return
		}
		var input struct {
			ID          string               `json:"id"`
			RequestID   string               `json:"requestId"`
			Message     string               `json:"message"`
			Surface     *OperatorSurface     `json:"surface"`
			Attachments []OperatorAttachment `json:"attachments"`
		}
		if !decodeLimit(w, r, &input, 8<<20) {
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
		run, err := h.service.Store.EnqueueOperator(r.Context(), c, input.RequestID, input.Message, h.config.Operator.Model, input.Surface, input.Attachments...)
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
	if len(parts) == 1 && r.Method == http.MethodGet {
		messages, err := h.service.Store.OperatorMessages(r.Context(), c.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		run, err := h.service.Store.LatestOperatorRun(r.Context(), c.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"conversation": c, "messages": messages, "run": run})
		return
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
		if !h.workspaceOperatorReady(r.Context(), workspace) {
			writeError(w, http.StatusServiceUnavailable, errOperatorUnavailable)
			return
		}
		var input struct {
			RequestID   string               `json:"requestId"`
			Message     string               `json:"message"`
			Surface     *OperatorSurface     `json:"surface"`
			Attachments []OperatorAttachment `json:"attachments"`
		}
		if !decodeLimit(w, r, &input, 8<<20) {
			return
		}
		run, err := h.service.Store.EnqueueOperator(r.Context(), c, input.RequestID, input.Message, h.config.Operator.Model, input.Surface, input.Attachments...)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
		if err = h.service.Store.cancelOperator(r.Context(), c.ID); err != nil {
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
		for {
			// Membership may change while a long poll is waiting.
			if err = h.allowWorkspace(r, p, workspace, false); err != nil {
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
