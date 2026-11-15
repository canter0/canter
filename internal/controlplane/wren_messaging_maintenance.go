package controlplane

import (
	"context"
	"time"
)

// Correspondence fetches must never delay login/security email delivery. This
// dispatcher is independent of account-mail maintenance and performs no sends.
func (h *HTTPServer) RunWrenMessagingMaintenance(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if e := h.service.Store.RecoverWrenMessageDispatches(ctx); e != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			if _, e := h.ProcessWrenMessageIngress(ctx, 4); e != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
}
