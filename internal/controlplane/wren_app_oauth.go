package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

type wrenMCPProvider struct{ Name, Endpoint, ResourceMetadata, Issuer string }

func (h *HTTPServer) appProvider(name string) (wrenMCPProvider, bool) {
	if h.config.Extensions.appProviders != nil {
		v, ok := h.config.Extensions.appProviders[name]
		return v, ok
	}
	if name == "linear" {
		return wrenMCPProvider{Name: "Linear", Endpoint: "https://mcp.linear.app/mcp/readonly", ResourceMetadata: "https://mcp.linear.app/.well-known/oauth-protected-resource/mcp/readonly", Issuer: "https://mcp.linear.app"}, true
	}
	return wrenMCPProvider{}, false
}

type appOAuthMetadata struct {
	Issuer         string   `json:"issuer"`
	Authorization  string   `json:"authorization_endpoint"`
	Token          string   `json:"token_endpoint"`
	Registration   string   `json:"registration_endpoint"`
	Challenges     []string `json:"code_challenge_methods_supported"`
	Scopes         []string `json:"scopes_supported"`
	ResponseIssuer bool     `json:"authorization_response_iss_parameter_supported"`
}
type appOAuthSecret struct {
	ClientID          string           `json:"clientId"`
	ReconnectID       string           `json:"reconnectId,omitempty"`
	ReconnectRevision int              `json:"reconnectRevision,omitempty"`
	AccessToken       string           `json:"accessToken,omitempty"`
	RefreshToken      string           `json:"refreshToken,omitempty"`
	Verifier          string           `json:"verifier,omitempty"`
	Next              string           `json:"next,omitempty"`
	Redirect          string           `json:"redirect"`
	Metadata          appOAuthMetadata `json:"metadata"`
}

func appAccountBinding(workspace, account, id string) []byte {
	return []byte("canter:app-account:v1\x00" + workspace + "\x00" + account + "\x00" + id)
}
func appOAuthBinding(workspace, account, state string) []byte {
	return []byte("canter:app-oauth:v1\x00" + workspace + "\x00" + account + "\x00" + state)
}
func includesString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

type appOAuthResponseError struct {
	status int
	code   string
}

func (e *appOAuthResponseError) Error() string {
	return "the app’s authorization service rejected the request"
}
func appAuthorizationExpired(err error) bool {
	var response *appOAuthResponseError
	return errors.As(err, &response) && (response.status == 400 || response.status == 401) && (response.code == "invalid_grant" || response.code == "invalid_client" || response.code == "unauthorized_client")
}

