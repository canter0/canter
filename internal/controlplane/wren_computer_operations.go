package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
)

// A control change and the complete remote mutation share this barrier. The
// run lease alone only orders authorization, leaving the remote call racing
// takeover. A database claim also orders independent API processes without
// holding a pool connection while the computer responds.
type computerOperation struct {
	store     *Store
	id, token string
	ctx       context.Context
	cancel    context.CancelFunc
	uncertain bool
}

func (s *Store) beginComputerOperation(ctx context.Context, id string) (*computerOperation, error) {
	token, err := newID("computer_op")
	if err != nil {
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		claimStarted := time.Now()
		result, err := s.pool.Exec(wait, `INSERT INTO wren_computer_operations(computer_id,token,expires_at) VALUES($1,$2,clock_timestamp()+interval '60 seconds') ON CONFLICT(computer_id) DO UPDATE SET token=EXCLUDED.token,expires_at=EXCLUDED.expires_at WHERE wren_computer_operations.expires_at<=clock_timestamp()`, id, token)
		if err != nil {
			if wait.Err() != nil && ctx.Err() == nil {
				return nil, fmt.Errorf("%w: the computer is finishing an action; try again shortly", ErrConflict)
			}
			return nil, err
		}
		if result.RowsAffected() == 1 {
			// Start the local bound before the database round trip, so a delayed
			// claim response cannot extend execution beyond its server-side lease.
			operationCtx, stop := context.WithDeadline(ctx, claimStarted.Add(45*time.Second))
			return &computerOperation{store: s, id: id, token: token, ctx: operationCtx, cancel: stop}, nil
		}
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: the computer is finishing an action; try again shortly", ErrConflict)
		case <-time.After(75 * time.Millisecond):
		}
	}
}

// An absent receipt is not proof that remote input stopped. Keep the bounded
// claim after an uncertain remote failure, including cancellation, so a new
// controller cannot overlap the still-finishing helper. Detached command jobs
// retain their own lifetime and stop handles; this barrier covers their launch.
func (o *computerOperation) run(action func(context.Context) error) error {
	if err := o.ctx.Err(); err != nil {
		return err
	}
	o.uncertain = true
	err := action(o.ctx)
	if err == nil {
		o.uncertain = false
	}
	var rejected *wrencomputer.Error
	if errors.As(err, &rejected) {
		switch rejected.Code {
		case "invalid_input", "invalid_path", "invalid_file", "invalid_job", "invalid_command", "invalid_timeout", "invalid_directory", "invalid_operation":
			// The trusted helper rejected validation before starting a mutation.
			o.uncertain = false
		}
	}
	return err
}

func (o *computerOperation) close() {
	o.cancel()
	if o.uncertain {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// A delayed old response must never release a replacement's claim.
	_, _ = o.store.pool.Exec(ctx, `DELETE FROM wren_computer_operations WHERE computer_id=$1 AND token=$2`, o.id, o.token)
}
