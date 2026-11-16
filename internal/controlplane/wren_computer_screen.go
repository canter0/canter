package controlplane

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
)

type computerScreenSource interface {
	ScreenStream(context.Context, func([]byte) error) error
}

func (h *HTTPServer) streamComputerScreen(w http.ResponseWriter, r *http.Request, client computerScreenSource, computer WrenComputer, account string) {
	started := false
	checked := time.Time{}
	response := http.NewResponseController(w)
	err := client.ScreenStream(r.Context(), func(frame []byte) error {
		// A connected viewer does not retain access after sign-out or removal from
		// the room. Revalidate within one second and on every renewed feed.
		if time.Since(checked) >= time.Second {
			p, e := h.human(r)
			if e != nil {
				return e
			}
			if p.Account == nil || p.Account.ID != account {
				return ErrForbidden
			}
			current, e := h.service.Store.WrenComputer(r.Context(), computer.WorkspaceID, account, computer.ID)
			if e != nil {
				return e
			}
			if current.Phase != "ready" || current.DeleteRequested || !current.ExpiresAt.After(h.service.Store.now()) {
				return ErrConflict
			}
			checked = time.Now()
		}
		if !started {
			w.Header().Set("Content-Type", wrencomputer.ScreenContentType)
			w.Header().Set("Cache-Control", "no-store, no-transform")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Accel-Buffering", "no")
			started = true
		}
		// Slow/disconnected viewers cannot hold a guest channel indefinitely.
		_ = response.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if e := binary.Write(w, binary.BigEndian, uint32(len(frame))); e != nil {
			return e
		}
		if _, e := w.Write(frame); e != nil {
			return e
		}
		return response.Flush()
	})
	if !started {
		if errors.Is(err, wrencomputer.ErrScreenUnsupported) {
			writeError(w, http.StatusNotImplemented, err)
		} else if err != nil {
			writeStoreError(w, err)
		}
		return
	}
	if err == nil {
		// Explicit terminator distinguishes normal bounded renewal from truncation.
		_ = binary.Write(w, binary.BigEndian, uint32(0))
		_ = response.Flush()
	}
}
