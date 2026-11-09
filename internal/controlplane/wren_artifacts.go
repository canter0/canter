package controlplane

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const WrenArtifactMaxBytes = 8 << 20

// References describe the actual source work, never authority to reach it.
// Source IDs are checked against the artifact's originating conversation.
type WrenArtifactProvenance struct {
	RunID      string   `json:"runId,omitempty"`
	JobID      string   `json:"jobId,omitempty"`
	ComputerID string   `json:"computerId,omitempty"`
	Path       string   `json:"path,omitempty"`
	Sources    []string `json:"sources,omitempty"`
	Summary    string   `json:"summary,omitempty"`
}
type WrenArtifactInput struct {
	ID               string                 `json:"id"`
	ConversationID   string                 `json:"conversationId"`
	ExpectedRevision int                    `json:"expectedRevision"`
	Name             string                 `json:"name"`
	MediaType        string                 `json:"mediaType"`
	Content          []byte                 `json:"content"`
	Provenance       WrenArtifactProvenance `json:"provenance"`
}
type WrenArtifact struct {
	ID             string                 `json:"id"`
	WorkspaceID    string                 `json:"workspaceId"`
	ConversationID string                 `json:"conversationId"`
	Revision       int                    `json:"revision"`
	LatestRevision int                    `json:"latestRevision"`
	Name           string                 `json:"name"`
	MediaType      string                 `json:"mediaType"`
	Size           int                    `json:"size"`
	SHA256         string                 `json:"sha256"`
	AuthorID       string                 `json:"authorId,omitempty"`
	Provenance     WrenArtifactProvenance `json:"provenance"`
	CreatedAt      time.Time              `json:"createdAt"`
	UpdatedAt      time.Time              `json:"updatedAt"`
	Content        []byte                 `json:"-"`
}

type artifactQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func validArtifactID(id string) bool {
	if !strings.HasPrefix(id, "artifact_") || len(id) < 12 || len(id) > 100 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func artifactSafeText(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validateWrenArtifact(v *WrenArtifactInput) error {
	v.Name = strings.TrimSpace(v.Name)
	if v.Content == nil {
		v.Content = []byte{}
	}
	if !validArtifactID(v.ID) || v.ExpectedRevision < 0 {
		return fmt.Errorf("invalid artifact ID or revision")
	}
	if v.Name == "" || v.Name == "." || v.Name == ".." || strings.ContainsAny(v.Name, "/\\") || !artifactSafeText(v.Name, 200) {
		return fmt.Errorf("choose a file name of up to 200 characters without a path")
	}
	if len(v.Content) > WrenArtifactMaxBytes {
		return fmt.Errorf("artifacts can contain up to 8 MiB")
	}
	media, params, err := mime.ParseMediaType(strings.TrimSpace(v.MediaType))
	if v.MediaType == "" {
		media = "application/octet-stream"
		err = nil
	}
	if err != nil || len(params) > 0 || !strings.Contains(media, "/") || len(media) > 150 {
		return fmt.Errorf("choose a valid media type without parameters")
	}
	v.MediaType = strings.ToLower(media)
	if err = validateArtifactFormat(v); err != nil {
		return err
	}
	p := v.Provenance
	if !artifactSafeText(p.RunID, 100) || !artifactSafeText(p.JobID, 100) || !artifactSafeText(p.ComputerID, 100) || !artifactSafeText(p.Path, 2048) || !artifactSafeText(p.Summary, 1000) || len(p.Sources) > 24 {
		return fmt.Errorf("artifact source references are too long or invalid")
	}
	for _, source := range p.Sources {
		u, e := url.ParseRequestURI(source)
		if e != nil || len(source) > 2048 || !artifactSafeText(source, 2048) || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return fmt.Errorf("artifact sources must be HTTP or HTTPS links without credentials")
		}
	}
	if p.Path != "" && p.ComputerID == "" {
		return fmt.Errorf("a computer file source needs its computer ID")
	}
	return nil
}

// Validate claimed formats instead of turning arbitrary bytes into an Office
// document by changing its extension. Unrecognized binary files still download.
func validateArtifactFormat(v *WrenArtifactInput) error {
	ext := strings.ToLower(path.Ext(v.Name))
	expected := map[string]string{".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation", ".pdf": "application/pdf", ".csv": "text/csv", ".json": "application/json"}
	kind := ext
	for extension, media := range expected {
		if v.MediaType == media {
			if _, known := expected[ext]; known && ext != extension {
				return fmt.Errorf("file extension does not match its media type")
			}
			kind = extension
		}
	}
	if media, known := expected[kind]; known {
		if v.MediaType != "application/octet-stream" && v.MediaType != media {
			return fmt.Errorf("file extension does not match its media type")
		}
		v.MediaType = media
	}
	switch kind {
	case ".xlsx", ".docx", ".pptx":
		mainFile, root, mainType := "xl/workbook.xml", "workbook", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
		if kind == ".docx" {
			mainFile, root, mainType = "word/document.xml", "document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"
		}
		if kind == ".pptx" {
			mainFile, root, mainType = "ppt/presentation.xml", "presentation", "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"
		}
		archive, err := zip.NewReader(bytes.NewReader(v.Content), int64(len(v.Content)))
		if err != nil {
			return fmt.Errorf("%s is not a valid Office file", v.Name)
		}
		files := map[string]*zip.File{}
		for _, file := range archive.File {
			if _, duplicate := files[file.Name]; duplicate {
				return fmt.Errorf("Office file contains duplicate entries")
			}
			files[file.Name] = file
		}
		contentTypes, err := artifactZipXML(files["[Content_Types].xml"])
		if err != nil {
			return fmt.Errorf("Office file is missing valid content types")
		}
		var types struct {
			XMLName   xml.Name `xml:"Types"`
			Overrides []struct {
				PartName    string `xml:"PartName,attr"`
				ContentType string `xml:"ContentType,attr"`
			} `xml:"Override"`
		}
		if err = xml.Unmarshal(contentTypes, &types); err != nil || types.XMLName.Local != "Types" {
			return fmt.Errorf("Office file has invalid content types")
		}
		found := false
		for _, item := range types.Overrides {
			if item.PartName == "/"+mainFile && item.ContentType == mainType {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("Office file does not contain the claimed document type")
		}
		document, err := artifactZipXML(files[mainFile])
		if err != nil {
			return fmt.Errorf("Office file is missing its document")
		}
		var documentRoot struct{ XMLName xml.Name }
		if err = xml.Unmarshal(document, &documentRoot); err != nil || documentRoot.XMLName.Local != root {
			return fmt.Errorf("Office file has an invalid document")
		}
		relationships, err := artifactZipXML(files["_rels/.rels"])
		if err != nil {
			return fmt.Errorf("Office file is missing document relationships")
		}
		var links struct {
			XMLName xml.Name
			Items   []struct {
				ID     string `xml:"Id,attr"`
				Type   string `xml:"Type,attr"`
				Target string `xml:"Target,attr"`
			} `xml:"Relationship"`
		}
		if err = xml.Unmarshal(relationships, &links); err != nil || links.XMLName.Local != "Relationships" {
			return fmt.Errorf("Office file has invalid document relationships")
		}
		linked := false
		for _, link := range links.Items {
			if strings.HasSuffix(link.Type, "/officeDocument") && strings.TrimPrefix(path.Clean(link.Target), "/") == mainFile {
				linked = true
			}
		}
		if !linked {
			return fmt.Errorf("Office file does not link its claimed document")
		}
		if kind == ".xlsx" {
			var workbook struct {
				Sheets []struct {
					ID string `xml:"id,attr"`
				} `xml:"sheets>sheet"`
			}
			if err = xml.Unmarshal(document, &workbook); err != nil || len(workbook.Sheets) == 0 {
				return fmt.Errorf("spreadsheet has no sheets")
			}
			rels, err := artifactZipXML(files["xl/_rels/workbook.xml.rels"])
			if err != nil {
				return fmt.Errorf("spreadsheet is missing sheet relationships")
			}
			links.Items = nil
			if err = xml.Unmarshal(rels, &links); err != nil || links.XMLName.Local != "Relationships" {
				return fmt.Errorf("spreadsheet has invalid sheet relationships")
			}
			for _, sheet := range workbook.Sheets {
				foundSheet := false
				for _, link := range links.Items {
					if link.ID != sheet.ID || !(strings.HasSuffix(link.Type, "/worksheet") || strings.HasSuffix(link.Type, "/chartsheet")) {
						continue
					}
					target := path.Join("xl", link.Target)
					if strings.HasPrefix(link.Target, "/") {
						target = strings.TrimPrefix(path.Clean(link.Target), "/")
					}
					sheetXML, e := artifactZipXML(files[target])
					if e != nil {
						return fmt.Errorf("spreadsheet is missing a sheet")
					}
					var sheetRoot struct{ XMLName xml.Name }
					if e = xml.Unmarshal(sheetXML, &sheetRoot); e != nil || (sheetRoot.XMLName.Local != "worksheet" && sheetRoot.XMLName.Local != "chartsheet") {
						return fmt.Errorf("spreadsheet has invalid sheet content")
					}
					foundSheet = true
				}
				if !foundSheet {
					return fmt.Errorf("spreadsheet has an unlinked sheet")
				}
			}
		}

	case ".pdf":
		if !bytes.HasPrefix(v.Content, []byte("%PDF-")) || !bytes.Contains(v.Content, []byte("%%EOF")) {
			return fmt.Errorf("file is not a PDF document")
		}
	case ".json":
		if !utf8.Valid(v.Content) || !json.Valid(v.Content) {
			return fmt.Errorf("file is not valid JSON")
		}
	case ".csv":
		if !utf8.Valid(v.Content) || bytes.IndexByte(v.Content, 0) >= 0 {
			return fmt.Errorf("CSV files must contain UTF-8 text")
		}
		reader := csv.NewReader(bytes.NewReader(v.Content))
		reader.FieldsPerRecord = -1
		for {
			_, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("file is not valid CSV: %w", err)
			}
		}
	}
	if strings.HasPrefix(v.MediaType, "text/") && (!utf8.Valid(v.Content) || bytes.IndexByte(v.Content, 0) >= 0) {
		return fmt.Errorf("text artifacts must contain UTF-8 text")
	}
	return nil
}
func artifactZipXML(file *zip.File) ([]byte, error) {
	if file == nil || file.UncompressedSize64 > 2<<20 {
		return nil, fmt.Errorf("missing or oversized Office XML")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, fmt.Errorf("invalid or oversized Office XML")
	}
	return data, nil
}

// The conversation row serializes room edits with writes. Membership is locked
// during a write so a concurrent removal cannot authorize a later commit.
func artifactAccess(ctx context.Context, q artifactQueryer, workspace, account, conversation string, lock bool) error {
	if strings.HasPrefix(conversation, "conv_job_") {
		var origin string
		err := q.QueryRow(ctx, `SELECT j.origin_conversation_id FROM wren_jobs j WHERE j.child_conversation_id=$1 AND j.workspace_id=$2 AND j.requester_account_id=$3 AND j.cancelled_at IS NULL AND NOT j.return_suppressed AND EXISTS(SELECT 1 FROM wrens w WHERE w.id=j.wren_id AND w.workspace_id=$2 AND (w.conversation_id=j.origin_conversation_id OR EXISTS(SELECT 1 FROM wren_rooms room JOIN wren_room_wrens rw ON rw.room_id=room.id WHERE room.conversation_id=j.origin_conversation_id AND room.workspace_id=$2 AND rw.wren_id=w.id)))`, conversation, workspace, account).Scan(&origin)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if origin == conversation || strings.HasPrefix(origin, "conv_job_") {
			return ErrNotFound
		}
		// Lock the originating grant before its child for a consistent order.
		if err = artifactAccess(ctx, q, workspace, account, origin, lock); err != nil {
			return err
		}
	}
	query := `SELECT c.id FROM operator_conversations c JOIN memberships m ON m.workspace_id=c.workspace_id AND m.account_id=$3 JOIN accounts a ON a.id=m.account_id AND a.disabled_at IS NULL WHERE c.id=$1 AND c.workspace_id=$2 AND ` + conversationAccess
	if lock {
		query += ` FOR UPDATE OF c FOR KEY SHARE OF m`
	}
	var id string
	err := q.QueryRow(ctx, query, conversation, workspace, account).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
func artifactProvenanceAccess(ctx context.Context, q artifactQueryer, workspace, conversation string, p WrenArtifactProvenance) error {
	var allowed bool
	if p.JobID != "" {
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_jobs WHERE id=$1 AND workspace_id=$2 AND origin_conversation_id=$3)`, p.JobID, workspace, conversation).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrNotFound
		}
	}
	if p.RunID != "" {
		var actor, sourceConversation string
		if err := q.QueryRow(ctx, `SELECT COALESCE(r.requester_account_id,c.account_id),r.conversation_id FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE r.id=$1 AND c.workspace_id=$2`, p.RunID, workspace).Scan(&actor, &sourceConversation); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := validateWrenProjectRunTx(ctx, q, workspace, actor, p.RunID, sourceConversation, true); err != nil {
			return err
		}
	}
	if p.RunID != "" {
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE r.id=$1 AND c.workspace_id=$2 AND (c.id=$3 OR EXISTS(SELECT 1 FROM wren_jobs j WHERE j.workspace_id=$2 AND j.origin_conversation_id=$3 AND j.child_conversation_id=c.id AND ($4='' OR j.id=$4))))`, p.RunID, workspace, conversation, p.JobID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrNotFound
		}
	}
	if p.ComputerID != "" {
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_computers pc JOIN operator_conversations c ON c.id=pc.conversation_id WHERE pc.id=$1 AND pc.workspace_id=$2 AND c.workspace_id=$2 AND (c.id=$3 OR ($4<>'' AND EXISTS(SELECT 1 FROM wren_jobs j WHERE j.id=$4 AND j.workspace_id=$2 AND j.origin_conversation_id=$3 AND j.child_conversation_id=c.id))))`, p.ComputerID, workspace, conversation, p.JobID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrNotFound
		}
	}
	return nil
}

func (s *Store) PutWrenArtifact(ctx context.Context, workspace, account string, input WrenArtifactInput) (WrenArtifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenArtifact{}, err
	}
	defer tx.Rollback(ctx)
	artifact, err := s.putWrenArtifactTx(ctx, tx, workspace, account, input)
	if err != nil {
		return WrenArtifact{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenArtifact{}, err
	}
	return s.GetWrenArtifact(ctx, workspace, account, artifact.ID, artifact.Revision)
}

// putWrenArtifactTx preserves all artifact validation, live ACL, provenance and
// optimistic revision checks while allowing an operation receipt to commit
// atomically with its actual bytes. The caller owns commit/rollback.
func (s *Store) putWrenArtifactTx(ctx context.Context, tx pgx.Tx, workspace, account string, input WrenArtifactInput) (WrenArtifact, error) {
	if input.ID == "" && input.ExpectedRevision == 0 {
		var err error
		input.ID, err = newID("artifact_")
		if err != nil {
			return WrenArtifact{}, err
		}
	}
	if err := validateWrenArtifact(&input); err != nil {
		return WrenArtifact{}, err
	}
	var identity string
	if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 AND disabled_at IS NULL FOR KEY SHARE`, account).Scan(&identity); err != nil {
		return WrenArtifact{}, ErrForbidden
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, workspace).Scan(&identity); err != nil {
		return WrenArtifact{}, ErrForbidden
	}
	if err := requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return WrenArtifact{}, err
	}
	var err error
	if err = artifactAccess(ctx, tx, workspace, account, input.ConversationID, true); err != nil {
		return WrenArtifact{}, err
	}
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return WrenArtifact{}, err
	}
	var current int
	var origin string
	err = tx.QueryRow(ctx, `SELECT conversation_id,current_revision FROM wren_artifacts WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, input.ID, workspace).Scan(&origin, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		if input.ExpectedRevision != 0 {
			return WrenArtifact{}, ErrNotFound
		}
		result, e := tx.Exec(ctx, `INSERT INTO wren_artifacts(id,workspace_id,conversation_id,current_revision) VALUES($1,$2,$3,1) ON CONFLICT(id) DO NOTHING`, input.ID, workspace, input.ConversationID)
		if e != nil {
			return WrenArtifact{}, e
		}
		if result.RowsAffected() != 1 {
			return WrenArtifact{}, ErrConflict
		}
	} else if err != nil {
		return WrenArtifact{}, err
	} else {
		if origin != input.ConversationID {
			return WrenArtifact{}, ErrNotFound
		}
		if input.ExpectedRevision != current {
			return WrenArtifact{}, fmt.Errorf("%w: this artifact changed; reload its latest revision", ErrConflict)
		}
		if _, err = tx.Exec(ctx, `UPDATE wren_artifacts SET current_revision=current_revision+1,updated_at=now() WHERE id=$1`, input.ID); err != nil {
			return WrenArtifact{}, err
		}
	}
	retainedProvenance := false
	if current > 0 {
		var prior WrenArtifactProvenance
		if err = tx.QueryRow(ctx, `SELECT provenance FROM wren_artifact_revisions WHERE artifact_id=$1 AND revision=$2`, input.ID, current).Scan(&prior); err != nil {
			return WrenArtifact{}, err
		}
		left, _ := json.Marshal(prior)
		right, _ := json.Marshal(input.Provenance)
		retainedProvenance = bytes.Equal(left, right)
	}
	// An unchanged source is historical lineage already validated at publish.
	// Editing a saved file must not require resurrecting its deleted computer.
	// New/changed claims still require the live same-scope source checks.
	if !retainedProvenance {
		if err = artifactProvenanceAccess(ctx, tx, workspace, input.ConversationID, input.Provenance); err != nil {
			return WrenArtifact{}, err
		}
	}
	revision := current + 1
	digest := sha256.Sum256(input.Content)
	_, err = tx.Exec(ctx, `INSERT INTO wren_artifact_revisions(artifact_id,revision,account_id,name,media_type,size_bytes,sha256,provenance,content) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, input.ID, revision, account, input.Name, input.MediaType, len(input.Content), hex.EncodeToString(digest[:]), input.Provenance, input.Content)
	if err != nil {
		return WrenArtifact{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE operator_conversations SET updated_at=now() WHERE id=$1`, input.ConversationID); err != nil {
		return WrenArtifact{}, err
	}
	event, _ := json.Marshal(map[string]any{"artifactId": input.ID, "revision": revision, "name": input.Name, "mediaType": input.MediaType, "size": len(input.Content)})
	if _, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,kind,data) VALUES($1,'artifact',$2)`, input.ConversationID, event); err != nil {
		return WrenArtifact{}, err
	}
	return scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactMetadataColumns+`,v.content FROM wren_artifacts a JOIN wren_artifact_revisions v ON v.artifact_id=a.id WHERE a.id=$1 AND a.workspace_id=$2 AND v.revision=$3`, input.ID, workspace, revision), true)
}

