package controlplane

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const WrenArtifactEditMaxBytes = 1 << 20
const WrenArtifactEditMaxRows = 2000
const WrenArtifactEditMaxColumns = 64
const WrenArtifactEditMaxCells = 50000

type WrenArtifactLibraryQuery struct {
	ConversationID string `json:"conversationId"`
	Query          string `json:"q"`
	PinnedOnly     bool   `json:"pinnedOnly"`
	Cursor         string `json:"cursor"`
	Limit          int    `json:"limit"`
}
type WrenArtifactLibraryEntry struct {
	sortTime time.Time
	WrenArtifact
	AuthorName        string     `json:"authorName"`
	SourceWrenName    string     `json:"sourceWrenName,omitempty"`
	ConversationTitle string     `json:"conversationTitle"`
	Pinned            bool       `json:"pinned"`
	PinnedAt          *time.Time `json:"pinnedAt,omitempty"`
}
type WrenArtifactLibraryPage struct {
	Artifacts  []WrenArtifactLibraryEntry `json:"artifacts"`
	HasMore    bool                       `json:"hasMore"`
	NextCursor string                     `json:"nextCursor,omitempty"`
}
type WrenArtifactDocument struct {
	Artifact   WrenArtifact `json:"artifact"`
	Kind       string       `json:"kind"`
	Editable   bool         `json:"editable"`
	Reason     string       `json:"reason,omitempty"`
	Text       *string      `json:"text,omitempty"`
	Rows       *[][]string  `json:"rows,omitempty"`
	Delimiter  string       `json:"delimiter,omitempty"`
	LineEnding string       `json:"lineEnding,omitempty"`
	HasBOM     bool         `json:"hasBOM,omitempty"`
}
type WrenArtifactEditInput struct {
	ExpectedRevision int         `json:"expectedRevision"`
	Kind             string      `json:"kind"`
	Text             *string     `json:"text,omitempty"`
	Rows             *[][]string `json:"rows,omitempty"`
}

