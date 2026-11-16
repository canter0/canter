package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const defaultWrenImageModel = "openai/gpt-image-2.5-flare"
const wrenMediaMaxPixels = 4 * 1024 * 1024
const wrenMediaMaxCallsPerHour = 4

// The provider credential comes from the existing workspace model config.
// Model selection is server-owned and is never accepted from the client/model.
type WrenMediaConfig struct {
	Model     string
	transport http.RoundTripper
	timeout   time.Duration
}
type WrenMediaInput struct {
	OperationKey     string `json:"operationKey"`
	Mode             string `json:"mode"`
	Prompt           string `json:"prompt"`
	Name             string `json:"name"`
	SourceArtifactID string `json:"sourceArtifactId,omitempty"`
	ExpectedRevision int    `json:"expectedRevision,omitempty"`
	Shape            string `json:"shape,omitempty"`
	Transparent      bool   `json:"transparent,omitempty"`
}
type WrenMediaOperation struct {
	ID                string          `json:"id"`
	ConversationID    string          `json:"conversationId"`
	AccountID         string          `json:"accountId"`
	RunID             string          `json:"runId,omitempty"`
	AuthorityRunID    string          `json:"-"`
	OperationKey      string          `json:"operationKey"`
	RequestSHA256     string          `json:"-"`
	Model             string          `json:"model"`
	Mode              string          `json:"mode"`
	Name              string          `json:"name"`
	Prompt            string          `json:"prompt"`
	SourceArtifactID  string          `json:"sourceArtifactId,omitempty"`
	SourceRevision    int             `json:"sourceRevision,omitempty"`
	State             string          `json:"state"`
	Failure           string          `json:"failure,omitempty"`
	ProviderRequestID string          `json:"providerRequestId,omitempty"`
	Usage             json.RawMessage `json:"usage"`
	Width             int             `json:"width"`
	Height            int             `json:"height"`
	SHA256            string          `json:"sha256,omitempty"`
	ArtifactID        string          `json:"artifactId,omitempty"`
	ArtifactRevision  int             `json:"artifactRevision,omitempty"`
	Recoverable       bool            `json:"recoverable"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
	Output            []byte          `json:"-"`
}

const mediaColumns = `id,conversation_id,account_id,COALESCE(run_id,''),COALESCE(authority_run_id,''),operation_key,request_sha256,model,mode,name,prompt,COALESCE(source_artifact_id,''),source_revision,state,failure,provider_request_id,usage,width,height,sha256,COALESCE(artifact_id,''),artifact_revision,created_at,updated_at,output_bytes,(output_bytes IS NOT NULL AND state<>'completed')`

func scanMedia(row interface{ Scan(...any) error }) (WrenMediaOperation, error) {
	var v WrenMediaOperation
	err := row.Scan(&v.ID, &v.ConversationID, &v.AccountID, &v.RunID, &v.AuthorityRunID, &v.OperationKey, &v.RequestSHA256, &v.Model, &v.Mode, &v.Name, &v.Prompt, &v.SourceArtifactID, &v.SourceRevision, &v.State, &v.Failure, &v.ProviderRequestID, &v.Usage, &v.Width, &v.Height, &v.SHA256, &v.ArtifactID, &v.ArtifactRevision, &v.CreatedAt, &v.UpdatedAt, &v.Output, &v.Recoverable)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}
func mediaImage(data []byte) (int, int, error) {
	if len(data) == 0 || len(data) > WrenArtifactMaxBytes {
		return 0, 0, fmt.Errorf("image must contain at most 8 MiB of actual image bytes")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" && format != "jpeg" || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > wrenMediaMaxPixels {
		return 0, 0, fmt.Errorf("image dimensions are invalid or exceed the 4 megapixel limit")
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return 0, 0, fmt.Errorf("image bytes could not be decoded completely")
	}
	return cfg.Width, cfg.Height, nil
}
func validateMediaInput(v *WrenMediaInput) error {
	v.Prompt = strings.TrimSpace(v.Prompt)
	v.Name = strings.TrimSpace(v.Name)
	if v.Mode == "" {
		v.Mode = "generate"
	}
	if v.Shape == "" {
		v.Shape = "square"
	}
	if v.Mode != "generate" && v.Mode != "edit" || v.Shape != "square" && v.Shape != "landscape" && v.Shape != "portrait" {
		return fmt.Errorf("choose generate/edit and square/landscape/portrait")
	}
	if len(v.OperationKey) < 8 || len(v.OperationKey) > 220 || !artifactSafeText(v.OperationKey, 220) || !utf8.ValidString(v.Prompt) || len(v.Prompt) < 1 || len(v.Prompt) > 4000 || !artifactSafeText(v.Name, 180) || v.Name == "" || strings.ContainsAny(v.Name, "/\\") || !strings.HasSuffix(strings.ToLower(v.Name), ".png") {
		return fmt.Errorf("provide a stable request key, a prompt of up to 4000 bytes, and a PNG filename")
	}
	if v.Mode == "edit" && v.SourceArtifactID == "" || v.SourceArtifactID != "" && !validArtifactID(v.SourceArtifactID) || v.SourceArtifactID != "" && v.ExpectedRevision < 1 || v.SourceArtifactID == "" && v.ExpectedRevision != 0 {
		return fmt.Errorf("image references and edits require an actual source artifact and its exact revision")
	}
	return nil
}
func (s *Store) WrenMediaOperation(ctx context.Context, workspace, account, id string) (WrenMediaOperation, error) {
	v, err := scanMedia(s.pool.QueryRow(ctx, `SELECT `+mediaColumns+` FROM wren_media_operations WHERE id=$1 AND workspace_id=$2 AND account_id=$3`, id, workspace, account))
	if err != nil {
		return v, err
	}
	if err = artifactAccess(ctx, s.pool, workspace, account, v.ConversationID, false); err != nil {
		return WrenMediaOperation{}, err
	}
	return v, nil
}
func (s *Store) WrenMediaOperations(ctx context.Context, workspace, account, conversation string) ([]WrenMediaOperation, error) {
	if err := artifactAccess(ctx, s.pool, workspace, account, conversation, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+strings.Replace(mediaColumns, ",output_bytes,", ",NULL::bytea,", 1)+` FROM wren_media_operations WHERE workspace_id=$1 AND account_id=$2 AND conversation_id=$3 ORDER BY created_at DESC,id LIMIT 40`, workspace, account, conversation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WrenMediaOperation{}
	for rows.Next() {
		v, e := scanMedia(rows)
		if e != nil {
			return nil, e
		}
		v.Output = nil
		out = append(out, v)
	}
	return out, rows.Err()
}
