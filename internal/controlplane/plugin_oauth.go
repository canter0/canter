package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type pluginOAuthSecret struct {
	Metadata     appOAuthMetadata `json:"metadata"`
	ClientID     string           `json:"clientId"`
	Verifier     string           `json:"verifier,omitempty"`
	RefreshToken string           `json:"refreshToken,omitempty"`
	ExpiresAt    time.Time        `json:"expiresAt"`
	Scopes       []string         `json:"scopes"`
}

var resourceMetadataChallenge = regexp.MustCompile(`(?i)\bresource_metadata="([^"]+)"`)

func (h *HTTPServer) pluginOAuthMetadata(ctx context.Context, endpoint string) (appOAuthMetadata, error) {
	var metadata appOAuthMetadata
	u, _, err := extensionURL(endpoint, h.config.Extensions)
	if err != nil {
		return metadata, err
	}
	origin := pluginOrigin(endpoint)
	locations := []string{origin + "/.well-known/oauth-protected-resource" + u.EscapedPath(), origin + "/.well-known/oauth-protected-resource"}
	client, closeClient, err := extensionHTTPClient(ctx, h.config.Extensions, endpoint)
	if err != nil {
		return metadata, err
	}
	defer closeClient()
	// Read the provider's challenge without ever replaying another credential.
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if res, e := client.Do(req); e == nil {
		if match := resourceMetadataChallenge.FindStringSubmatch(res.Header.Get("WWW-Authenticate")); len(match) > 1 {
			locations = append([]string{match[1]}, locations...)
		}
		res.Body.Close()
	}
	var resource struct {
		Resource string   `json:"resource"`
		Issuers  []string `json:"authorization_servers"`
	}
	found := false
	for _, location := range locations {
		candidate, _, e := extensionURL(location, h.config.Extensions)
		if e != nil || pluginOrigin(candidate.String()) != origin {
			continue
		}
		if e = h.appOAuthRequest(ctx, location, "GET", "", nil, &resource); e == nil && strings.TrimRight(resource.Resource, "/") == strings.TrimRight(endpoint, "/") && len(resource.Issuers) > 0 {
			found = true
			break
		}
	}
	if !found {
		return metadata, fmt.Errorf("this provider did not advertise OAuth sign-in; use its API token or check its setup instructions")
	}
	issuer, _, err := extensionURL(resource.Issuers[0], h.config.Extensions)
	if err != nil {
		return metadata, fmt.Errorf("invalid OAuth issuer")
	}
	issuerURL := strings.TrimRight(issuer.String(), "/")
	issuerOrigin := pluginOrigin(issuerURL)
	metadataURLs := []string{issuerOrigin + "/.well-known/oauth-authorization-server" + strings.TrimRight(issuer.EscapedPath(), "/"), issuerURL + "/.well-known/openid-configuration"}
	found = false
	for _, location := range metadataURLs {
		if err = h.appOAuthRequest(ctx, location, "GET", "", nil, &metadata); err == nil && strings.TrimRight(metadata.Issuer, "/") == issuerURL {
			found = true
			break
		}
	}
	if !found || !includesString(metadata.Challenges, "S256") {
		return metadata, fmt.Errorf("this provider does not advertise PKCE sign-in")
	}
	for _, destination := range []string{metadata.Authorization, metadata.Token, metadata.Registration} {
		if destination == "" {
			continue
		}
		if _, _, err = extensionURL(destination, h.config.Extensions); err != nil {
			return metadata, fmt.Errorf("invalid OAuth endpoint")
		}
	}
	if metadata.Authorization == "" || metadata.Token == "" {
		return metadata, fmt.Errorf("the provider's OAuth endpoints are missing")
	}
	return metadata, nil
}
func (h *HTTPServer) pluginOAuthCookieName() string {
	if h.config.CookieSecure {
		return "__Host-canter_plugin_oauth"
	}
	return "canter_plugin_oauth"
}
func (h *HTTPServer) pluginOAuthCookie(w http.ResponseWriter, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: h.pluginOAuthCookieName(), Value: value, Path: "/", HttpOnly: true, Secure: h.config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func (h *HTTPServer) pluginOAuthRedirect() string {
	return strings.TrimRight(h.config.PublicURL, "/") + "/api/canter/auth/plugin-oauth/callback"
}
func (h *HTTPServer) authorizePlugin(w http.ResponseWriter, r *http.Request, workspace, account string, v pluginConnection, clientID string) {
	if h.config.Secrets == nil {
		writeError(w, 503, fmt.Errorf("plugin credential storage is unavailable"))
		return
	}
	if v.State != "needs_auth" && v.State != "review" && v.State != "failed" {
		writeStoreError(w, ErrConflict)
		return
	}
	secret, err := h.readPluginSecret(r.Context(), h.service.Store.pool, workspace, account, v.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	metadata, err := h.pluginOAuthMetadata(ctx, secret.Endpoint)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	scopes := []string{}
	for _, server := range v.Catalog.Servers {
		resolved, resolveErr := resolvePluginServer(v.Catalog, server.Name, nil, "", h.config.Extensions)
		if resolveErr == nil && resolved.Endpoint == secret.Endpoint {
			if clientID == "" {
				clientID = server.OAuthClientID
			}
			scopes = server.OAuthScopes
			break
		}
	}
	if len(clientID) > 1000 || strings.ContainsAny(clientID, "\r\n") {
		writeError(w, 400, fmt.Errorf("invalid OAuth client ID"))
		return
	}
	if clientID == "" && metadata.Registration != "" {
		var registered struct {
			ID     string `json:"client_id"`
			Method string `json:"token_endpoint_auth_method"`
		}
		raw, _ := json.Marshal(map[string]any{"client_name": "Canter", "client_uri": "https://canter.dev", "redirect_uris": []string{h.pluginOAuthRedirect()}, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "token_endpoint_auth_method": "none"})
		if err = h.appOAuthRequest(ctx, metadata.Registration, "POST", "application/json", raw, &registered); err == nil && registered.Method != "" && registered.Method != "none" {
			err = fmt.Errorf("provider requires a confidential client")
		}
		if err == nil {
			clientID = registered.ID
		}
	}
	if clientID == "" {
		writeJSON(w, 200, map[string]any{"clientRegistrationRequired": true, "redirectUrl": h.pluginOAuthRedirect(), "message": "Register Canter with this provider and enter its OAuth client ID."})
		return
	}
	state := oauth2.GenerateVerifier()
	browser := oauth2.GenerateVerifier()
	verifier := oauth2.GenerateVerifier()
	secret.OAuth = &pluginOAuthSecret{Metadata: metadata, ClientID: clientID, Verifier: verifier, Scopes: scopes}
	// Pending secrets are bound to the connection and revision. A disconnect
	// invalidates the callback, and the one-time browser state prevents CSRF.
	raw, _ := json.Marshal(secret)
	defer clear(raw)
	key, sealed, err := h.config.Secrets.seal(raw, []byte("canter:plugin-oauth:"+state))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if _, err = h.service.Store.pool.Exec(ctx, `DELETE FROM plugin_oauth_states WHERE expires_at<=now()`); err != nil {
		writeStoreError(w, err)
		return
	}
	_, err = h.service.Store.pool.Exec(ctx, `INSERT INTO plugin_oauth_states(state_hash,browser_hash,connection_id,revision,key_id,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5,$6,now()+interval '10 minutes')`, secretHash(state), secretHash(browser), v.ID, v.Revision, key, sealed)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	h.pluginOAuthCookie(w, browser, 600)
	config := oauth2.Config{ClientID: clientID, RedirectURL: h.pluginOAuthRedirect(), Scopes: scopes, Endpoint: oauth2.Endpoint{AuthURL: metadata.Authorization}}
	authURL := config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", secret.Endpoint))
	writeJSON(w, 200, map[string]string{"authorizationUrl": authURL})
}
func (h *HTTPServer) exchangePluginToken(ctx context.Context, secret *pluginSecret, values url.Values) error {
	auth := secret.OAuth
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := h.appOAuthRequest(ctx, auth.Metadata.Token, "POST", "application/x-www-form-urlencoded", []byte(values.Encode()), &token); err != nil {
		return err
	}
	if token.AccessToken == "" || len(token.AccessToken) > 4096 || !strings.EqualFold(token.TokenType, "bearer") || strings.ContainsAny(token.AccessToken, "\r\n") {
		return fmt.Errorf("invalid OAuth token response")
	}
	if len(auth.Scopes) > 0 {
		for _, scope := range strings.Fields(token.Scope) {
			if !includesString(auth.Scopes, scope) {
				return fmt.Errorf("the provider returned unrequested permissions")
			}
		}
	}
	if token.ExpiresIn <= 0 {
		token.ExpiresIn = 3600
	}
	if token.ExpiresIn > 86400*365 {
		return fmt.Errorf("invalid OAuth token lifetime")
	}
	if secret.Headers == nil {
		secret.Headers = map[string]string{}
	}
	secret.Headers["Authorization"] = "Bearer " + token.AccessToken
	if token.RefreshToken != "" {
		auth.RefreshToken = token.RefreshToken
	}
	auth.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	auth.Verifier = ""
	return nil
}
func (h *HTTPServer) refreshPluginOAuth(ctx context.Context, secret *pluginSecret) error {
	auth := secret.OAuth
	if auth.RefreshToken == "" {
		return errAppReconnect
	}
	values := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {auth.RefreshToken}, "client_id": {auth.ClientID}, "resource": {secret.Endpoint}}
	if err := h.exchangePluginToken(ctx, secret, values); err != nil {
		if appAuthorizationExpired(err) {
			return errAppReconnect
		}
		return fmt.Errorf("the provider could not refresh access right now; try again")
	}
	return nil
}
func (h *HTTPServer) pluginOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	slug := ""
	finish := func(status string) {
		u := strings.TrimRight(h.config.PublicURL, "/") + "/app/customize?plugin=" + url.QueryEscape(slug) + "&connection=" + status
		http.Redirect(w, r, u, http.StatusSeeOther)
	}
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie(h.pluginOAuthCookieName())
	if err != nil || len(state) < 32 || len(state) > 256 || h.config.Secrets == nil {
		finish("expired")
		return
	}
	p, err := h.human(r)
	if err != nil || !h.recentAuth(r.Context(), p) {
		finish("expired")
		return
	}
	var id, workspace, account, key string
	var revision int
	var sealed []byte
	err = h.service.Store.pool.QueryRow(r.Context(), `DELETE FROM plugin_oauth_states s USING plugin_connections p WHERE s.connection_id=p.id AND s.state_hash=$1 AND s.browser_hash=$2 AND p.account_id=$3 AND s.expires_at>now() AND p.revision=s.revision AND p.enabled RETURNING p.id,p.workspace_id,p.account_id,p.slug,s.revision,s.key_id,s.ciphertext`, secretHash(state), secretHash(cookie.Value), p.Actor.ID).Scan(&id, &workspace, &account, &slug, &revision, &key, &sealed)
	if err != nil {
		finish("expired")
		return
	}
	h.pluginOAuthCookie(w, "", -1)
	if err = h.service.Store.RequireWorkspaceWriter(r.Context(), workspace, account); err != nil {
		finish("expired")
		return
	}
	if r.URL.Query().Get("error") != "" {
		finish("cancelled")
		return
	}
	raw, err := h.config.Secrets.open(key, sealed, []byte("canter:plugin-oauth:"+state))
	if err != nil {
		finish("failed")
		return
	}
	defer clear(raw)
	var secret pluginSecret
	if json.Unmarshal(raw, &secret) != nil || secret.OAuth == nil {
		finish("failed")
		return
	}
	auth := secret.OAuth
	issuer := r.URL.Query().Get("iss")
	if (auth.Metadata.ResponseIssuer || issuer != "") && issuer != auth.Metadata.Issuer {
		finish("failed")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 4096 {
		finish("failed")
		return
	}
	values := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {auth.Verifier}, "client_id": {auth.ClientID}, "redirect_uri": {h.pluginOAuthRedirect()}, "resource": {secret.Endpoint}}
	if err = h.exchangePluginToken(r.Context(), &secret, values); err != nil {
		finish("failed")
		return
	}
	v, err := h.getPluginConnection(r.Context(), workspace, account, id)
	if err != nil || v.Revision != revision {
		finish("changed")
		return
	}
	v, err = h.discoverPlugin(r.Context(), workspace, account, v, secret)
	if err != nil {
		finish("failed")
		return
	}
	if v.State == "review" {
		finish("connected")
	} else {
		finish("failed")
	}
}
