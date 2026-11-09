package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

func mediaDigest(v any) string {
	b, _ := json.Marshal(v)
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

// Source intent is read from accepted server records, never generated tool arguments,
// retrieved documents or an extension event payload.
func (s *Store) mediaAuthority(ctx context.Context, run OperatorRun, c Conversation) (string, error) {
	var live bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE r.id=$1 AND r.conversation_id=$2 AND r.status='running' AND r.lease_token=$3 AND r.lease_expires_at>now() AND COALESCE(r.requester_account_id,c.account_id)=$4)`, run.ID, c.ID, run.Lease, c.AccountID).Scan(&live)
	if err != nil {
		return "", err
	}
	if !live {
		return "", ErrForbidden
	}
	if err = s.ValidateWrenLifecycleRun(ctx, run); err != nil {
		return "", err
	}
	id := run.ID
	for depth := 0; depth < 8; depth++ {
		var kind string
		if err = s.pool.QueryRow(ctx, `SELECT origin_kind FROM operator_runs WHERE id=$1`, id).Scan(&kind); err != nil {
			return "", err
		}
		var instruction string
		switch kind {
		case "human":
			err = s.pool.QueryRow(ctx, `SELECT content FROM operator_messages WHERE run_id=$1 AND role='user' ORDER BY created_at LIMIT 1`, id).Scan(&instruction)
		case "responsibility":
			if id != run.ID {
				return "", ErrForbidden
			}
			if err = s.ValidateResponsibilityRun(ctx, c, run); err != nil {
				return "", err
			}
			err = s.pool.QueryRow(ctx, `SELECT instruction FROM wren_responsibility_wakes WHERE run_id=$1`, id).Scan(&instruction)
		case "job":
			var parent string
			err = s.pool.QueryRow(ctx, `SELECT parent_run_id FROM wren_jobs WHERE child_run_id=$1 OR return_run_id=$1`, id).Scan(&parent)
			if err != nil {
				return "", err
			}
			id = parent
			continue
		default:
			return "", ErrForbidden
		}
		if err != nil {
			return "", err
		}
		if !wrenMediaInstructionAllows(instruction) {
			return "", fmt.Errorf("needs-image-request: ask directly to create or edit an image; the accepted instruction is ambiguous or requests another kind of work")
		}
		return id, nil
	}
	return "", ErrForbidden
}

func (h *HTTPServer) mediaConfig(ctx context.Context, workspace, account string) (OperatorConfig, string, error) {
	config, err := h.workspaceModelConfig(ctx, workspace, sdk.ActorRef{Kind: "human", ID: account}, h.config.Operator)
	model := h.config.Media.Model
	if model == "" {
		model = defaultWrenImageModel
	}
	if err != nil {
		return config, model, err
	}
	if config.APIKey == "" {
		return config, model, fmt.Errorf("image provider is not configured")
	}
	if config.BaseURL != "" && strings.TrimRight(config.BaseURL, "/") != "https://openrouter.ai/api/v1" {
		return config, model, fmt.Errorf("configured provider does not support this image operation")
	}
	return config, model, nil
}

// GenerateWrenMedia accepts a single bounded image. A durable claim precedes any
// paid request; an uncertain physical request is never silently sent again.
func (h *HTTPServer) GenerateWrenMedia(ctx context.Context, c Conversation, run OperatorRun, in WrenMediaInput) (WrenMediaOperation, error) {
	s := h.service.Store
	if err := validateMediaInput(&in); err != nil {
		return WrenMediaOperation{}, err
	}
	if err := s.RequireWorkspaceWriter(ctx, c.WorkspaceID, c.AccountID); err != nil {
		return WrenMediaOperation{}, err
	}
	if err := s.ValidateWrenLifecycleConversation(ctx, c.ID, ""); err != nil {
		return WrenMediaOperation{}, err
	}
	if err := artifactAccess(ctx, s.pool, c.WorkspaceID, c.AccountID, c.ID, false); err != nil {
		return WrenMediaOperation{}, err
	}
	authority := ""
	var err error
	if run.ID != "" {
		authority, err = s.mediaAuthority(ctx, run, c)
		if err != nil {
			return WrenMediaOperation{}, err
		}
	}
	config, model, err := h.mediaConfig(ctx, c.WorkspaceID, c.AccountID)
	if err != nil {
		return WrenMediaOperation{}, err
	}
	var source WrenArtifact
	if in.SourceArtifactID != "" {
		source, err = s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, in.SourceArtifactID, in.ExpectedRevision)
		if err != nil {
			return WrenMediaOperation{}, err
		}
		if source.ConversationID != c.ID {
			return WrenMediaOperation{}, ErrForbidden
		}
		if _, _, err = mediaImage(source.Content); err != nil {
			return WrenMediaOperation{}, err
		}
		if in.Mode == "edit" && source.LatestRevision != in.ExpectedRevision {
			return WrenMediaOperation{}, ErrConflict
		}
	}
	hashInput := in
	hashInput.OperationKey = ""
	visualInput := hashInput
	visualInput.Name = ""
	visualHash := mediaDigest(struct {
		Input WrenMediaInput
		Model string
	}{visualInput, model})
	hash := mediaDigest(struct {
		Input WrenMediaInput
		Model string
	}{hashInput, model})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenMediaOperation{}, err
	}
	defer tx.Rollback(ctx)
	var identity string
	if err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 AND disabled_at IS NULL FOR KEY SHARE`, c.AccountID).Scan(&identity); err != nil {
		return WrenMediaOperation{}, ErrForbidden
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, c.WorkspaceID).Scan(&identity); err != nil {
		return WrenMediaOperation{}, ErrForbidden
	}
	if err = requireWorkspaceWriter(ctx, tx, c.WorkspaceID, c.AccountID, true); err != nil {
		return WrenMediaOperation{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,52))`, c.AccountID); err != nil {
		return WrenMediaOperation{}, err
	}
	existing, e := scanMedia(tx.QueryRow(ctx, `SELECT `+mediaColumns+` FROM wren_media_operations WHERE account_id=$1 AND operation_key=$2`, c.AccountID, in.OperationKey))
	if e == nil {
		if existing.RequestSHA256 != hash || existing.ConversationID != c.ID {
			return existing, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(e, ErrNotFound) {
		return existing, e
	}
	// Repeated model attempts within the same accepted task share one receipt even
	// when the model invents a fresh operation key or filename.
	if authority != "" {
		existing, e = scanMedia(tx.QueryRow(ctx, `SELECT `+mediaColumns+` FROM wren_media_operations WHERE account_id=$1 AND authority_run_id=$2 AND conversation_id=$3 AND visual_sha256=$4 ORDER BY created_at DESC LIMIT 1`, c.AccountID, authority, c.ID, visualHash))
		if e == nil {
			return existing, nil
		}
		if !errors.Is(e, ErrNotFound) {
			return existing, e
		}
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM wren_media_operations WHERE account_id=$1 AND created_at>now()-interval '1 hour'`, c.AccountID).Scan(&count); err != nil {
		return existing, err
	}
	if count >= wrenMediaMaxCallsPerHour {
		return existing, ErrCapacity
	}
	if err = artifactAccess(ctx, tx, c.WorkspaceID, c.AccountID, c.ID, true); err != nil {
		return existing, err
	}
	if err = requireWorkspaceWriter(ctx, tx, c.WorkspaceID, c.AccountID, true); err != nil {
		return existing, err
	}
	if err = validateWrenLifecycleTx(ctx, tx, c.ID, ""); err != nil {
		return existing, err
	}
	id, err := newID("media_")
	if err != nil {
		return existing, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO wren_media_operations(id,workspace_id,conversation_id,account_id,run_id,authority_run_id,operation_key,request_sha256,model,mode,name,prompt,source_artifact_id,source_revision,state,visual_sha256) VALUES($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14,'running',$15)`, id, c.WorkspaceID, c.ID, c.AccountID, run.ID, authority, in.OperationKey, hash, model, in.Mode, in.Name, in.Prompt, in.SourceArtifactID, in.ExpectedRevision, visualHash)
	if err != nil {
		return existing, err
	}
	if err = tx.Commit(ctx); err != nil {
		return existing, err
	}
	var usage json.RawMessage
	providerID := ""
	fail := func(state, reason string) (WrenMediaOperation, error) {
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, e := s.pool.Exec(save, `UPDATE wren_media_operations SET state=$2,failure=$3,usage=$4,provider_request_id=$5,updated_at=now() WHERE id=$1`, id, state, reason, mediaUsage(usage), providerID)
		if e != nil {
			return WrenMediaOperation{}, e
		}
		return s.WrenMediaOperation(save, c.WorkspaceID, c.AccountID, id)
	}
	size := "1024x1024"
	if in.Shape == "landscape" {
		size = "1536x1024"
	}
	if in.Shape == "portrait" {
		size = "1024x1536"
	}
	background := "opaque"
	if in.Transparent {
		background = "transparent"
	}
	payload := map[string]any{"model": model, "prompt": in.Prompt, "n": 1, "quality": "low", "size": size, "output_format": "png", "background": background, "provider": map[string]any{"allow_fallbacks": false}}
	if len(source.Content) > 0 {
		payload["input_references"] = []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + source.MediaType + ";base64," + base64.StdEncoding.EncodeToString(source.Content)}}}
	}
	body, _ := json.Marshal(payload)
	paid, err := h.BeginWrenAIPaidRequest(ctx, WrenAIReservationInput{WorkspaceID: c.WorkspaceID, AccountID: c.AccountID, ConversationID: c.ID, RunID: run.ID, RunLease: run.Lease, RequestKey: id, Kind: "media", Model: model}, WrenAIRequestEstimate{Kind: "media", Model: model, InputBytes: len(body)})
	if err != nil {
		return fail("failed", "Image request was not sent: AI allowance or cost policy is unavailable")
	}
	defer func() { _ = paid.Finish(context.WithoutCancel(ctx), usage, providerID) }()
	timeout := h.config.Media.timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://openrouter.ai/api/v1/images", bytes.NewReader(body))
	if err != nil {
		return fail("failed", "Could not prepare image request")
	}
	request.Header.Set("Authorization", "Bearer "+config.APIKey)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout, Transport: h.config.Media.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err = ctx.Err(); err != nil {
		return fail("failed", "Image request was cancelled before dispatch")
	}
	if err = s.RequireWorkspaceWriter(ctx, c.WorkspaceID, c.AccountID); err != nil {
		return fail("failed", "Image write permission was revoked before dispatch")
	}
	if run.ID != "" {
		if _, err = s.mediaAuthority(ctx, run, c); err != nil {
			return fail("failed", "Image authority was revoked before dispatch")
		}
	}
	if err = s.ValidateWrenLifecycleConversation(ctx, c.ID, ""); err != nil {
		return fail("failed", "Image request was not sent: this Wren or room was archived")
	}
	if err = paid.MarkDispatched(ctx); err != nil {
		return fail("failed", "Image request was not sent: AI allowance was revoked")
	}
	response, err := client.Do(request)
	if err != nil {
		return fail("unknown", "Provider request may have been accepted; no image receipt was received. It will not be retried automatically")
	}
	defer response.Body.Close()
	providerID = response.Header.Get("X-Request-ID")
	raw, err := io.ReadAll(io.LimitReader(response.Body, 12<<20))
	if err != nil {
		return fail("unknown", "Provider response was interrupted; image outcome is unknown")
	}
	var receipt struct {
		Data []struct {
			Base64    string `json:"b64_json"`
			MediaType string `json:"media_type"`
		} `json:"data"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &receipt) == nil {
		usage = receipt.Usage
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fail("failed", fmt.Sprintf("Image provider returned HTTP %d; no image was published", response.StatusCode))
	}
	if len(receipt.Data) != 1 || receipt.Data[0].MediaType != "image/png" {
		return fail("failed", "Provider did not return exactly one actual PNG image")
	}
	data, err := base64.StdEncoding.DecodeString(receipt.Data[0].Base64)
	if err != nil {
		return fail("failed", "Provider image encoding was invalid")
	}
	width, height, err := mediaImage(data)
	if err != nil {
		return fail("failed", err.Error())
	}
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return fail("failed", "Provider output was not the requested PNG format")
	}
	digest := sha256.Sum256(data)
	if len(usage) == 0 || !json.Valid(usage) {
		usage = json.RawMessage(`{}`)
	}
	save, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer saveCancel()
	_, err = s.pool.Exec(save, `UPDATE wren_media_operations SET output_bytes=$2,width=$3,height=$4,sha256=$5,usage=$6,provider_request_id=$7,state='ready',updated_at=now() WHERE id=$1`, id, data, width, height, hex.EncodeToString(digest[:]), usage, providerID)
	if err != nil {
		return WrenMediaOperation{}, err
	}
	if run.ID != "" {
		if _, err = s.mediaAuthority(ctx, run, c); err != nil {
			return fail("blocked", "Image bytes were retained privately; the originating run is no longer authorized to publish")
		}
	}
	return h.publishWrenMedia(ctx, c.WorkspaceID, c.AccountID, id, false, run)
}

