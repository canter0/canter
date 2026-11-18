package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

var transferLimits = sdk.SandboxTransferLimits{Files: 10000, Bytes: 256 << 20, ChunkBytes: 1 << 20, MetadataBytes: 3 << 20}
var transferSHA = regexp.MustCompile(`^[a-f0-9]{64}$`)
var transferVersion = regexp.MustCompile(`^[a-f0-9]{32}$`)

type sandboxTransferGuest interface {
	SandboxTransfer(context.Context, string, any, any) error
}

func (h *HTTPServer) transferGuest(ctx context.Context, v Sandbox) (*fencedSandboxClient, sandboxTransferGuest, error) {
	if err := sandboxMutationAllowed(v); err != nil {
		return nil, nil, fmt.Errorf("%w: directory transfer requires an awake sandbox; wake it separately", err)
	}
	wrapped, err := h.sandboxClient(ctx, v)
	if err != nil {
		return nil, nil, err
	}
	client, ok := wrapped.(*fencedSandboxClient)
	if !ok {
		wrapped.Close()
		return nil, nil, ErrConflict
	}
	guest, ok := client.sandboxAgentClient.(sandboxTransferGuest)
	capabilities, err := client.Probe(ctx)
	if err != nil || !ok || capabilities.TransferProtocol < 1 {
		client.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: this image does not support directory transfers; sleep and wake on a supported image", ErrConflict)
	}
	return client, guest, nil
}

func validateTransferManifest(input sdk.SandboxTransferRequest) (int64, error) {
	if len(input.Entries)+len(input.Exclusions) > transferLimits.Files {
		return 0, fmt.Errorf("%w: transfer entry limit exceeded", ErrCapacity)
	}
	seen := make(map[string]bool, len(input.Entries))
	var size int64
	for _, entry := range input.Entries {
		if len(input.Path)+1+len(entry.Path) > 4096 {
			return 0, fmt.Errorf("%w: combined transfer path is too long", ErrConflict)
		}
		rootDirectory := entry.Path == "." && entry.Directory && input.Direction == "push" && len(input.Entries) == 1 && input.Path != "/workspace"
		if err := sdk.SandboxTransferRelativePath(entry.Path, input.IncludeSensitive); !rootDirectory && err != nil {
			return 0, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		if _, ok := seen[entry.Path]; ok {
			return 0, fmt.Errorf("%w: duplicate transfer path %q", ErrConflict, entry.Path)
		}
		seen[entry.Path] = entry.Directory
		if entry.Directory {
			if entry.Size != 0 || entry.SHA256 != "" || entry.Version != "" || entry.Executable {
				return 0, fmt.Errorf("%w: invalid directory entry", ErrConflict)
			}
			continue
		}
		if entry.Size < 0 || entry.Size > transferLimits.Bytes-size || !transferSHA.MatchString(entry.SHA256) ||
			(input.Direction == "pull" && !transferVersion.MatchString(entry.Version)) ||
			(input.Direction == "push" && entry.Version != "") {
			return 0, fmt.Errorf("%w: invalid file size, hash, or version", ErrConflict)
		}
		size += entry.Size
	}
	for _, entry := range input.Entries {
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			if directory, exists := seen[parent]; exists && !directory {
				return 0, fmt.Errorf("%w: file also used as directory", ErrConflict)
			}
		}
	}
	return size, nil
}

var errTransferOverwriteUnavailable = fmt.Errorf("%w: choose a destination without existing files", ErrConflict)

