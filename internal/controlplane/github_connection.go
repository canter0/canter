package controlplane

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
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
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Reconnect bool   `json:"reconnect,omitempty"`
	Login     string `json:"login,omitempty"`
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
func (h *HTTPServer) githubCipher() (cipher.AEAD, error) {
	if h.config.GitHubOAuth.ClientSecret == "" {
		return nil, errGitHubReconnect
	}
	key := sha256.Sum256([]byte("canter:github-repository-token:v1\x00" + h.config.GitHubOAuth.ClientSecret))
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
	aead, err := h.githubCipher()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	sealed := aead.Seal(nonce, nonce, []byte(token.AccessToken), githubTokenBinding(account, workspace))
	var expires *time.Time
	if !token.Expiry.IsZero() {
		expires = &token.Expiry
	}
	_, err = h.service.Store.pool.Exec(ctx, `INSERT INTO github_repository_connections(account_id,workspace_id,github_id,login,encrypted_token,scopes,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(account_id,workspace_id) DO UPDATE SET github_id=EXCLUDED.github_id,login=EXCLUDED.login,encrypted_token=EXCLUDED.encrypted_token,scopes=EXCLUDED.scopes,expires_at=EXCLUDED.expires_at,updated_at=now()`, account, workspace, id, login, sealed, scope, expires)
	return err
}

func (h *HTTPServer) githubAccess(ctx context.Context, account, workspace string) (githubConnection, string, error) {
	state := githubConnection{Enabled: h.oauth["github"] != nil}
	var sealed []byte
	var expires *time.Time
	err := h.service.Store.pool.QueryRow(ctx, `SELECT login,encrypted_token,expires_at FROM github_repository_connections WHERE account_id=$1 AND workspace_id=$2`, account, workspace).Scan(&state.Login, &sealed, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, "", nil
	}
	if err != nil {
		return state, "", err
	}
	aead, err := h.githubCipher()
	if err != nil || !state.Enabled || (expires != nil && !expires.After(time.Now())) || len(sealed) < aead.NonceSize() {
		state.Reconnect = true
		return state, "", nil
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], githubTokenBinding(account, workspace))
	if err != nil || len(plain) == 0 {
		state.Reconnect = true
		return state, "", nil
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
	if err != nil || login.Provider != "github" || login.LinkAccountID == nil || p.Actor.ID != *login.LinkAccountID || login.WorkspaceID == nil {
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
	if !hasRepo || token.AccessToken == "" {
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
	if err = h.saveGitHubConnection(ctx, p.Actor.ID, *login.WorkspaceID, strconv.FormatInt(user.ID, 10), user.Login, scope, token); err != nil {
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
		writeJSON(w, http.StatusOK, githubConnection{Enabled: h.oauth["github"] != nil})
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