func (h *HTTPServer) publishWrenMedia(ctx context.Context, workspace, account, id string, recoverAsNew bool, sourceRuns ...OperatorRun) (WrenMediaOperation, error) {
	s := h.service.Store
	snapshot, err := s.WrenMediaOperation(ctx, workspace, account, id)
	if err != nil {
		return snapshot, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(ctx)
	// Match account deletion and the shared authority fence before taking the
	// operation and artifact locks. No nested pool query/transaction is needed.
	var identity string
	if err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 AND disabled_at IS NULL FOR KEY SHARE`, account).Scan(&identity); err != nil {
		return snapshot, ErrForbidden
	}
	runID, lease := "", ""
	if !recoverAsNew && len(sourceRuns) > 0 {
		runID = sourceRuns[0].ID
		lease = wrenAIRunLeaseHash(sourceRuns[0].Lease)
	}
	if err = s.lockWrenAIDispatchAuthority(ctx, tx, workspace, account, snapshot.ConversationID, runID, lease, "", "media", nil); err != nil {
		return snapshot, err
	}
	op, err := scanMedia(tx.QueryRow(ctx, `SELECT `+mediaColumns+` FROM wren_media_operations WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND conversation_id=$4 FOR UPDATE`, id, workspace, account, snapshot.ConversationID))
	if err != nil {
		return op, err
	}
	if op.State == "completed" {
		return op, tx.Commit(ctx)
	}
	if len(op.Output) == 0 {
		return op, ErrConflict
	}
	if !recoverAsNew && op.RunID != "" && runID != op.RunID {
		return op, ErrForbidden
	}
	readArtifact := func(artifactID string, revision int) (WrenArtifact, error) {
		return scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactMetadataColumns+`,v.content FROM wren_artifacts a JOIN wren_artifact_revisions v ON v.artifact_id=a.id WHERE a.id=$1 AND a.workspace_id=$2 AND a.conversation_id=$3 AND v.revision=$4`, artifactID, workspace, op.ConversationID, revision), true)
	}
	if op.SourceArtifactID != "" {
		if _, err = readArtifact(op.SourceArtifactID, op.SourceRevision); err != nil {
			return op, err
		}
	}
	finish := func(artifact WrenArtifact) (WrenMediaOperation, error) {
		e := tx.QueryRow(ctx, `UPDATE wren_media_operations SET state='completed',artifact_id=$2,artifact_revision=$3,output_bytes=NULL,failure='',updated_at=now() WHERE id=$1 RETURNING updated_at`, op.ID, artifact.ID, artifact.Revision).Scan(&op.UpdatedAt)
		if e != nil {
			return op, e
		}
		if e = tx.Commit(ctx); e != nil {
			return op, e
		}
		op.State = "completed"
		op.ArtifactID = artifact.ID
		op.ArtifactRevision = artifact.Revision
		op.Output = nil
		op.Recoverable = false
		op.Failure = ""
		return op, nil
	}
	summary := "Image generation operation " + op.ID
	// Reconcile old receipts from the former split transaction implementation.
	// The origin and operation locks also fence concurrent initial/recovery calls.
	if op.Mode == "edit" {
		already, e := readArtifact(op.SourceArtifactID, op.SourceRevision+1)
		if e == nil && already.SHA256 == op.SHA256 && already.Provenance.Summary == summary {
			return finish(already)
		}
		if e != nil && !errors.Is(e, ErrNotFound) {
			return op, e
		}
	}
	artifactID := "artifact_" + mediaDigest(op.ID)[:32]
	expected := 0
	if op.Mode == "edit" && !recoverAsNew {
		artifactID = op.SourceArtifactID
		expected = op.SourceRevision
	}
	provenance := WrenArtifactProvenance{RunID: op.RunID, Summary: summary}
	if op.RunID != "" {
		var job string
		e := tx.QueryRow(ctx, `SELECT id FROM wren_jobs WHERE child_run_id=$1`, op.RunID).Scan(&job)
		if e == nil {
			provenance.JobID = job
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return op, e
		}
	}
	// Savepoint rolls back any partial artifact mutation when validation fails;
	// the outer transaction can still retain a truthful recoverable receipt.
	write, err := tx.Begin(ctx)
	if err != nil {
		return op, err
	}
	artifact, err := s.putWrenArtifactTx(ctx, write, workspace, account, WrenArtifactInput{ID: artifactID, ConversationID: op.ConversationID, ExpectedRevision: expected, Name: op.Name, MediaType: "image/png", Content: op.Output, Provenance: provenance})
	if err != nil {
		if e := write.Rollback(ctx); e != nil {
			return op, e
		}
		if errors.Is(err, ErrConflict) {
			a, e := readArtifact(artifactID, expected+1)
			if e == nil && a.SHA256 == op.SHA256 && a.Provenance.Summary == summary {
				return finish(a)
			}
		}
		state := "blocked"
		if errors.Is(err, ErrConflict) {
			state = "conflict"
		}
		failure := "Generated bytes retained privately; publication requires current access and a matching artifact revision"
		if _, e := tx.Exec(ctx, `UPDATE wren_media_operations SET state=$2,failure=$3,updated_at=now() WHERE id=$1`, op.ID, state, failure); e != nil {
			return op, e
		}
		if e := tx.Commit(ctx); e != nil {
			return op, e
		}
		op.State = state
		op.Failure = failure
		op.Recoverable = true
		return op, nil
	}
	if err = write.Commit(ctx); err != nil {
		return op, err
	}
	return finish(artifact)
}

func mediaUsage(v json.RawMessage) json.RawMessage {
	if len(v) == 0 || !json.Valid(v) {
		return json.RawMessage(`{}`)
	}
	return v
}

var mediaClauseSeparator = regexp.MustCompile(`[.;!?\n]|\b(?:but|however|instead)\b`)
var mediaInstructionWords = regexp.MustCompile(`[a-z]+(?:'[a-z]+)?`)

// This conservative lexical check is not semantic authorization. It only
// recognizes direct visual requests in accepted human/standing source records;
// ambiguous prose must result in needs-image-request without a paid call.
func wrenMediaInstructionAllows(instruction string) bool {
	text := strings.ToLower(strings.ReplaceAll(instruction, "’", "'"))
	actions := map[string]bool{"generate": true, "create": true, "draw": true, "make": true, "design": true, "edit": true, "change": true, "render": true, "produce": true}
	visuals := map[string]bool{"image": true, "images": true, "illustration": true, "illustrations": true, "picture": true, "pictures": true, "logo": true, "logos": true, "artwork": true, "poster": true, "posters": true, "icon": true, "icons": true, "photo": true, "photos": true, "portrait": true, "portraits": true, "banner": true, "banners": true}
	textObjects := map[string]bool{"readme": true, "code": true, "docs": true, "documentation": true, "document": true, "documents": true, "text": true, "prose": true, "description": true, "descriptions": true, "caption": true, "captions": true, "metadata": true, "css": true, "html": true, "markdown": true, "script": true, "instructions": true, "tutorial": true, "guide": true, "filename": true, "alt": true, "prompt": true, "prompts": true, "specification": true, "url": true, "link": true, "component": true, "tag": true, "test": true, "tests": true, "function": true}
	allowed := false
	for _, clause := range mediaClauseSeparator.Split(text, -1) {
		words := mediaInstructionWords.FindAllString(clause, -1)
		clauseAllowed := false
		blocked := false
		for i, word := range words {
			if !actions[word] && word != "update" && word != "write" {
				continue
			}
			negated := false
			ambiguous := false
			for _, before := range words[:i] {
				if before == "don't" || before == "dont" || before == "never" || before == "not" || before == "no" {
					negated = true
				}
				if before == "how" || before == "if" || before == "whether" || before == "explain" || before == "describe" {
					ambiguous = true
				}
			}
			visualObject := false
			objectAt := -1
			for j := i + 1; j < len(words) && j <= i+8; j++ {
				w := words[j]
				if textObjects[w] {
					blocked = true
					break
				}
				if visuals[w] {
					visualObject = true
					objectAt = j
					break
				}
				if actions[w] || w == "for" || w == "about" || w == "describing" || w == "describe" || w == "explain" || w == "explaining" || w == "to" || w == "with" {
					break
				}
			}
			if visualObject && negated {
				return false
			}
			if visualObject && objectAt+1 < len(words) && textObjects[words[objectAt+1]] {
				blocked = true
			}
			if visualObject && actions[word] && !negated && !ambiguous {
				clauseAllowed = true
			}
		}
		if clauseAllowed && !blocked {
			allowed = true
		}
	}
	return allowed
}
