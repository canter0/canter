package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/canter0/canter/internal/envfile"
)

// Explicitly opt-in, isolated test database only. This performs one real paid
// image request through the authenticated application operation and downloads
// the persisted artifact. Ordinary tests never contact a real image provider.
func TestWrenMediaLiveAuthorizedGeneration(t *testing.T) {
	if os.Getenv("CANTER_MEDIA_LIVE_AUTHORIZED") != "true" {
		t.Skip("real image request requires explicit opt-in")
	}
	if _, err := envfile.Load(); err != nil {
		t.Fatal("could not load configured provider")
	}
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Fatal("existing provider key is not configured")
	}
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	if err := s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000000); err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: key}, Media: WrenMediaConfig{}, AIUsage: WrenAIUsageConfig{Enforce: true, KindEstimatesMicroUSD: map[string]int64{"media": 250000}}}).(*HTTPServer)
	input := map[string]any{"conversationId": c.ID, "operationKey": "authorized-live-image-once", "prompt": "A simple small friendly dark blue wren bird silhouette on a plain warm white background, a clean polished icon, no letters, no text, no shadows.", "name": "wren-live-image.png", "shape": "square"}
	outputName := "wren-live-image.png"
	receiptName := "real-media-receipt.json"
	if os.Getenv("CANTER_MEDIA_LIVE_EDIT") == "true" {
		original, err := os.ReadFile(filepath.Join("..", "..", "artifacts", "wrens-media", "wren-live-image.png"))
		if err != nil {
			t.Fatal(err)
		}
		source, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ConversationID: c.ID, Name: "actual-generated-source.png", MediaType: "image/png", Content: original, Provenance: WrenArtifactProvenance{Summary: "Imported actual PNG from the previously verified authorized generation"}})
		if err != nil {
			t.Fatal(err)
		}
		input["mode"] = "edit"
		input["sourceArtifactId"] = source.ID
		input["expectedRevision"] = 1
		input["prompt"] = "Keep this same bird silhouette and layout; change the blue bird to warm orange. No letters or text."
		input["name"] = "wren-live-edited.png"
		outputName = "wren-live-edited.png"
		receiptName = "real-media-edit-receipt.json"
	}
	body, _ := json.Marshal(input)
	request := httptest.NewRequest("POST", "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/wren-media", bytes.NewReader(body))
	request.Header.Set("Origin", "http://canter.test")
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	var op WrenMediaOperation
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &op) != nil || op.State != "completed" {
		t.Fatalf("real application generation failed: HTTP %d state=%s failure=%s", response.Code, op.State, op.Failure)
	}
	artifact, err := s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, op.ArtifactID, op.ArtifactRevision)
	if err != nil {
		t.Fatal(err)
	}
	download := httptest.NewRequest("GET", "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/wren-artifacts/"+op.ArtifactID+"/content", nil)
	download.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	downloaded := httptest.NewRecorder()
	h.ServeHTTP(downloaded, download)
	if downloaded.Code != 200 || !bytes.Equal(downloaded.Body.Bytes(), artifact.Content) || downloaded.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("authenticated real artifact download failed", downloaded.Code)
	}
	dir := filepath.Join("..", "..", "artifacts", "wrens-media")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, outputName), artifact.Content, 0600); err != nil {
		t.Fatal(err)
	}
	budget, err := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, _ := json.MarshalIndent(map[string]any{"operation": op, "artifact": artifact, "allowance": budget}, "", "  ")
	if err = os.WriteFile(filepath.Join(dir, receiptName), evidence, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("real provider PNG: %dx%d %d bytes sha256=%s operation=%s actual spent microUSD=%d held=%d", op.Width, op.Height, artifact.Size, artifact.SHA256, op.ID, budget.SpentMicroUSD, budget.HeldMicroUSD)
}
