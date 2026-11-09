package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWrenConnectorCatalogManifestsAndQueryEncoding(t *testing.T) {
	for _, entry := range wrenConnectorCatalog() {
		input := WrenExtensionInput{ID: "extension_catalog_test", Manifest: entry.Manifest}
		if err := validateExtension(&input, WrenExtensionConfig{}); err != nil {
			t.Fatalf("%s: %v", entry.Slug, err)
		}
		for _, tool := range input.Manifest.Tools {
			if (tool.Method != "GET" && tool.Method != "MCP") || tool.Impact != "read" {
				t.Fatal("catalog capability must match public read promise")
			}
		}
	}
	encoded, err := extensionQueryArguments(json.RawMessage(`{"q":"a&admin=true + 日本語","rows":3,"flag":false}`))
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.ParseQuery(encoded)
	if parsed.Get("q") != "a&admin=true + 日本語" || parsed.Get("admin") != "" || parsed.Get("rows") != "3" || parsed.Get("flag") != "false" {
		t.Fatalf("query injection or corruption: %s", encoded)
	}
	for _, value := range []string{`{"nested":{}}`, `{"array":[]}`, `{"null":null}`, `{"q":"` + strings.Repeat("x", 8193) + `"}`} {
		if _, err := extensionQueryArguments(json.RawMessage(value)); err == nil {
			t.Fatal("accepted unsupported query")
		}
	}
	input := WrenExtensionInput{ID: "extension_catalog_test", Manifest: wrenConnectorCatalog()[0].Manifest}
	input.Manifest.Tools[0].Impact = "write"
	if validateExtension(&input, WrenExtensionConfig{}) == nil {
		t.Fatal("GET granted writes")
	}
}

func TestWrenConnectorCatalogPreparationIsSharedButInstallationIsScoped(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	owner, err := s.prepareWrenCatalogConnector(ctx, private.WorkspaceID, private.AccountID, "github-public")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := s.prepareWrenCatalogConnector(ctx, private.WorkspaceID, member, "github-public")
	if err != nil || peer.ID != owner.ID {
		t.Fatalf("shared preparation: %v", err)
	}
	scope := WrenKnowledgeScope{Scope: "room", ConversationID: shared.ID}
	installs, err := s.WrenExtensionInstallations(ctx, private.WorkspaceID, member, scope)
	if err != nil || len(installs) != 0 {
		t.Fatal("preparation granted room access")
	}
	install, err := s.InstallWrenExtension(ctx, private.WorkspaceID, member, WrenExtensionInstallationInput{ID: "extinstall_catalog_test", WrenKnowledgeScope: scope, ExtensionID: owner.ID, Version: 1, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	seen, err := s.WrenExtensionInstallation(ctx, private.WorkspaceID, private.AccountID, install.ID)
	if err != nil || !seen.Enabled || seen.AllowWrite || seen.Extension.Manifest.Auth != "none" {
		t.Fatalf("room setup mismatch %v", err)
	}
	mutation := WrenExtensionInput{ID: owner.ID, Manifest: owner.Manifest, ExpectedVersion: 1}
	mutation.Manifest.Name = "Impersonated catalog update"
	if _, err = s.SaveWrenExtension(ctx, private.WorkspaceID, private.AccountID, mutation); err == nil {
		t.Fatal("catalog editable by workspace author")
	}
	if _, err = s.prepareWrenCatalogConnector(ctx, "other-workspace", member, "github-public"); err == nil {
		t.Fatal("cross workspace catalog access")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	if _, err = s.prepareWrenCatalogConnector(ctx, private.WorkspaceID, member, "crossref"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer prepared package %v", err)
	}
}

func TestWrenConnectorGETUsesActualQueryAndFencesRevokedRoomMember(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("query") == "rate-limit" {
			w.WriteHeader(429)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method != "GET" || len(body) != 0 || r.URL.Query().Get("query") != "a&injected=yes" || r.URL.Query().Get("injected") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("wrong real GET request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"answer":"Actual public lookup"}`)
	}))
	defer server.Close()
	h, private, shared, member, install := extensionFixture(t, server.URL, "none")
	s := h.service.Store
	ctx := context.Background()
	manifest := install.Extension.Manifest
	manifest.Tools[0].Method = "GET"
	ext, err := s.saveWrenExtension(ctx, private.WorkspaceID, private.AccountID, WrenExtensionInput{ID: install.ExtensionID, ExpectedVersion: 1, Manifest: manifest}, h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	edit := extensionInstallEdit(install)
	edit.Version = ext.Version
	install, err = s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	call, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "public-query", json.RawMessage(`{"query":"a&injected=yes"}`))
	if err != nil || call.Status != "completed" || hits.Load() != 1 {
		t.Fatalf("GET outcome %+v %v", call, err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "public-query", json.RawMessage(`{"query":"a&injected=yes"}`)); err != nil || hits.Load() != 1 {
		t.Fatal("replay made another request")
	}
	limited, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "rate-limited-read", json.RawMessage(`{"query":"rate-limit"}`))
	if err != nil || limited.Status != "failed" || limited.HTTPStatus != 429 || !strings.Contains(limited.Failure, "rate limiting") {
		t.Fatalf("read rate limit misreported: %+v %v", limited, err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM wren_room_members WHERE room_id=(SELECT id FROM wren_rooms WHERE conversation_id=$1) AND account_id=$2`, shared.ID, member); err != nil {
		t.Fatal(err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "revoked-query", json.RawMessage(`{"query":"a&injected=yes"}`)); err == nil || hits.Load() != 2 {
		t.Fatal("removed room member reached provider")
	}
}