func (s *Store) WrenArtifactLibrary(ctx context.Context, workspace, account string, q WrenArtifactLibraryQuery) (WrenArtifactLibraryPage, error) {
	page := WrenArtifactLibraryPage{Artifacts: []WrenArtifactLibraryEntry{}}
	if _, err := workspaceRole(ctx, s.pool, workspace, account, false); err != nil {
		return page, err
	}
	if q.ConversationID != "" {
		if err := artifactAccess(ctx, s.pool, workspace, account, q.ConversationID, false); err != nil {
			return page, err
		}
	}
	q.Query = strings.TrimSpace(q.Query)
	if len(q.Query) > 200 {
		return page, fmt.Errorf("keep library searches under 200 bytes")
	}
	if q.Limit < 1 {
		q.Limit = 40
	}
	if q.Limit > 100 {
		q.Limit = 100
	}
	before, id, err := parseOperatorMessageCursor(q.Cursor)
	if err != nil || q.Cursor != "" && !validArtifactID(id) {
		return page, fmt.Errorf("invalid artifact library cursor")
	}
	var date any
	if q.Cursor != "" {
		date = before
	}
	access := strings.ReplaceAll(conversationAccess, "$3", "$2")
	rows, err := s.pool.Query(ctx, `SELECT `+artifactMetadataColumns+`,COALESCE(author.email,''),COALESCE(run.speaker->>'name',lead.name,''),c.title,pin.artifact_id IS NOT NULL,pin.created_at,a.updated_at FROM wren_artifacts a JOIN operator_conversations c ON c.id=a.conversation_id JOIN wren_artifact_revisions v ON v.artifact_id=a.id AND v.revision=a.current_revision LEFT JOIN accounts author ON author.id=v.account_id LEFT JOIN operator_runs run ON run.id=v.provenance->>'runId' LEFT JOIN wren_jobs job ON job.id=v.provenance->>'jobId' LEFT JOIN wrens lead ON lead.id=job.wren_id LEFT JOIN wren_artifact_pins pin ON pin.artifact_id=a.id AND pin.account_id=$2 WHERE a.workspace_id=$1 AND `+access+` AND NOT EXISTS(SELECT 1 FROM wren_jobs child WHERE child.child_conversation_id=c.id) AND ($3='' OR c.id=$3) AND ($4='' OR position(lower($4) in lower(v.name||' '||v.media_type||' '||COALESCE(v.provenance->>'summary','')))>0) AND (NOT $5 OR pin.artifact_id IS NOT NULL) AND ($6::timestamptz IS NULL OR (a.updated_at,a.id)<($6,$7)) ORDER BY a.updated_at DESC,a.id DESC LIMIT $8`, workspace, account, q.ConversationID, q.Query, q.PinnedOnly, date, id, q.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var v WrenArtifactLibraryEntry
		err = rows.Scan(&v.ID, &v.WorkspaceID, &v.ConversationID, &v.Revision, &v.LatestRevision, &v.Name, &v.MediaType, &v.Size, &v.SHA256, &v.AuthorID, &v.Provenance, &v.CreatedAt, &v.UpdatedAt, &v.AuthorName, &v.SourceWrenName, &v.ConversationTitle, &v.Pinned, &v.PinnedAt, &v.sortTime)
		if err != nil {
			return page, err
		}
		page.Artifacts = append(page.Artifacts, v)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Artifacts) > q.Limit {
		page.HasMore = true
		page.Artifacts = page.Artifacts[:q.Limit]
	}
	if page.HasMore && len(page.Artifacts) > 0 {
		last := page.Artifacts[len(page.Artifacts)-1]
		page.NextCursor = operatorMessageCursor(last.sortTime, last.ID)
	}
	filtered := []WrenArtifactLibraryEntry{}
	for _, v := range page.Artifacts {
		if err = artifactAccess(ctx, s.pool, workspace, account, v.ConversationID, false); err == nil {
			filtered = append(filtered, v)
		} else if !errors.Is(err, ErrNotFound) {
			return page, err
		}
	}
	page.Artifacts = filtered
	return page, nil
}
func (s *Store) PinWrenArtifact(ctx context.Context, workspace, account, id string, pinned bool) (WrenArtifactLibraryEntry, error) {
	artifact, err := s.GetWrenArtifact(ctx, workspace, account, id, 0)
	if err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	if strings.HasPrefix(artifact.ConversationID, "conv_job_") {
		return WrenArtifactLibraryEntry{}, fmt.Errorf("publish the worker file to its originating conversation before pinning it")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	defer tx.Rollback(ctx)
	if err = artifactAccess(ctx, tx, workspace, account, artifact.ConversationID, true); err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,45))`, account); err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	// Expired audience access must not strand an invisible personal pin quota.
	// This only removes the caller's pin metadata, never the shared artifact.
	access := strings.ReplaceAll(conversationAccess, "$3", "$1")
	if _, err = tx.Exec(ctx, `DELETE FROM wren_artifact_pins pin USING wren_artifacts a,operator_conversations c WHERE pin.artifact_id=a.id AND c.id=a.conversation_id AND pin.account_id=$1 AND NOT EXISTS(SELECT 1 FROM memberships m WHERE m.account_id=$1 AND m.workspace_id=c.workspace_id AND `+access+`)`, account); err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	if pinned {
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM wren_artifact_pins WHERE account_id=$1`, account).Scan(&count); err != nil {
			return WrenArtifactLibraryEntry{}, err
		}
		var existing bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_artifact_pins WHERE artifact_id=$1 AND account_id=$2)`, id, account).Scan(&existing); err != nil {
			return WrenArtifactLibraryEntry{}, err
		}
		if !existing && count >= 200 {
			return WrenArtifactLibraryEntry{}, fmt.Errorf("%w: up to 200 pinned artifacts", ErrCapacity)
		}
		_, err = tx.Exec(ctx, `INSERT INTO wren_artifact_pins(artifact_id,account_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, account)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM wren_artifact_pins WHERE artifact_id=$1 AND account_id=$2`, id, account)
	}
	if err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	artifact, err = s.GetWrenArtifact(ctx, workspace, account, id, 0)
	if err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	out := WrenArtifactLibraryEntry{WrenArtifact: artifact, Pinned: pinned}
	if err = s.pool.QueryRow(ctx, `SELECT COALESCE(author.email,''),c.title,COALESCE(run.speaker->>'name',lead.name,'') FROM wren_artifacts a JOIN operator_conversations c ON c.id=a.conversation_id JOIN wren_artifact_revisions v ON v.artifact_id=a.id AND v.revision=a.current_revision LEFT JOIN accounts author ON author.id=v.account_id LEFT JOIN operator_runs run ON run.id=v.provenance->>'runId' LEFT JOIN wren_jobs job ON job.id=v.provenance->>'jobId' LEFT JOIN wrens lead ON lead.id=job.wren_id WHERE a.id=$1`, id).Scan(&out.AuthorName, &out.ConversationTitle, &out.SourceWrenName); err != nil {
		return out, err
	}
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT created_at FROM wren_artifact_pins WHERE artifact_id=$1 AND account_id=$2)`, id, account).Scan(&out.PinnedAt); err != nil {
		return out, err
	}
	out.Pinned = out.PinnedAt != nil
	if err = artifactAccess(ctx, s.pool, workspace, account, artifact.ConversationID, false); err != nil {
		return WrenArtifactLibraryEntry{}, err
	}
	return out, nil
}
func artifactDocument(v WrenArtifact) WrenArtifactDocument {
	content := v.Content
	v.Content = nil
	out := WrenArtifactDocument{Artifact: v, Kind: "unsupported"}
	if len(content) > WrenArtifactEditMaxBytes {
		out.Reason = "This file is too large for the editor. Download the original file."
		return out
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		out.Reason = "This file needs its original application. Download it to edit."
		return out
	}
	extension := strings.ToLower(filepath.Ext(v.Name))
	if v.MediaType == "text/csv" || v.MediaType == "text/tab-separated-values" || extension == ".csv" || extension == ".tsv" {
		out.Kind = "csv"
		out.Delimiter = ","
		separator := ','
		if v.MediaType == "text/tab-separated-values" || extension == ".tsv" {
			out.Delimiter = "\t"
			separator = '\t'
		}
		if bytes.HasPrefix(content, []byte{0xef, 0xbb, 0xbf}) {
			out.HasBOM = true
			content = content[3:]
		}
		out.LineEnding = "lf"
		if bytes.Contains(content, []byte("\r\n")) {
			out.LineEnding = "crlf"
		}
		reader := csv.NewReader(bytes.NewReader(content))
		reader.Comma = separator
		reader.FieldsPerRecord = -1
		rows := [][]string{}
		cells := 0
		for {
			row, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				out.Reason = "This table could not be parsed safely. Download the original file."
				return out
			}
			cells += len(row)
			if len(rows) >= WrenArtifactEditMaxRows || len(row) > WrenArtifactEditMaxColumns || cells > WrenArtifactEditMaxCells {
				out.Reason = "This table is too large for the editor. Download the original file."
				return out
			}
			for _, cell := range row {
				if strings.Contains(cell, "\r") {
					out.Reason = "This table uses cell line breaks unsupported by the editor. Download the original file."
					return out
				}
				if len(cell) > 65536 {
					out.Reason = "This table contains a cell too large for the editor."
					return out
				}
			}
			rows = append(rows, row)
		}
		out.Rows = &rows
		out.Editable = true
		return out
	}
	if strings.HasPrefix(v.MediaType, "text/") || v.MediaType == "application/json" || v.MediaType == "application/yaml" || v.MediaType == "application/x-yaml" {
		text := string(content)
		out.Kind = "text"
		out.Text = &text
		out.Editable = true
		return out
	}
	out.Reason = "This format is available as a download; editing is not supported here."
	return out
}
func (s *Store) WrenArtifactDocument(ctx context.Context, workspace, account, id string, revision int) (WrenArtifactDocument, error) {
	v, err := s.GetWrenArtifact(ctx, workspace, account, id, revision)
	if err != nil {
		return WrenArtifactDocument{}, err
	}
	out := artifactDocument(v)
	if v.Revision != v.LatestRevision {
		out.Editable = false
		out.Reason = "This is a historical revision. Open the latest revision before editing."
		return out, nil
	}
	if out.Editable {
		if err = s.RequireWorkspaceWriter(ctx, workspace, account); errors.Is(err, ErrForbidden) {
			out.Editable = false
			out.Reason = "You can view this file. Editing requires an operator or owner role."
		} else if err != nil {
			return WrenArtifactDocument{}, err
		}
	}
	return out, nil
}
func (s *Store) EditWrenArtifact(ctx context.Context, workspace, account, id string, input WrenArtifactEditInput) (WrenArtifactDocument, error) {
	if input.ExpectedRevision < 1 {
		return WrenArtifactDocument{}, fmt.Errorf("the expected current revision is required")
	}
	if err := s.RequireWorkspaceWriter(ctx, workspace, account); err != nil {
		return WrenArtifactDocument{}, err
	}
	base, err := s.GetWrenArtifact(ctx, workspace, account, id, input.ExpectedRevision)
	if err != nil {
		return WrenArtifactDocument{}, err
	}
	if base.LatestRevision != input.ExpectedRevision {
		return WrenArtifactDocument{}, ErrConflict
	}
	document := artifactDocument(base)
	if !document.Editable {
		return WrenArtifactDocument{}, fmt.Errorf("this file cannot be edited here: %s", document.Reason)
	}
	if input.Kind != document.Kind {
		return WrenArtifactDocument{}, fmt.Errorf("use the file's actual editing format")
	}
	var content []byte
	switch document.Kind {
	case "text":
		if input.Text == nil || input.Rows != nil || !utf8.ValidString(*input.Text) || strings.IndexByte(*input.Text, 0) >= 0 || len(*input.Text) > WrenArtifactEditMaxBytes {
			return WrenArtifactDocument{}, fmt.Errorf("provide UTF-8 text up to 1 MiB")
		}
		content = []byte(*input.Text)
	case "csv":
		if input.Rows == nil || input.Text != nil || len(*input.Rows) > WrenArtifactEditMaxRows {
			return WrenArtifactDocument{}, fmt.Errorf("provide a table of up to 2000 rows")
		}
		var buffer bytes.Buffer
		if document.HasBOM {
			buffer.Write([]byte{0xef, 0xbb, 0xbf})
		}
		writer := csv.NewWriter(&buffer)
		if document.Delimiter == "\t" {
			writer.Comma = '\t'
		}
		writer.UseCRLF = document.LineEnding == "crlf"
		cells, total := 0, 0
		for _, row := range *input.Rows {
			if len(row) == 0 {
				return WrenArtifactDocument{}, fmt.Errorf("each row needs at least one cell; use an empty table for a blank file")
			}
			if len(row) > WrenArtifactEditMaxColumns {
				return WrenArtifactDocument{}, fmt.Errorf("up to 64 columns per row are supported")
			}
			cells += len(row)
			if cells > WrenArtifactEditMaxCells {
				return WrenArtifactDocument{}, fmt.Errorf("up to 50000 cells are supported")
			}
			for _, cell := range row {
				total += len(cell)
				if !utf8.ValidString(cell) || strings.IndexByte(cell, 0) >= 0 || strings.Contains(cell, "\r") || len(cell) > 65536 || total > WrenArtifactEditMaxBytes {
					return WrenArtifactDocument{}, fmt.Errorf("table cells must be UTF-8 text within the editor limits")
				}
			}
			if len(row) == 1 && row[0] == "" {
				writer.Flush()
				if err = writer.Error(); err != nil {
					return WrenArtifactDocument{}, err
				}
				buffer.WriteString("\"\"")
				if document.LineEnding == "crlf" {
					buffer.WriteByte('\r')
				}
				buffer.WriteByte('\n')
				continue
			}
			if err = writer.Write(row); err != nil {
				return WrenArtifactDocument{}, err
			}
		}
		writer.Flush()
		if err = writer.Error(); err != nil {
			return WrenArtifactDocument{}, err
		}
		if buffer.Len() > WrenArtifactEditMaxBytes {
			return WrenArtifactDocument{}, fmt.Errorf("edited file exceeds the 1 MiB editor limit")
		}
		content = buffer.Bytes()
	default:
		return WrenArtifactDocument{}, fmt.Errorf("unsupported editor format")
	}
	saved, err := s.PutWrenArtifact(ctx, workspace, account, WrenArtifactInput{ID: id, ConversationID: base.ConversationID, ExpectedRevision: input.ExpectedRevision, Name: base.Name, MediaType: base.MediaType, Content: content, Provenance: base.Provenance})
	if err != nil {
		return WrenArtifactDocument{}, err
	}
	return artifactDocument(saved), nil
}
func (h *HTTPServer) workspaceWrenArtifactLibrary(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	if len(parts) == 0 && r.Method == http.MethodGet {
		pinned := false
		if raw := r.URL.Query().Get("pinnedOnly"); raw != "" {
			var err error
			pinned, err = strconv.ParseBool(raw)
			if err != nil {
				writeError(w, 400, fmt.Errorf("invalid pinned filter"))
				return
			}
		}
		out, err := h.service.Store.WrenArtifactLibrary(r.Context(), workspace, account, WrenArtifactLibraryQuery{ConversationID: r.URL.Query().Get("conversationId"), Query: r.URL.Query().Get("q"), PinnedOnly: pinned, Cursor: r.URL.Query().Get("cursor")})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	if len(parts) == 2 && parts[1] == "pin" && r.Method == http.MethodPut {
		var input struct {
			Pinned bool `json:"pinned"`
		}
		if !decodeLimit(w, r, &input, 4096) {
			return
		}
		out, err := h.service.Store.PinWrenArtifact(r.Context(), workspace, account, parts[0], input.Pinned)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	if len(parts) == 2 && parts[1] == "edit" && r.Method == http.MethodGet {
		revision := 0
		if raw := r.URL.Query().Get("revision"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 {
				writeError(w, 400, fmt.Errorf("invalid artifact revision"))
				return
			}
			revision = value
		}
		out, err := h.service.Store.WrenArtifactDocument(r.Context(), workspace, account, parts[0], revision)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	if len(parts) == 2 && parts[1] == "edit" && r.Method == http.MethodPut {
		var input WrenArtifactEditInput
		if !decodeLimit(w, r, &input, 8<<20) {
			return
		}
		out, err := h.service.Store.EditWrenArtifact(r.Context(), workspace, account, parts[0], input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	http.NotFound(w, r)
}
