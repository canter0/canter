package controlplane

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

var errGitHubReconnect = errors.New("Reconnect GitHub to access this repository.")

type githubTokenKey struct{}

type githubConnection struct {
	Enabled    bool   `json:"enabled"`
	Connected  bool   `json:"connected"`
	Reconnect  bool   `json:"reconnect,omitempty"`
	Login      string `json:"login,omitempty"`
	Provider   string `json:"provider,omitempty"`
	AppEnabled bool   `json:"appEnabled"`
	InstallURL string `json:"installUrl,omitempty"`
}

type githubRepository struct {
	Name          string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// Domain separation makes this key independent of OAuth protocol secrets. A
// client-secret rotation intentionally requires users to reconnect repositories.
// Ciphertext is bound to its account and workspace; it cannot be moved between them.
func (h *HTTPServer) githubCipher(providers ...string) (cipher.AEAD, error) {
	secret := h.config.GitHubOAuth.ClientSecret
	domain := "canter:github-repository-token:v1\x00"
	if len(providers) > 0 && providers[0] == "github-app" {
		secret = h.config.GitHubApp.ClientSecret
		domain = "canter:github-app-repository-token:v1\x00"
	}
	if secret == "" {
		return nil, errGitHubReconnect
	}
	key := sha256.Sum256([]byte(domain + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func githubTokenBinding(account, workspace string) []byte {
	return []byte(account + "\x00" + workspace)
}

func (h *HTTPServer) saveGitHubConnection(ctx context.Context, account, workspace, id, login, scope string, token *oauth2.Token) error {
	return h.saveGitHubProviderConnection(ctx, account, workspace, id, login, scope, "github", token)
}

func (h *HTTPServer) saveGitHubProviderConnection(ctx context.Context, account, workspace, id, login, scope, provider string, token *oauth2.Token) error {
	access, refresh, refreshExpiry, err := h.sealGitHubTokens(provider, account, workspace, token)
	if err != nil {
		return err
	}
	var expires *time.Time
	if !token.Expiry.IsZero() {
		expires = &token.Expiry
	}
	_, err = h.service.Store.pool.Exec(ctx, `INSERT INTO github_repository_connections(account_id,workspace_id,github_id,login,encrypted_token,scopes,expires_at,auth_provider,encrypted_refresh_token,refresh_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(account_id,workspace_id) DO UPDATE SET github_id=EXCLUDED.github_id,login=EXCLUDED.login,encrypted_token=EXCLUDED.encrypted_token,scopes=EXCLUDED.scopes,expires_at=EXCLUDED.expires_at,auth_provider=EXCLUDED.auth_provider,encrypted_refresh_token=EXCLUDED.encrypted_refresh_token,refresh_expires_at=EXCLUDED.refresh_expires_at,updated_at=now()`, account, workspace, id, login, access, scope, expires, provider, refresh, refreshExpiry)
	return err
}

func (h *HTTPServer) githubAccess(ctx context.Context, account, workspace string) (githubConnection, string, error) {
	state := h.githubConnectionDefaults()
	// Serialize rotation against other reads, disconnects and reconnects. Refresh
	// tokens are single-use; two callers must never redeem the same token.
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return state, "", err
	}
	defer tx.Rollback(ctx)
	var sealed, refresh []byte
	var expires, refreshExpires *time.Time
	err = tx.QueryRow(ctx, `SELECT login,encrypted_token,expires_at,auth_provider,encrypted_refresh_token,refresh_expires_at FROM github_repository_connections WHERE account_id=$1 AND workspace_id=$2 FOR UPDATE`, account, workspace).Scan(&state.Login, &sealed, &expires, &state.Provider, &refresh, &refreshExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, "", nil
	}
	if err != nil {
		return state, "", err
	}
	aead, err := h.githubCipher(state.Provider)
	if err != nil || h.oauth[state.Provider] == nil {
		state.Reconnect = true
		return state, "", nil
	}
	plain, err := openGitHubToken(aead, sealed, githubTokenBinding(account, workspace))
	if err != nil {
		state.Reconnect = true
		return state, "", nil
	}
	if expires != nil && !expires.After(time.Now().Add(time.Minute)) {
		if state.Provider != "github-app" || refreshExpires == nil || !refreshExpires.After(time.Now()) {
			state.Reconnect = true
			return state, "", nil
		}
		refreshToken, err := openGitHubToken(aead, refresh, githubRefreshBinding(account, workspace))
		if err != nil {
			state.Reconnect = true
			return state, "", nil
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		refreshCtx = context.WithValue(refreshCtx, oauth2.HTTPClient, oauthHTTPClient)
		// Force renewal inside the safety window rather than returning a token
		// that could expire during an artifact read.
		token, err := h.oauth["github-app"].config.TokenSource(refreshCtx, &oauth2.Token{RefreshToken: string(refreshToken), Expiry: time.Now().Add(-time.Hour)}).Token()
		if err != nil {
			var rejected *oauth2.RetrieveError
			if errors.As(err, &rejected) && (rejected.ErrorCode == "bad_refresh_token" || rejected.ErrorCode == "invalid_grant" || rejected.ErrorCode == "incorrect_client_credentials") {
				state.Reconnect = true
				return state, "", nil
			}
			return state, "", fmt.Errorf("GitHub token renewal is temporarily unavailable; try again")
		}
		access, rotated, rotatedExpiry, err := h.sealGitHubTokens(state.Provider, account, workspace, token)
		if err != nil {
			state.Reconnect = true
			return state, "", nil
		}
		_, err = tx.Exec(ctx, `UPDATE github_repository_connections SET encrypted_token=$3,encrypted_refresh_token=$4,expires_at=$5,refresh_expires_at=$6,updated_at=now() WHERE account_id=$1 AND workspace_id=$2`, account, workspace, access, rotated, token.Expiry, rotatedExpiry)
		if err != nil {
			return state, "", err
		}
		plain = []byte(token.AccessToken)
	}
	if err := tx.Commit(ctx); err != nil {
		return state, "", err
	}
	state.Connected = true
	return state, string(plain), nil
}

func (h *HTTPServer) githubReturn(w http.ResponseWriter, r *http.Request, next, result string) {
	u, _ := url.Parse(safeOAuthNext(next, "repository"))
	query := u.Query()
	query.Set("github", result)
	u.RawQuery = query.Encode()
	w.Header().Del("Content-Type")
	http.Redirect(w, r, strings.TrimRight(h.config.PublicURL, "/")+u.String(), http.StatusSeeOther)
}

func (h *HTTPServer) finishGitHubConnection(w http.ResponseWriter, r *http.Request, ctx context.Context, login oauthLoginState, token *oauth2.Token) {
	fail := func(code string) { h.githubReturn(w, r, login.Next, code) }
	p, err := h.human(r)
	if err != nil || (login.Provider != "github" && login.Provider != "github-app") || login.LinkAccountID == nil || p.Actor.ID != *login.LinkAccountID || login.WorkspaceID == nil {
		fail("session_expired")
		return
	}
	if err = h.allowWorkspace(r, p, *login.WorkspaceID, false); err != nil {
		fail("access_restricted")
		return
	}
	scope, _ := token.Extra("scope").(string)
	hasRepo := false
	for _, value := range strings.Fields(strings.ReplaceAll(scope, ",", " ")) {
		if value == "repo" {
			hasRepo = true
		}
	}
	if (login.Provider == "github" && !hasRepo) || token.AccessToken == "" {
		fail("repository_access_required")
		return
	}
	ctx = context.WithValue(ctx, githubTokenKey{}, token.AccessToken)
	raw, err := githubBytes(ctx, "https://api.github.com/user", 128<<10)
	if err != nil {
		fail("connection_failed")
		return
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if json.Unmarshal(raw, &user) != nil || user.ID == 0 || user.Login == "" {
		fail("connection_failed")
		return
	}
	if err = h.saveGitHubProviderConnection(ctx, p.Actor.ID, *login.WorkspaceID, strconv.FormatInt(user.ID, 10), user.Login, scope, login.Provider, token); err != nil {
		fail("connection_failed")
		return
	}
	h.githubReturn(w, r, login.Next, "connected")
}

func listGitHubRepositories(ctx context.Context, page int) ([]githubRepository, bool, error) {
	raw, err := githubBytes(ctx, fmt.Sprintf("https://api.github.com/user/repos?sort=updated&direction=desc&per_page=50&page=%d", page), 2<<20)
	if err != nil {
		return nil, false, err
	}
	repos := []githubRepository{}
	if err = json.Unmarshal(raw, &repos); err != nil {
		return nil, false, fmt.Errorf("GitHub returned an invalid repository list")
	}
	return repos, len(repos) == 50, nil
}

func (h *HTTPServer) workspaceGitHub(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodDelete {
		tx, err := h.service.Store.pool.Begin(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer tx.Rollback(r.Context())
		_, err = tx.Exec(r.Context(), `DELETE FROM github_repository_connections WHERE account_id=$1 AND workspace_id=$2`, p.Account.ID, workspace)
		if err == nil {
			_, err = tx.Exec(r.Context(), `DELETE FROM oauth_login_states WHERE link_account_id=$1 AND workspace_id=$2 AND mode='repository'`, p.Account.ID, workspace)
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, h.githubConnectionDefaults())
		return
	}
	if r.Method != http.MethodGet || len(parts) > 1 || (len(parts) == 1 && parts[0] != "repositories" && parts[0] != "compare" && parts[0] != "file" && parts[0] != "inspect") {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	state, token, err := h.githubAccess(r.Context(), p.Account.ID, workspace)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 0 {
		writeJSON(w, http.StatusOK, state)
		return
	}
	if parts[0] == "compare" || parts[0] == "file" || parts[0] == "inspect" {
		repo, err := normalizeRepository(r.URL.Query().Get("repository"))
		commit := r.URL.Query().Get("commit")
		if err != nil || !repositoryCommit.MatchString(commit) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("a repository and immutable commit are required"))
			return
		}
		if state.Reconnect {
			writeError(w, http.StatusUnauthorized, errGitHubReconnect)
			return
		}
		ctx := context.WithValue(r.Context(), githubTokenKey{}, token)
		var value any
		if parts[0] == "inspect" {
			value, err = inspectRepository(ctx, repo, commit)
		} else if parts[0] == "compare" {
			value, err = compareRepository(ctx, repo, r.URL.Query().Get("base"), commit)
		} else {
			value, err = readRepositoryFile(ctx, repo, commit, r.URL.Query().Get("path"))
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
		return
	}
	page := 1
	if value := r.URL.Query().Get("page"); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 || page > 200 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid repository page"))
			return
		}
	}
	repos := []githubRepository{}
	more := false
	if state.Connected {
		repos, more, err = listGitHubRepositories(context.WithValue(r.Context(), githubTokenKey{}, token), page)
		if errors.Is(err, errGitHubReconnect) {
			state.Connected, state.Reconnect = false, true
			repos = []githubRepository{}
		} else if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": state, "repositories": repos, "hasMore": more, "page": page})
}