// OAuth discovery and token exchange use the same DNS pinning and redirect
// rejection as tools. Bodies/errors are bounded and never returned verbatim.
func (h *HTTPServer) appOAuthRequest(ctx context.Context, endpoint, method, media string, body []byte, out any) error {
	client, closeClient, err := extensionHTTPClient(ctx, h.config.Extensions, endpoint)
	if err != nil {
		return err
	}
	defer closeClient()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.GetBody = nil
	request.Header.Set("Accept", "application/json")
	if media != "" {
		request.Header.Set("Content-Type", media)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("the app’s authorization service is unavailable")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return fmt.Errorf("the app returned an invalid authorization response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Code string `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		// Retain only protocol codes needed for recovery, never provider descriptions.
		code := ""
		switch failure.Code {
		case "invalid_grant", "invalid_client", "unauthorized_client":
			code = failure.Code
		}
		return &appOAuthResponseError{status: response.StatusCode, code: code}
	}
	if json.Unmarshal(raw, out) != nil {
		return fmt.Errorf("the app returned an invalid authorization response")
	}
	return nil
}
func (h *HTTPServer) appMetadata(ctx context.Context, provider wrenMCPProvider) (appOAuthMetadata, error) {
	var resource struct {
		Resource string   `json:"resource"`
		Issuers  []string `json:"authorization_servers"`
		Scopes   []string `json:"scopes_supported"`
	}
	var metadata appOAuthMetadata
	if err := h.appOAuthRequest(ctx, provider.ResourceMetadata, "GET", "", nil, &resource); err != nil {
		return metadata, err
	}
	if resource.Resource != provider.Endpoint || !includesString(resource.Issuers, provider.Issuer) || !includesString(resource.Scopes, "read") {
		return metadata, fmt.Errorf("the app no longer advertises the expected read-only authorization")
	}
	if err := h.appOAuthRequest(ctx, provider.Issuer+"/.well-known/oauth-authorization-server", "GET", "", nil, &metadata); err != nil {
		return metadata, err
	}
	if metadata.Issuer != provider.Issuer || !includesString(metadata.Challenges, "S256") || !includesString(metadata.Scopes, "read") {
		return metadata, fmt.Errorf("the app does not support the required authorization protections")
	}
	for _, endpoint := range []string{metadata.Authorization, metadata.Token, metadata.Registration} {
		u, _, err := extensionURL(endpoint, h.config.Extensions)
		if err != nil || u.Scheme+"://"+u.Host != provider.Issuer {
			return metadata, fmt.Errorf("the app’s authorization destination changed")
		}
	}
	return metadata, nil
}
func (h *HTTPServer) appOAuthCookie(w http.ResponseWriter, value string, age int) {
	name := "canter_app_oauth"
	if h.config.CookieSecure {
		name = "__Host-" + name
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: h.config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func appNextPath(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || strings.ContainsAny(value, "\\\r\n") {
		return "/app/customize?app=linear"
	}
	if u.Path == "/app/customize" {
		q := url.Values{"app": []string{"linear"}}
		if conversation := u.Query().Get("conversation"); strings.HasPrefix(conversation, "conv_") {
			q.Set("conversation", conversation)
		}
		u.RawQuery = q.Encode()
		u.Fragment = ""
		return u.String()
	}
	if strings.HasPrefix(u.Path, "/app/wrens") {
		q := url.Values{"apps": []string{"1"}}
		u.RawQuery = q.Encode()
		u.Fragment = ""
		return u.String()
	}
	return "/app/customize?app=linear"
}
func (h *HTTPServer) startAppOAuth(w http.ResponseWriter, r *http.Request, workspace, account string) {
	if h.config.Secrets == nil {
		writeError(w, 503, fmt.Errorf("account connections are not configured on this server"))
		return
	}
	var input struct {
		Provider         string `json:"provider"`
		Next             string `json:"next"`
		ReconnectID      string `json:"reconnectId"`
		ExpectedRevision int    `json:"expectedRevision"`
	}
	if !decodeLimit(w, r, &input, 4096) {
		return
	}
	provider, ok := h.appProvider(input.Provider)
	if !ok {
		writeStoreError(w, ErrNotFound)
		return
	}
	p, err := h.human(r)
	if err != nil || p.Actor.ID != account || !h.recentAuth(r.Context(), p) {
		writeError(w, 401, fmt.Errorf("sign in again before connecting an account"))
		return
	}
	if err = h.service.Store.RequireWorkspaceWriter(r.Context(), workspace, account); err != nil {
		writeStoreError(w, err)
		return
	}
	if input.ReconnectID != "" {
		var revision int
		err = h.service.Store.pool.QueryRow(r.Context(), `SELECT revision FROM wren_app_accounts WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND provider=$4 AND endpoint=$5 AND revoked_at IS NULL`, input.ReconnectID, workspace, account, input.Provider, provider.Endpoint).Scan(&revision)
		if errors.Is(err, pgx.ErrNoRows) {
			writeStoreError(w, ErrNotFound)
			return
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if revision != input.ExpectedRevision {
			writeStoreError(w, ErrConflict)
			return
		}
	} else if input.ExpectedRevision != 0 {
		writeStoreError(w, ErrConflict)
		return
	}
	// The browser cookie and callback must use the same configured origin.
	public, err := url.Parse(h.config.PublicURL)
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	if err != nil || public.Host == "" || !strings.EqualFold(public.Host, host) {
		writeError(w, 400, fmt.Errorf("open this conversation at the server’s configured address before connecting an account"))
		return
	}
	var pending int
	err = h.service.Store.pool.QueryRow(r.Context(), `SELECT count(*) FROM wren_app_oauth_states WHERE account_id=$1 AND expires_at>now()`, account).Scan(&pending)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if pending >= 5 {
		writeStoreError(w, ErrCapacity)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	metadata, err := h.appMetadata(ctx, provider)
	if err != nil {
		writeError(w, 502, err)
		return
	}
	redirect := strings.TrimRight(h.config.PublicURL, "/") + "/api/canter/auth/app-oauth/callback"
	registration, _ := json.Marshal(map[string]any{"client_name": "Canter", "redirect_uris": []string{redirect}, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "token_endpoint_auth_method": "none", "scope": "read"})
	var client struct {
		ID        string   `json:"client_id"`
		Auth      string   `json:"token_endpoint_auth_method"`
		Redirects []string `json:"redirect_uris"`
	}
	if err = h.appOAuthRequest(ctx, metadata.Registration, "POST", "application/json", registration, &client); err != nil {
		writeError(w, 502, err)
		return
	}
	if client.ID == "" || len(client.ID) > 2048 || client.Auth != "none" || !includesString(client.Redirects, redirect) {
		writeError(w, 502, fmt.Errorf("the app did not register the expected authorization client"))
		return
	}
	state, err := newSecret("", 32)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	browser, err := newSecret("", 32)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	secret := appOAuthSecret{ReconnectID: input.ReconnectID, ReconnectRevision: input.ExpectedRevision, ClientID: client.ID, Verifier: oauth2.GenerateVerifier(), Next: appNextPath(input.Next), Redirect: redirect, Metadata: metadata}
	raw, _ := json.Marshal(secret)
	key, sealed, err := h.config.Secrets.seal(raw, appOAuthBinding(workspace, account, state))
	clear(raw)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_, err = h.service.Store.pool.Exec(ctx, `INSERT INTO wren_app_oauth_states(state_hash,browser_hash,workspace_id,account_id,provider,key_id,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,now()+interval '10 minutes')`, secretHash(state), secretHash(browser), workspace, account, input.Provider, key, sealed)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	h.appOAuthCookie(w, browser, 600)
	config := oauth2.Config{ClientID: client.ID, RedirectURL: redirect, Scopes: []string{"read"}, Endpoint: oauth2.Endpoint{AuthURL: metadata.Authorization}}
	authURL := config.AuthCodeURL(state, oauth2.S256ChallengeOption(secret.Verifier), oauth2.SetAuthURLParam("resource", provider.Endpoint))
	writeJSON(w, 200, map[string]string{"authorizationUrl": authURL})
}
func (h *HTTPServer) appOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	next := "/app/customize?app=linear"
	finish := func(status string) {
		u, _ := url.Parse(next)
		q := u.Query()
		q.Set("appConnection", status)
		u.RawQuery = q.Encode()
		w.Header().Del("Content-Type")
		http.Redirect(w, r, strings.TrimRight(h.config.PublicURL, "/")+u.String(), http.StatusSeeOther)
	}
	if h.config.Secrets == nil {
		finish("failed")
		return
	}
	state := r.URL.Query().Get("state")
	cookieName := "canter_app_oauth"
	if h.config.CookieSecure {
		cookieName = "__Host-" + cookieName
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil || len(state) < 32 || len(state) > 256 || cookie.Value == "" {
		finish("expired")
		return
	}
	p, err := h.human(r)
	if err != nil || !h.recentAuth(r.Context(), p) {
		finish("expired")
		return
	}
	var workspace, account, providerID, key string
	var sealed []byte
	err = h.service.Store.pool.QueryRow(r.Context(), `DELETE FROM wren_app_oauth_states WHERE state_hash=$1 AND browser_hash=$2 AND account_id=$3 AND expires_at>now() RETURNING workspace_id,account_id,provider,key_id,ciphertext`, secretHash(state), secretHash(cookie.Value), p.Actor.ID).Scan(&workspace, &account, &providerID, &key, &sealed)
	if err != nil {
		finish("expired")
		return
	}
	h.appOAuthCookie(w, "", -1)
	raw, err := h.config.Secrets.open(key, sealed, appOAuthBinding(workspace, account, state))
	if err != nil {
		finish("failed")
		return
	}
	defer clear(raw)
	var secret appOAuthSecret
	if json.Unmarshal(raw, &secret) != nil {
		finish("failed")
		return
	}
	next = appNextPath(secret.Next)
	provider, ok := h.appProvider(providerID)
	if !ok {
		finish("failed")
		return
	}
	if err = h.service.Store.RequireWorkspaceWriter(r.Context(), workspace, account); err != nil {
		finish("expired")
		return
	}
	if r.URL.Query().Get("error") != "" {
		finish("cancelled")
		return
	}
	issuer := r.URL.Query().Get("iss")
	if (secret.Metadata.ResponseIssuer && issuer != provider.Issuer) || (issuer != "" && issuer != provider.Issuer) {
		finish("failed")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 4096 {
		finish("failed")
		return
	}
	values := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {secret.Verifier}, "client_id": {secret.ClientID}, "redirect_uri": {secret.Redirect}, "resource": {provider.Endpoint}}
	token, expires, err := h.exchangeAppToken(r.Context(), secret.Metadata.Token, values)
	if err != nil {
		finish("failed")
		return
	}
	secret.AccessToken, secret.RefreshToken, secret.Verifier, secret.Next = token.AccessToken, token.RefreshToken, "", ""
	id := secret.ReconnectID
	reconnectRevision := secret.ReconnectRevision
	secret.ReconnectID, secret.ReconnectRevision = "", 0
	if id == "" {
		id, err = newID("appaccount_")
	}
	if err != nil {
		finish("failed")
		return
	}
	rawToken, _ := json.Marshal(secret)
	key, sealed, err = h.config.Secrets.seal(rawToken, appAccountBinding(workspace, account, id))
	clear(rawToken)
	if err != nil {
		finish("failed")
		return
	}
	tx, err := h.service.Store.pool.Begin(r.Context())
	if err != nil {
		finish("failed")
		return
	}
	defer tx.Rollback(r.Context())
	if err = requireWorkspaceWriter(r.Context(), tx, workspace, account, true); err != nil {
		finish("expired")
		return
	}
	if reconnectRevision > 0 {
		// Reauthorization restores this exact connection only. A concurrent rename,
		// reconnect, or disconnect invalidates the captured revision.
		result, updateErr := tx.Exec(r.Context(), `UPDATE wren_app_accounts SET key_id=$6,ciphertext=$7,expires_at=$8,reconnect=false,revision=revision+1,updated_at=now() WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND provider=$4 AND endpoint=$5 AND revision=$9 AND revoked_at IS NULL`, id, workspace, account, providerID, provider.Endpoint, key, sealed, expires, reconnectRevision)
		if updateErr != nil {
			finish("failed")
			return
		}
		if result.RowsAffected() != 1 {
			finish("changed")
			return
		}
	} else {
		var count int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM wren_app_accounts WHERE account_id=$1 AND workspace_id=$2 AND revoked_at IS NULL`, account, workspace).Scan(&count); err != nil || count >= 20 {
			finish("failed")
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO wren_app_accounts(id,workspace_id,account_id,provider,endpoint,label,key_id,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, workspace, account, providerID, provider.Endpoint, provider.Name+" account", key, sealed, expires)
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		finish("failed")
		return
	}
	if reconnectRevision > 0 {
		finish("reconnected")
	} else {
		finish("connected")
	}
}

type appTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (h *HTTPServer) exchangeAppToken(ctx context.Context, endpoint string, values url.Values) (appTokenResponse, time.Time, error) {
	var token appTokenResponse
	if err := h.appOAuthRequest(ctx, endpoint, "POST", "application/x-www-form-urlencoded", []byte(values.Encode()), &token); err != nil {
		return token, time.Time{}, err
	}
	valid := func(s string) bool {
		return len(s) > 0 && len(s) <= 16384 && strings.IndexFunc(s, func(r rune) bool { return r < 33 || r > 126 }) < 0
	}
	if !valid(token.AccessToken) || !strings.EqualFold(token.TokenType, "bearer") || token.ExpiresIn <= 0 || token.ExpiresIn > 31536000 || (token.RefreshToken != "" && !valid(token.RefreshToken)) {
		return appTokenResponse{}, time.Time{}, fmt.Errorf("the app returned an unusable token")
	}
	if token.Scope != "" && strings.Join(strings.Fields(token.Scope), " ") != "read" {
		return appTokenResponse{}, time.Time{}, fmt.Errorf("the app returned unexpected account permissions")
	}
	return token, time.Now().Add(time.Duration(token.ExpiresIn) * time.Second), nil
}

var errAppReconnect = errors.New("Reconnect this app account before using it.")

// Caller holds the existing installation/credential locks first. Disconnect
// locks only the account row, so refresh cannot revive a disconnected account.
func (h *HTTPServer) appAccountTokenTx(ctx context.Context, tx pgx.Tx, workspace, account, id, endpoint string) (string, error) {
	if h.config.Secrets == nil {
		return "", fmt.Errorf("account connections are not configured on this server")
	}
	var providerID, key string
	var sealed []byte
	var expiry *time.Time
	var reconnect bool
	err := tx.QueryRow(ctx, `SELECT provider,key_id,ciphertext,expires_at,reconnect FROM wren_app_accounts WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND endpoint=$4 AND revoked_at IS NULL FOR UPDATE`, id, workspace, account, endpoint).Scan(&providerID, &key, &sealed, &expiry, &reconnect)
	if errors.Is(err, pgx.ErrNoRows) || reconnect {
		return "", errAppReconnect
	}
	if err != nil {
		return "", err
	}
	markReconnect := func() (string, error) {
		if _, err := tx.Exec(ctx, `UPDATE wren_app_accounts SET reconnect=true,revision=revision+1,updated_at=now() WHERE id=$1 AND NOT reconnect AND revoked_at IS NULL`, id); err != nil {
			return "", err
		}
		return "", errAppReconnect
	}
	provider, ok := h.appProvider(providerID)
	if !ok || provider.Endpoint != endpoint {
		return markReconnect()
	}
	raw, err := h.config.Secrets.open(key, sealed, appAccountBinding(workspace, account, id))
	if err != nil {
		return markReconnect()
	}
	defer clear(raw)
	var secret appOAuthSecret
	if json.Unmarshal(raw, &secret) != nil || secret.AccessToken == "" {
		return markReconnect()
	}
	if expiry == nil || !expiry.After(time.Now().Add(time.Minute)) {
		if secret.RefreshToken == "" {
			return markReconnect()
		}
		values := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {secret.RefreshToken}, "client_id": {secret.ClientID}, "resource": {endpoint}}
		token, expires, err := h.exchangeAppToken(ctx, secret.Metadata.Token, values)
		if err != nil {
			if appAuthorizationExpired(err) {
				return markReconnect()
			}
			return "", fmt.Errorf("the app could not refresh access right now; try again")
		}
		secret.AccessToken = token.AccessToken
		if token.RefreshToken != "" {
			secret.RefreshToken = token.RefreshToken
		}
		updated, _ := json.Marshal(secret)
		key, sealed, err = h.config.Secrets.seal(updated, appAccountBinding(workspace, account, id))
		clear(updated)
		if err != nil {
			return "", err
		}
		if _, err = tx.Exec(ctx, `UPDATE wren_app_accounts SET key_id=$2,ciphertext=$3,expires_at=$4,updated_at=now() WHERE id=$1`, id, key, sealed, expires); err != nil {
			return "", err
		}
	}
	return secret.AccessToken, nil
}
