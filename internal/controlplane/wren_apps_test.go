package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"
)

func TestWrenAppsReportsLiveConversationPauseAndResumeWithoutRemoteCalls(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	h, _, c, _, install := extensionFixture(t, server.URL, "none")
	ctx := context.Background()
	s := h.service.Store
	run, err := s.EnqueueOperator(ctx, c, "apps-status", "Inspect our connections", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string, usable bool) {
		t.Helper()
		items, e := h.wrenAppConnectionStatuses(ctx, c, run)
		if e != nil || len(items) != 1 {
			t.Fatalf("status: %+v %v", items, e)
		}
		if items[0].State != want || !items[0].CanManage && want != "removed" {
			t.Fatalf("wrong status %+v", items[0])
		}
		available := false
		for _, tool := range items[0].Tools {
			available = available || tool.CanInvoke
		}
		if available != usable {
			t.Fatalf("status grants incorrect tool availability %+v", items[0])
		}
	}
	check("available", true)
	edit := extensionInstallEdit(install)
	edit.Enabled = false
	install, err = s.InstallWrenExtension(ctx, c.WorkspaceID, c.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	check("paused", false)
	tools, err := h.WrenExtensionTools(ctx, c, run)
	if err != nil || len(tools) != 0 {
		t.Fatalf("paused tools advertised %+v %v", tools, err)
	}
	edit = extensionInstallEdit(install)
	edit.Enabled = true
	install, err = s.InstallWrenExtension(ctx, c.WorkspaceID, c.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	check("available", true)
	edit = extensionInstallEdit(install)
	edit.Revoke = true
	if _, err = s.InstallWrenExtension(ctx, c.WorkspaceID, c.AccountID, edit); err != nil {
		t.Fatal(err)
	}
	check("removed", false)
	if hits.Load() != 0 {
		t.Fatal("saved-state discovery called the remote service")
	}
}

func TestWrenAppsStatusUsesRequesterCredentialWithoutDisclosingIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("status lookup made a remote request") }))
	defer server.Close()
	h, _, owner, member, install := extensionFixture(t, server.URL, "bearer")
	ctx := context.Background()
	s := h.service.Store
	const secret = "owner-only-secret-never-return"
	if _, err := h.ConfigureWrenExtensionCredential(ctx, owner.WorkspaceID, owner.AccountID, install.ID, WrenExtensionCredentialInput{Value: secret}); err != nil {
		t.Fatal(err)
	}
	ownerRun, err := s.EnqueueOperator(ctx, owner, "owner-apps-status", "Inspect apps", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := h.wrenAppConnectionStatuses(ctx, owner, ownerRun)
	if err != nil || len(items) != 1 || items[0].State != "available" {
		t.Fatalf("owner status %+v %v", items, err)
	}
	peer, err := s.Conversation(ctx, owner.WorkspaceID, member, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	peerRun, err := s.EnqueueOperator(ctx, peer, "peer-apps-status", "Inspect apps", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err = h.wrenAppConnectionStatuses(ctx, peer, peerRun)
	if err != nil || len(items) != 1 || items[0].State != "needs_sign_in" || items[0].CanManage {
		t.Fatalf("peer inherited owner access %+v %v", items, err)
	}
	for _, tool := range items[0].Tools {
		if tool.CanInvoke {
			t.Fatal("peer inherited credential")
		}
	}
	raw, _ := json.Marshal(items)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), owner.AccountID) || strings.Contains(string(raw), member) {
		t.Fatalf("private account data in shared status: %s", raw)
	}
	peer.AccountID = "foreign-account"
	if _, err = h.wrenAppConnectionStatuses(ctx, peer, peerRun); err == nil {
		t.Fatal("foreign requester read connection state")
	}
}

func TestWrenAppsDiscoveryDoesNotGrantAccessOrExposeCredentials(t *testing.T) {
	s, c, run := wrenJobFixture(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", GitHubOAuth: OAuthCredentials{ClientID: "test-client", ClientSecret: "test-secret"}}).(*HTTPServer)
	o := &OperatorRuntime{Server: h}
	if err := h.saveGitHubConnection(ctx, c.AccountID, c.WorkspaceID, "github-test", "private-login", "repo", &oauth2.Token{AccessToken: "never-return-this-token"}); err != nil {
		t.Fatal(err)
	}
	value, handled, err := o.localTool(ctx, run, c, Principal{}, "canter_show_connections", json.RawMessage(`{}`))
	if err != nil || !handled {
		t.Fatalf("discover: %v %v", handled, err)
	}
	result := value.(map[string]any)
	choices := result["publicSources"].([]map[string]string)
	if len(choices) != len(wrenConnectorCatalog()) {
		t.Fatalf("missing catalog choices %+v", choices)
	}
	for i, choice := range choices {
		if choice["slug"] != wrenConnectorCatalog()[i].Slug || choice["name"] == "" {
			t.Fatalf("wrong catalog target %+v", choice)
		}
	}
	raw, _ := json.Marshal(value)
	if !strings.Contains(string(raw), `"name":"Linear"`) || !strings.Contains(string(raw), `"connectedForRequester":true`) || strings.Contains(string(raw), "private-login") || strings.Contains(string(raw), "never-return-this-token") || strings.Contains(string(raw), "github-test") {
		t.Fatalf("unexpected discovery result: %s", raw)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_extension_installations WHERE conversation_id=$1`, c.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("discovery created a grant: %d %v", count, err)
	}
	var kind string
	if err = s.pool.QueryRow(ctx, `SELECT data->>'kind' FROM operator_events WHERE conversation_id=$1 AND kind='surface' ORDER BY sequence DESC LIMIT 1`, c.ID).Scan(&kind); err != nil || kind != "wren-extensions" {
		t.Fatalf("missing apps surface %q %v", kind, err)
	}
	// A stale/foreign conversation must fail before reading any account state.
	c.AccountID = "unrelated-account"
	if _, err = o.showWrenApps(ctx, c, run); err == nil {
		t.Fatal("foreign account could discover this conversation's apps")
	}
}
