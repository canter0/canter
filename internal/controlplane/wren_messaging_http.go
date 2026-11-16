package controlplane

import (
	"net/http"
)

func (h *HTTPServer) workspaceWrenMessaging(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	if len(parts) == 1 && parts[0] == "inbox" && r.Method == http.MethodGet {
		out, e := h.service.Store.WrenMessageInbox(r.Context(), workspace, account, r.URL.Query().Get("conversationId"))
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"messages": out})
		return
	}
	if len(parts) == 1 && parts[0] == "connections" {
		if r.Method == http.MethodGet {
			scope := WrenKnowledgeScope{Scope: r.URL.Query().Get("scope"), ConversationID: r.URL.Query().Get("conversationId"), WrenID: r.URL.Query().Get("wrenId")}
			out, e := h.service.Store.WrenMessageConnections(r.Context(), workspace, account, scope)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, map[string]any{"connections": out})
			return
		}
		if r.Method == http.MethodPost {
			var in WrenMessageConnectionInput
			if !decodeLimit(w, r, &in, 16<<10) {
				return
			}
			out, e := h.SaveWrenMessageConnection(r.Context(), workspace, account, in)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, out)
			return
		}
	}
	if len(parts) == 1 && parts[0] == "drafts" {
		if r.Method == http.MethodGet {
			out, e := h.service.Store.WrenMessageDrafts(r.Context(), workspace, account, r.URL.Query().Get("conversationId"))
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, map[string]any{"drafts": out})
			return
		}
		if r.Method == http.MethodPost {
			var in WrenMessageDraftInput
			if !decodeLimit(w, r, &in, 96<<10) {
				return
			}
			out, e := h.service.Store.SaveWrenMessageDraft(r.Context(), workspace, account, in, nil)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, out)
			return
		}
	}
	if len(parts) == 2 && parts[0] == "drafts" {
		if r.Method == http.MethodGet {
			out, e := h.service.Store.WrenMessageDraft(r.Context(), workspace, account, parts[1])
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, out)
			return
		}
		if r.Method == http.MethodPatch {
			var in WrenMessageDraftInput
			if !decodeLimit(w, r, &in, 96<<10) {
				return
			}
			in.ID = parts[1]
			out, e := h.service.Store.SaveWrenMessageDraft(r.Context(), workspace, account, in, nil)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, out)
			return
		}
	}
	if len(parts) == 3 && parts[0] == "drafts" && (parts[2] == "send" || parts[2] == "reject") && r.Method == http.MethodPost {
		var in struct {
			ExpectedRevision int `json:"expectedRevision"`
		}
		if !decodeLimit(w, r, &in, 2048) {
			return
		}
		out, e := h.ActWrenMessageDraft(r.Context(), workspace, account, parts[1], in.ExpectedRevision, parts[2])
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	writeStoreError(w, ErrNotFound)
}