func (h *HTTPServer) planSandboxTransfer(ctx context.Context, v Sandbox, input sdk.SandboxTransferRequest, dryRun bool) (sdk.SandboxTransfer, error) {
	out := sdk.SandboxTransfer{WorkspaceID: v.WorkspaceID, SandboxID: v.ID, Generation: v.Generation, State: "planned", Receipts: map[int]sdk.SandboxTransferFileReceipt{}}
	if input.Overwrite {
		return out, errTransferOverwriteUnavailable
	}
	if input.Direction != "push" && input.Direction != "pull" {
		return out, fmt.Errorf("%w: choose push or pull", ErrConflict)
	}
	if input.Path != "/workspace" {
		suffix, ok := strings.CutPrefix(input.Path, "/workspace/")
		if !ok || sdk.SandboxTransferRelativePath(suffix, input.IncludeSensitive) != nil {
			return out, fmt.Errorf("%w: choose a normalized path inside /workspace", ErrConflict)
		}
	}
	client, guest, err := h.transferGuest(ctx, v)
	if err != nil {
		return out, err
	}
	defer client.Close()
	if input.Direction == "pull" {
		if err = guest.SandboxTransfer(ctx, "manifest", map[string]any{"path": input.Path, "includeSensitive": input.IncludeSensitive}, &input.SandboxTransferManifest); err != nil {
			return out, err
		}
	}
	size, err := validateTransferManifest(input)
	if err != nil {
		return out, err
	}
	out.SandboxTransferRequest, out.TotalBytes = input, size
	if out.Entries == nil {
		out.Entries = []sdk.SandboxTransferEntry{}
	}
	if input.Direction == "push" {
		if err = guest.SandboxTransfer(ctx, "stat", input, &out.Destinations); err != nil {
			return out, err
		}
		if len(out.Destinations) != len(input.Entries) {
			return out, ErrConflict
		}
		for i, version := range out.Destinations {
			entry := input.Entries[i]
			switch {
			case version == "missing" || (entry.Directory && version == "directory"):
			case !entry.Directory && transferVersion.MatchString(version):
				out.Collisions = append(out.Collisions, i)
				out.Receipts[i] = sdk.SandboxTransferFileReceipt{State: "conflict", Message: "Destination file already exists."}
			default:
				out.Collisions = append(out.Collisions, i)
				out.Receipts[i] = sdk.SandboxTransferFileReceipt{State: "conflict", Message: "Destination is an incompatible entry."}
			}
		}
	}
	if dryRun {
		out.State = "preview"
	} else {
		out.ID, err = newID("xfer_")
		if err != nil {
			return out, err
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if len(encoded) > transferLimits.MetadataBytes {
		return out, fmt.Errorf("%w: transfer metadata exceeds the limit", ErrCapacity)
	}
	if dryRun {
		return out, nil
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var generation int
	err = tx.QueryRow(ctx, `SELECT generation FROM sandboxes WHERE workspace_id=$1 AND id=$2 AND phase='ready'
		AND NOT sleep_requested AND NOT delete_requested FOR UPDATE`, v.WorkspaceID, v.ID).Scan(&generation)
	if err != nil || generation != v.Generation {
		return out, firstErr(err, ErrConflict)
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM sandbox_transfers WHERE sandbox_id=$1 AND state='planned' AND expires_at>now()`, v.ID).Scan(&active); err != nil {
		return out, err
	}
	if active >= 8 {
		return out, fmt.Errorf("%w: cancel or finish a pending transfer first", ErrCapacity)
	}
	if len(out.Entries) == 0 {
		out.State = "complete"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sandbox_transfers(id,workspace_id,sandbox_id,generation,plan,state) VALUES($1,$2,$3,$4,$5,$6)`,
		out.ID, v.WorkspaceID, v.ID, v.Generation, encoded, out.State); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func loadSandboxTransfer(ctx context.Context, tx pgx.Tx, v Sandbox, id string, lock bool) (sdk.SandboxTransfer, error) {
	var out sdk.SandboxTransfer
	var raw []byte
	var state string
	var expired bool
	query := `SELECT plan,state,expires_at<=now() FROM sandbox_transfers WHERE workspace_id=$1 AND sandbox_id=$2 AND id=$3`
	if lock {
		query += " FOR UPDATE"
	}
	err := tx.QueryRow(ctx, query, v.WorkspaceID, v.ID, id).Scan(&raw, &state, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	out.State = state
	if expired || (state == "planned" && (v.Generation != out.Generation || sandboxMutationAllowed(v) != nil)) {
		out.State = "stale"
	}
	rows, err := tx.Query(ctx, `SELECT entry,state,byte_offset,message FROM sandbox_transfer_files WHERE transfer_id=$1`, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var result sdk.SandboxTransferFileReceipt
		if err = rows.Scan(&index, &result.State, &result.Offset, &result.Message); err != nil {
			return out, err
		}
		out.Receipts[index] = result
	}
	return out, rows.Err()
}

func (h *HTTPServer) sandboxTransfers(w http.ResponseWriter, r *http.Request, v Sandbox, write bool, parts []string) {
	if !h.service.Store.canaryFeatureEnabled(v.WorkspaceID, h.config.Sandboxes.DirectoryTransfers) || v.DeleteRequested || v.Phase == "deleted" {
		writeStoreError(w, ErrNotFound)
		return
	}
	ctx := r.Context()
	w.Header().Set("Cache-Control", "private, no-store")
	if len(parts) == 1 {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, transferLimits)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var input sdk.SandboxTransferRequest
		if !decodeLimit(w, r, &input, int64(transferLimits.MetadataBytes)) {
			return
		}
		dry := r.URL.Query().Get("dryRun") == "true"
		if !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		out, err := h.planSandboxTransfer(ctx, v, input, dry)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(parts) > 4 || (len(parts) == 3 && parts[2] != "destination") {
		writeStoreError(w, ErrNotFound)
		return
	}
	if !write {
		writeStoreError(w, ErrForbidden)
		return
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	plan, err := loadSandboxTransfer(ctx, tx, v, parts[1], r.Method != http.MethodGet || len(parts) == 4)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, plan)
		case http.MethodDelete:
			if plan.State == "planned" || plan.State == "stale" {
				_, err = tx.Exec(ctx, `UPDATE sandbox_transfers SET state='cancelled' WHERE id=$1`, plan.ID)
				if err == nil {
					err = tx.Commit(ctx)
				}
				if err != nil {
					writeStoreError(w, err)
					return
				}
				plan.State = "cancelled"
				h.cancelTransferStaging(ctx, v, plan)
			}
			writeJSON(w, http.StatusOK, plan)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) == 3 {
		if r.Method != http.MethodPut || plan.Direction != "pull" || plan.State != "planned" {
			writeStoreError(w, ErrConflict)
			return
		}
		var input struct {
			Binding  string   `json:"binding"`
			Versions []string `json:"versions"`
		}
		if !decodeLimit(w, r, &input, int64(transferLimits.MetadataBytes)) {
			return
		}
		if !transferSHA.MatchString(input.Binding) || len(input.Versions) != len(plan.Entries) {
			writeStoreError(w, ErrConflict)
			return
		}
		if plan.DestinationBinding != "" {
			if plan.DestinationBinding != input.Binding || !slices.Equal(plan.Destinations, input.Versions) {
				writeStoreError(w, ErrConflict)
				return
			}
			writeJSON(w, http.StatusOK, plan)
			return
		}
		plan.DestinationBinding, plan.Destinations = input.Binding, input.Versions
		for index, version := range input.Versions {
			if version == "missing" || (version == "directory" && plan.Entries[index].Directory) {
				continue
			}
			if version != "unsupported" && version != "directory" && !transferSHA.MatchString(version) {
				writeStoreError(w, ErrConflict)
				return
			}
			plan.Collisions = append(plan.Collisions, index)
			plan.Receipts[index] = sdk.SandboxTransferFileReceipt{State: "conflict", Message: "Destination exists or is incompatible."}
		}
		raw, err := json.Marshal(plan)
		if err == nil && len(raw) > transferLimits.MetadataBytes {
			err = ErrCapacity
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE sandbox_transfers SET plan=$2 WHERE id=$1`, plan.ID, raw)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
		return
	}
	index, err := strconv.Atoi(parts[2])
	if err != nil || index < 0 || index >= len(plan.Entries) {
		writeStoreError(w, ErrNotFound)
		return
	}
	if plan.State != "planned" && plan.State != "complete" {
		writeStoreError(w, fmt.Errorf("%w: transfer is %s", ErrConflict, plan.State))
		return
	}
	receipt, err := h.sandboxTransferFile(w, r, tx, v, plan, index, parts[3])
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if receipt != nil {
		writeJSON(w, http.StatusOK, receipt)
	}
}

func (h *HTTPServer) sandboxTransferFile(w http.ResponseWriter, r *http.Request, tx pgx.Tx, v Sandbox, plan sdk.SandboxTransfer, index int, action string) (*sdk.SandboxTransferFileReceipt, error) {
	ctx := r.Context()
	entry := plan.Entries[index]
	receipt := plan.Receipts[index]
	invalid := fmt.Errorf("%w: invalid transfer operation", ErrConflict)
	if receipt.State == "complete" || receipt.State == "skipped" {
		return &receipt, nil
	}
	if plan.State != "planned" {
		return nil, invalid
	}
	if action == "skip" && r.Method == http.MethodPost {
		receipt.State = "skipped"
		return &receipt, saveTransferReceipt(ctx, tx, plan, index, receipt)
	}
	if plan.Overwrite {
		return nil, errTransferOverwriteUnavailable
	}
	if receipt.State == "conflict" {
		return nil, fmt.Errorf("%w: destination conflict; create a new plan", ErrConflict)
	}
	fullPath := path.Join(plan.Path, entry.Path)
	if plan.Direction == "push" {
		expected := plan.Destinations[index]
		if expected != "missing" && !(entry.Directory && expected == "directory") {
			return nil, errTransferOverwriteUnavailable
		}
		client, guest, err := h.transferGuest(ctx, v)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		defer h.sandboxActivity(ctx, v.ID)()
		upload := sandboxTransferUploadID(plan.ID, index)
		var value wrencomputer.FileUpload
		switch {
		case entry.Directory && action == "complete" && r.Method == http.MethodPost:
			_, err = sandboxMutatingCall(ctx, h.service.Store, v, 45*time.Second, func(active context.Context) (bool, error) {
				var result struct{ Complete bool }
				err := guest.SandboxTransfer(active, "mkdir", map[string]string{"path": fullPath}, &result)
				return result.Complete, err
			})
			if err == nil {
				receipt.State = "complete"
			}
		case !entry.Directory && action == "begin" && r.Method == http.MethodPost:
			value, err = client.BeginUpload(ctx, upload, fullPath, entry.Size, entry.SHA256)
		case !entry.Directory && action == "chunk" && r.Method == http.MethodPatch:
			start, parse := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
			if parse != nil || start < 0 || start > entry.Size {
				return nil, invalid
			}
			data, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(transferLimits.ChunkBytes)))
			if readErr != nil || len(data) == 0 || int64(len(data)) > entry.Size-start {
				return nil, invalid
			}
			value, err = client.UploadChunk(ctx, upload, start, data)
		case !entry.Directory && action == "complete" && r.Method == http.MethodPost:
			value, err = sandboxMutatingCall(ctx, h.service.Store, v, 90*time.Second, func(active context.Context) (wrencomputer.FileUpload, error) {
				var result wrencomputer.FileUpload
				err := guest.SandboxTransfer(active, "commit", map[string]any{"id": upload, "path": fullPath, "version": expected,
					"size": entry.Size, "sha256": entry.SHA256, "executable": entry.Executable}, &result)
				return result, err
			})
		default:
			return nil, invalid
		}
		if err != nil {
			var rejected *sandboxMutationRejected
			var guestErr *wrencomputer.Error
			if errors.As(err, &rejected) {
				return nil, err
			}
			if errors.As(err, &guestErr) && guestErr.Code != "operation_failed" {
				receipt.State, receipt.Message = "failed", "The guest rejected this operation."
				if guestErr.Code == "conflict" && action == "complete" {
					receipt.State, receipt.Message = "conflict", "The staged bytes or destination no longer match the plan."
				}
				_ = saveTransferReceipt(ctx, tx, plan, index, receipt)
				return nil, err
			}
			receipt.State, receipt.Message = "uncertain", "The guest did not return a confirmed receipt."
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			_ = saveTransferReceipt(persist, tx, plan, index, receipt)
			return nil, fmt.Errorf("%w: guest response uncertain; inspect this transfer before resuming", ErrConflict)
		}
		if !entry.Directory {
			if value.ID != upload || value.Path != fullPath || value.SHA256 != entry.SHA256 || value.Size != entry.Size ||
				value.Offset < 0 || value.Offset > entry.Size || (value.Complete && value.Offset != entry.Size) {
				return nil, invalid
			}
			receipt = sdk.SandboxTransferFileReceipt{State: "staged", Offset: value.Offset}
			if value.Complete {
				receipt.State = "complete"
			}
		}
		return &receipt, saveTransferReceipt(ctx, tx, plan, index, receipt)
	}
	if plan.DestinationBinding == "" {
		return nil, fmt.Errorf("%w: bind the pull destination to the plan before transferring", ErrConflict)
	}
	if action == "complete" && r.Method == http.MethodPost {
		var input struct {
			SHA256 string `json:"sha256"`
		}
		if !decodeLimit(w, r, &input, 1024) {
			return nil, nil
		}
		if input.SHA256 != entry.SHA256 {
			return nil, invalid
		}
		receipt = sdk.SandboxTransferFileReceipt{State: "complete", Offset: entry.Size}
		return &receipt, saveTransferReceipt(ctx, tx, plan, index, receipt)
	}
	if entry.Directory || action != "read" || r.Method != http.MethodGet {
		return nil, invalid
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 || offset > entry.Size {
		return nil, invalid
	}
	client, guest, err := h.transferGuest(ctx, v)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	defer h.sandboxActivity(ctx, v.ID)()
	var chunk wrencomputer.FileChunk
	err = guest.SandboxTransfer(ctx, "read", map[string]any{"path": fullPath, "offset": offset, "limit": transferLimits.ChunkBytes, "version": entry.Version}, &chunk)
	if err != nil {
		return nil, err
	}
	if chunk.Version != entry.Version || chunk.Size != entry.Size || chunk.Offset != offset ||
		chunk.NextOffset != offset+int64(len(chunk.Data)) || len(chunk.Data) > transferLimits.ChunkBytes || chunk.NextOffset > entry.Size ||
		chunk.EOF != (chunk.NextOffset == entry.Size) || len(chunk.Data) == 0 && !chunk.EOF {
		return nil, invalid
	}
	var live bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandboxes WHERE workspace_id=$1 AND id=$2 AND generation=$3
		AND phase='ready' AND NOT sleep_requested AND NOT delete_requested)`, v.WorkspaceID, v.ID, plan.Generation).Scan(&live)
	if err != nil || !live {
		return nil, firstErr(err, ErrConflict)
	}
	writeJSON(w, http.StatusOK, chunk)
	return nil, nil
}

func saveTransferReceipt(ctx context.Context, tx pgx.Tx, plan sdk.SandboxTransfer, index int, receipt sdk.SandboxTransferFileReceipt) error {
	_, err := tx.Exec(ctx, `INSERT INTO sandbox_transfer_files VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(transfer_id,entry) DO UPDATE SET state=EXCLUDED.state,byte_offset=EXCLUDED.byte_offset,message=EXCLUDED.message`, plan.ID, index, receipt.State, receipt.Offset, receipt.Message)
	if err != nil {
		return err
	}
	if receipt.State == "complete" || receipt.State == "skipped" {
		_, err = tx.Exec(ctx, `UPDATE sandbox_transfers SET state='complete' WHERE id=$1 AND
			(SELECT count(*) FROM sandbox_transfer_files WHERE transfer_id=$1 AND state IN ('complete','skipped'))=$2`, plan.ID, len(plan.Entries))
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func sandboxTransferUploadID(transfer string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", transfer, index)))
	return "upl_" + hex.EncodeToString(sum[:16])
}

func (h *HTTPServer) cancelTransferStaging(ctx context.Context, v Sandbox, plan sdk.SandboxTransfer) {
	if plan.Direction != "push" || plan.Generation != v.Generation || sandboxMutationAllowed(v) != nil {
		return
	}
	cleanup, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client, err := h.sandboxClient(cleanup, v)
	if err != nil {
		return
	}
	defer client.Close()
	for i, receipt := range plan.Receipts {
		if cleanup.Err() != nil {
			break
		}
		if receipt.State == "staged" || receipt.State == "uncertain" || receipt.State == "failed" || receipt.State == "conflict" && receipt.Offset > 0 {
			_ = client.AbortUpload(cleanup, sandboxTransferUploadID(plan.ID, i))
		}
	}
}
