package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// TransferWrenRoomOwnership changes management of an existing audience. It
// neither adds room members nor grants credentials, tools, or private history.
func (s *Store) TransferWrenRoomOwnership(ctx context.Context, workspace, account, roomID, target string) (WrenRoom, error) {
	var observedOwner string
	if err := s.pool.QueryRow(ctx, `SELECT account_id FROM wren_rooms WHERE id=$1 AND workspace_id=$2`, roomID, workspace).Scan(&observedOwner); errors.Is(err, pgx.ErrNoRows) {
		return WrenRoom{}, ErrNotFound
	} else if err != nil {
		return WrenRoom{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenRoom{}, err
	}
	defer tx.Rollback(ctx)
	ids := []string{account, target, observedOwner}
	sort.Strings(ids)
	for i, id := range ids {
		if i > 0 && id == ids[i-1] {
			continue
		}
		var active bool
		err = tx.QueryRow(ctx, `SELECT disabled_at IS NULL FROM accounts WHERE id=$1 FOR SHARE`, id).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) && id != account && id != target {
			continue
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return WrenRoom{}, ErrNotFound
		}
		if err != nil {
			return WrenRoom{}, err
		}
		if !active && (id == account || id == target) {
			return WrenRoom{}, ErrNotFound
		}
	}
	var lock string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&lock); errors.Is(err, pgx.ErrNoRows) {
		return WrenRoom{}, ErrNotFound
	} else if err != nil {
		return WrenRoom{}, err
	}
	for _, id := range []string{account, target} {
		if err = requireWorkspaceWriter(ctx, tx, workspace, id, true); err != nil {
			return WrenRoom{}, err
		}
	}
	var conversation, owner, kind string
	err = tx.QueryRow(ctx, `SELECT r.conversation_id FROM wren_rooms r WHERE r.id=$1 AND r.workspace_id=$2`, roomID, workspace).Scan(&conversation)
	if errors.Is(err, pgx.ErrNoRows) {
		return WrenRoom{}, ErrNotFound
	}
	if err != nil {
		return WrenRoom{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM operator_conversations WHERE id=$1 FOR UPDATE`, conversation).Scan(&lock); err != nil {
		return WrenRoom{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT account_id,kind FROM wren_rooms WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, roomID, workspace).Scan(&owner, &kind); err != nil {
		return WrenRoom{}, err
	}
	if err = artifactAccess(ctx, tx, workspace, account, conversation, false); err != nil {
		return WrenRoom{}, err
	}
	if owner != observedOwner {
		return WrenRoom{}, ErrConflict
	}
	if owner != account {
		actorRole, e := workspaceRole(ctx, tx, workspace, account, true)
		if e != nil {
			return WrenRoom{}, e
		}
		eligible, e := roomOwnerEligible(ctx, tx, workspace, roomID, owner, kind)
		if e != nil {
			return WrenRoom{}, e
		}
		if actorRole != "owner" || eligible {
			return WrenRoom{}, ErrForbidden
		}
	}
	if kind == "group" {
		var member bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_room_members WHERE room_id=$1 AND account_id=$2)`, roomID, target).Scan(&member)
		if err != nil {
			return WrenRoom{}, err
		}
		if !member {
			return WrenRoom{}, ErrNotFound
		}
	}
	if owner == target {
		return WrenRoom{}, ErrConflict
	}
	rows, err := tx.Query(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE workspace_id=$1 AND conversation_id=$2 AND scope_kind='room' ORDER BY id FOR UPDATE`, workspace, conversation)
	if err != nil {
		return WrenRoom{}, err
	}
	computers := []WrenComputer{}
	for rows.Next() {
		v, e := scanWrenComputer(rows)
		if e != nil {
			rows.Close()
			return WrenRoom{}, e
		}
		computers = append(computers, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return WrenRoom{}, err
	}
	for _, computer := range computers {
		busy, e := activeComputerACP(ctx, tx, computer.ID, "")
		if e != nil {
			return WrenRoom{}, e
		}
		held, e := liveComputerMutation(ctx, tx, computer)
		if e != nil {
			return WrenRoom{}, e
		}
		if busy || held || computer.Phase == "queued" || computer.Phase == "provisioning" || computer.Phase == "deleting" || computer.DeleteRequested && computer.Phase != "deleted" {
			return WrenRoom{}, fmt.Errorf("%w: finish or stop active computer work before transferring ownership", ErrConflict)
		}
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_computer_checkpoints WHERE workspace_id=$1 AND conversation_id=$2 AND scope_kind='room' AND state IN ('creating','restoring','deleting'))`, workspace, conversation).Scan(&pending); err != nil {
		return WrenRoom{}, err
	}
	if pending {
		return WrenRoom{}, fmt.Errorf("%w: wait for saved workspace changes before transferring ownership", ErrConflict)
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_rooms SET account_id=$2 WHERE id=$1`, roomID, target); err != nil {
		return WrenRoom{}, err
	}

	if _, err = tx.Exec(ctx, `UPDATE operator_conversations SET account_id=$2,updated_at=now() WHERE id=$1`, conversation, target); err != nil {
		return WrenRoom{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_computers SET owner_id=$3,control_account_id=NULL,control_expires_at=NULL,updated_at=now() WHERE workspace_id=$1 AND conversation_id=$2 AND scope_kind='room'`, workspace, conversation, target); err != nil {
		return WrenRoom{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_computer_checkpoints SET owner_id=$3,updated_at=now() WHERE workspace_id=$1 AND conversation_id=$2 AND scope_kind='room'`, workspace, conversation, target); err != nil {
		return WrenRoom{}, err
	}
	data, _ := json.Marshal(map[string]any{"roomId": roomID, "ownerId": target, "actorId": account, "action": "ownership-transferred"})
	if _, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,kind,data) VALUES($1,'room',$2)`, conversation, data); err != nil {
		return WrenRoom{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenRoom{}, err
	}
	return s.Room(ctx, workspace, account, roomID)
}

func roomOwnerEligible(ctx context.Context, q artifactQueryer, workspace, room, owner, kind string) (bool, error) {
	var eligible bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memberships m JOIN accounts a ON a.id=m.account_id WHERE m.workspace_id=$1 AND m.account_id=$2 AND m.role IN ('owner','operator') AND a.disabled_at IS NULL AND ($3='channel' OR EXISTS(SELECT 1 FROM wren_room_members WHERE room_id=$4 AND account_id=m.account_id)))`, workspace, owner, kind, room).Scan(&eligible)
	return eligible, err
}
