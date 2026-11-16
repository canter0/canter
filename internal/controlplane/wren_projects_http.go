package controlplane

import (
	"net/http"
	"strconv"
)

func (h *HTTPServer) workspaceWrenProjects(w http.ResponseWriter, r *http.Request, p Principal, ws string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	s, account := h.service.Store, p.Account.ID
	if len(parts) == 0 {
		if r.Method == http.MethodGet {
			v, e := s.WrenProjects(r.Context(), ws, account)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, map[string]any{"projects": v})
			return
		}
		if r.Method == http.MethodPost {
			var in struct{ Name, SystemName string }
			if !decodeLimit(w, r, &in, 8192) {
				return
			}
			v, e := s.CreateWrenProject(r.Context(), ws, account, in.Name, in.SystemName)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 201, v)
			return
		}
		methodNotAllowed(w)
		return
	}
	project := parts[0]
	if len(parts) == 1 {
		if r.Method == http.MethodGet {
			v, e := s.WrenProject(r.Context(), ws, account, project)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		}
		if r.Method == http.MethodPatch {
			var in WrenProjectPatch
			if !decodeLimit(w, r, &in, 8192) {
				return
			}
			v, e := s.UpdateWrenProject(r.Context(), ws, account, project, in)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		}
		methodNotAllowed(w)
		return
	}
	if parts[1] == "grants" {
		if len(parts) == 2 {
			if r.Method == http.MethodGet {
				v, e := s.WrenProjectGrants(r.Context(), ws, account, project)
				if e != nil {
					writeStoreError(w, e)
					return
				}
				writeJSON(w, 200, map[string]any{"grants": v})
				return
			}
			if r.Method == http.MethodPost {
				var in struct {
					ConversationID   string
					ExpectedRevision *int
				}
				if !decodeLimit(w, r, &in, 4096) {
					return
				}
				v, e := s.AttachWrenProject(r.Context(), ws, account, project, in.ConversationID, in.ExpectedRevision)
				if e != nil {
					writeStoreError(w, e)
					return
				}
				writeJSON(w, 200, v)
				return
			}
		}
		if len(parts) == 4 && parts[3] == "revoke" && r.Method == http.MethodPost {
			var in struct{ ExpectedRevision int }
			if !decodeLimit(w, r, &in, 4096) {
				return
			}
			v, e := s.RevokeWrenProjectGrant(r.Context(), ws, account, project, parts[2], in.ExpectedRevision)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		}
		methodNotAllowed(w)
		return
	}
	if parts[1] != "notes" {
		writeStoreError(w, ErrNotFound)
		return
	}
	if len(parts) == 2 {
		if r.Method == http.MethodGet {
			v, e := s.WrenProjectNotes(r.Context(), ws, account, project, r.URL.Query().Get("q"))
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, map[string]any{"notes": v})
			return
		}
		if r.Method == http.MethodPost {
			var in WrenProjectNoteInput
			if !decodeLimit(w, r, &in, 80<<10) {
				return
			}
			if in.ID != "" {
				writeStoreError(w, ErrForbidden)
				return
			}
			v, e := s.SaveWrenProjectNote(r.Context(), ws, account, project, in, false)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 201, v)
			return
		}
		methodNotAllowed(w)
		return
	}
	note := parts[2]
	if len(parts) == 3 {
		if r.Method == http.MethodGet {
			revision := 0
			if raw := r.URL.Query().Get("revision"); raw != "" {
				n, e := strconv.Atoi(raw)
				if e != nil || n < 0 {
					writeStoreError(w, ErrConflict)
					return
				}
				revision = n
			}
			v, e := s.WrenProjectNote(r.Context(), ws, account, project, note, revision)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		}
		if r.Method == http.MethodPatch {
			var in WrenProjectNoteInput
			if !decodeLimit(w, r, &in, 80<<10) {
				return
			}
			in.ID = note
			v, e := s.SaveWrenProjectNote(r.Context(), ws, account, project, in, false)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		}
	}
	if len(parts) == 4 && parts[3] == "revisions" && r.Method == http.MethodGet {
		v, e := s.WrenProjectNoteHistory(r.Context(), ws, account, project, note)
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"revisions": v})
		return
	}
	if len(parts) == 4 && parts[3] == "forget" && r.Method == http.MethodPost {
		var in WrenProjectNoteInput
		if !decodeLimit(w, r, &in, 4096) {
			return
		}
		in.ID = note
		v, e := s.SaveWrenProjectNote(r.Context(), ws, account, project, in, true)
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	methodNotAllowed(w)
}
