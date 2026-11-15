package controlplane

import (
	"context"
	"time"
)

// Database lock waits and streams can consume the token's remaining age.
// Check the original signed clock policy and both clipped deadlines again,
// not just whether exp remains in the future.
func ciCacheCheckGrantClock(ctx context.Context, g ciCacheGrant, deadline, now time.Time) error {
	if ctx == nil || ctx.Err() != nil || !g.token.verified || !g.identity.bound ||
		!ciCacheClockOK(g.token.identity.IssuedAt, g.token.identity.ExpiresAt,
			g.token.notBefore, now) ||
		!g.identity.ExpiresAt.After(now) || !deadline.After(now) {
		return errCICacheUnauthorized
	}
	return nil
}