const artifactMetadataColumns = `a.id,a.workspace_id,a.conversation_id,v.revision,a.current_revision,v.name,v.media_type,v.size_bytes,v.sha256,COALESCE(v.account_id,''),v.provenance,a.created_at,v.created_at`

func scanArtifact(row interface{ Scan(...any) error }, includeContent bool) (WrenArtifact, error) {
	var item WrenArtifact
	fields := []any{&item.ID, &item.WorkspaceID, &item.ConversationID, &item.Revision, &item.LatestRevision, &item.Name, &item.MediaType, &item.Size, &item.SHA256, &item.AuthorID, &item.Provenance, &item.CreatedAt, &item.UpdatedAt}
	if includeContent {
		fields = append(fields, &item.Content)
	}
	err := row.Scan(fields...)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return item, err
}
func (s *Store) GetWrenArtifact(ctx context.Context, workspace, account, id string, revision int) (WrenArtifact, error) {
	if !validArtifactID(id) || revision < 0 {
		return WrenArtifact{}, ErrNotFound
	}
	var conversation string
	err := s.pool.QueryRow(ctx, `SELECT conversation_id FROM wren_artifacts WHERE id=$1 AND workspace_id=$2`, id, workspace).Scan(&conversation)
	if errors.Is(err, pgx.ErrNoRows) {
		return WrenArtifact{}, ErrNotFound
	}
	if err != nil {
		return WrenArtifact{}, err
	}
	if err = artifactAccess(ctx, s.pool, workspace, account, conversation, false); err != nil {
		return WrenArtifact{}, err
	}
	item, err := scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactMetadataColumns+`,v.content FROM wren_artifacts a JOIN wren_artifact_revisions v ON v.artifact_id=a.id AND v.revision=CASE WHEN $3=0 THEN a.current_revision ELSE $3 END WHERE a.id=$1 AND a.workspace_id=$2`, id, workspace, revision), true)
	if err != nil {
		return WrenArtifact{}, err
	}
	if err = artifactAccess(ctx, s.pool, workspace, account, conversation, false); err != nil {
		return WrenArtifact{}, err
	}
	return item, nil
}
func (s *Store) ListWrenArtifacts(ctx context.Context, workspace, account, conversation string) ([]WrenArtifact, error) {
	if err := artifactAccess(ctx, s.pool, workspace, account, conversation, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+artifactMetadataColumns+` FROM wren_artifacts a JOIN wren_artifact_revisions v ON v.artifact_id=a.id AND v.revision=a.current_revision WHERE a.workspace_id=$1 AND a.conversation_id=$2 ORDER BY a.updated_at DESC,a.id LIMIT 100`, workspace, conversation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WrenArtifact{}
	for rows.Next() {
		item, e := scanArtifact(rows, false)
		if e != nil {
			return nil, e
		}
		out = append(out, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = artifactAccess(ctx, s.pool, workspace, account, conversation, false); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) WrenArtifactRevisions(ctx context.Context, workspace, account, id string) ([]WrenArtifact, error) {
	item, err := s.GetWrenArtifact(ctx, workspace, account, id, 0)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+artifactMetadataColumns+` FROM wren_artifacts a JOIN wren_artifact_revisions v ON v.artifact_id=a.id WHERE a.id=$1 AND a.workspace_id=$2 ORDER BY v.revision DESC LIMIT 100`, id, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WrenArtifact{}
	for rows.Next() {
		version, e := scanArtifact(rows, false)
		if e != nil {
			return nil, e
		}
		out = append(out, version)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = artifactAccess(ctx, s.pool, workspace, account, item.ConversationID, false); err != nil {
		return nil, err
	}
	return out, nil
}

// Attach under /workspaces/:workspace/wren-artifacts. Never inline arbitrary
// HTML or active document content into the application origin.
func (h *HTTPServer) workspaceWrenArtifacts(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodGet {
		items, err := h.service.Store.ListWrenArtifacts(r.Context(), workspace, p.Account.ID, r.URL.Query().Get("conversationId"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"artifacts": items})
		return
	}
	if len(parts) == 0 && r.Method == http.MethodPost {
		var input WrenArtifactInput
		if !decodeLimit(w, r, &input, 12<<20) {
			return
		}
		saved, err := h.service.Store.PutWrenArtifact(r.Context(), workspace, p.Account.ID, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var input WrenArtifactInput
		if !decodeLimit(w, r, &input, 12<<20) {
			return
		}
		input.ID = parts[0]
		if input.ExpectedRevision < 1 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("the expected current revision is required"))
			return
		}
		saved, err := h.service.Store.PutWrenArtifact(r.Context(), workspace, p.Account.ID, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
		return
	}
	if len(parts) == 2 && parts[1] == "revisions" && r.Method == http.MethodGet {
		items, err := h.service.Store.WrenArtifactRevisions(r.Context(), workspace, p.Account.ID, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revisions": items})
		return
	}
	if (len(parts) == 1 || len(parts) == 2 && parts[1] == "content") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		revision := 0
		var err error
		if value := r.URL.Query().Get("revision"); value != "" {
			revision, err = strconv.Atoi(value)
			if err != nil || revision < 1 {
				writeError(w, http.StatusBadRequest, fmt.Errorf("choose a positive artifact revision"))
				return
			}
		}
		item, err := h.service.Store.GetWrenArtifact(r.Context(), workspace, p.Account.ID, parts[0], revision)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if len(parts) == 1 {
			writeJSON(w, http.StatusOK, item)
			return
		}
		w.Header().Set("Content-Type", item.MediaType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": item.Name}))
		w.Header().Set("Content-Length", strconv.Itoa(len(item.Content)))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("ETag", `"`+item.SHA256+`"`)
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(item.Content)
		}
		return
	}
	methodNotAllowed(w)
}
