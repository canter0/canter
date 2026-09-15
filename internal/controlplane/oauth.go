package controlplane

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type OAuthCredentials struct{ ClientID, ClientSecret string }
type oauthIdentity struct{ Provider, Subject, Email string }
type oauthProvider struct {
	config   oauth2.Config
	identity func(context.Context, *oauth2.Token, string) (oauthIdentity, error)
}

var oauthHTTPClient = &http.Client{Timeout: 15 * time.Second}

func newOAuthProviders(config HTTPConfig) map[string]*oauthProvider {
	providers := map[string]*oauthProvider{}
	callback := func(provider string) string {
		return strings.TrimRight(config.PublicURL, "/") + "/api/canter/auth/oauth/" + provider + "/callback"
	}
	if c := config.GoogleOAuth; c.ClientID != "" && c.ClientSecret != "" {
		keys := oidc.NewRemoteKeySet(context.Background(), "https://www.googleapis.com/oauth2/v3/certs")
		verifier := oidc.NewVerifier("https://accounts.google.com", keys, &oidc.Config{ClientID: c.ClientID})
		providers["google"] = &oauthProvider{
			config: oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, RedirectURL: callback("google"), Scopes: []string{"openid", "email", "profile"}, Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}},
			identity: func(ctx context.Context, token *oauth2.Token, nonce string) (oauthIdentity, error) {
				return googleIdentity(ctx, verifier, token, nonce)
			},
		}
	}
	if c := config.GitHubOAuth; c.ClientID != "" && c.ClientSecret != "" {
		providers["github"] = &oauthProvider{
			config: oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, RedirectURL: callback("github"), Scopes: []string{"read:user", "user:email"}, Endpoint: oauth2.Endpoint{AuthURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token", AuthStyle: oauth2.AuthStyleInParams}},
			identity: func(ctx context.Context, token *oauth2.Token, _ string) (oauthIdentity, error) {
				return githubIdentity(ctx, oauthHTTPClient, "https://api.github.com", token.AccessToken)
			},
		}
	}
	return providers
}

func googleIdentity(ctx context.Context, verifier *oidc.IDTokenVerifier, token *oauth2.Token, nonce string) (oauthIdentity, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok || nonce == "" {
		return oauthIdentity{}, ErrUnauthorized
	}
	id, err := verifier.Verify(ctx, raw)
	if err != nil || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return oauthIdentity{}, ErrUnauthorized
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err = id.Claims(&claims); err != nil || !claims.Verified || id.Subject == "" {
		return oauthIdentity{}, ErrUnauthorized
	}
	return oauthIdentity{Provider: "google", Subject: id.Subject, Email: claims.Email}, nil
}

func githubIdentity(ctx context.Context, client *http.Client, origin, token string) (oauthIdentity, error) {
	get := func(path string, out any) error {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
		if err != nil {
			return err
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Accept", "application/vnd.github+json")
		r.Header.Set("User-Agent", "Canter")
		response, err := client.Do(r)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return ErrUnauthorized
		}
		return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out)
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if err := get("/user", &user); err != nil || user.ID <= 0 {
		return oauthIdentity{}, ErrUnauthorized
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := get("/user/emails", &emails); err != nil {
		return oauthIdentity{}, ErrUnauthorized
	}
	for _, email := range emails {
		if email.Primary && email.Verified {
			return oauthIdentity{Provider: "github", Subject: strconv.FormatInt(user.ID, 10), Email: email.Email}, nil
		}
	}
	return oauthIdentity{}, ErrUnauthorized
}

func safeOAuthNext(next, mode string) string {
	u, err := url.Parse(next)
	if err == nil && strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !u.IsAbs() && u.Host == "" && !strings.ContainsAny(next, "\\\r\n") && !strings.HasPrefix(u.Path, "//") && !strings.ContainsAny(u.Path, "\\\r\n") {
		return u.RequestURI()
	}
	if mode == "create-account" {
		return "/onboarding/agent"
	}
	if mode == "link" {
		return "/app/account"
	}
	return "/app"
}

func (h *HTTPServer) oauthProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	connected := map[string]bool{}
	if principal, err := h.human(r); err == nil {
		rows, err := h.service.Store.pool.Query(r.Context(), `SELECT provider FROM oauth_identities WHERE account_id=$1`, principal.Actor.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				writeStoreError(w, err)
				return
			}
			connected[name] = true
		}
		if err = rows.Err(); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	providers := []map[string]any{}
	for _, name := range []string{"github", "google"} {
		providers = append(providers, map[string]any{"id": name, "enabled": h.oauth[name] != nil, "connected": connected[name]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers, "requireInvite": h.config.RequireInvite})
}

func (h *HTTPServer) oauthCookieName() string {
	if h.config.CookieSecure {
		return "__Host-canter_oauth"
	}
	return "canter_oauth"
}
func (h *HTTPServer) setOAuthCookie(w http.ResponseWriter, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: h.oauthCookieName(), Value: value, Path: "/", HttpOnly: true, Secure: h.config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: age})
}

func (h *HTTPServer) oauthFailure(w http.ResponseWriter, r *http.Request, code, mode, next string) {
	if mode == "repository" {
		h.githubReturn(w, r, next, code)
		return
	}
	path := "/sign-in"
	if mode == "create-account" {
		path = "/create-account"
	}
	if mode == "link" {
		path = "/app/account"
	}
	query := url.Values{"error": {code}}
	if next != "" {
		query.Set("next", safeOAuthNext(next, mode))
	}
	w.Header().Del("Content-Type")
	http.Redirect(w, r, strings.TrimRight(h.config.PublicURL, "/")+path+"?"+query.Encode(), http.StatusSeeOther)
}

func (h *HTTPServer) oauthAuth(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if len(parts) < 1 || len(parts) > 2 || (len(parts) == 2 && parts[1] != "callback") {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	name := parts[0]
	provider := h.oauth[name]
	if provider == nil {
		h.oauthFailure(w, r, "provider_unavailable", r.URL.Query().Get("mode"), r.URL.Query().Get("next"))
		return
	}
	if len(parts) == 2 {
		h.oauthCallback(w, r, name, provider)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode != "create-account" && mode != "link" && mode != "repository" {
		mode = "sign-in"
	}
	if mode == "repository" && name != "github" {
		writeStoreError(w, ErrForbidden)
		return
	}
	// Always start and finish on the configured public origin, even if localhost
	// or the API port was used to open the login page.
	public, _ := url.Parse(h.config.PublicURL)
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	if public != nil && !strings.EqualFold(host, public.Host) {
		w.Header().Del("Content-Type")
		http.Redirect(w, r, strings.TrimRight(h.config.PublicURL, "/")+"/api/canter/auth/oauth/"+name+"?"+r.URL.RawQuery, http.StatusSeeOther)
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
	nonce, err := newSecret("", 32)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	login := oauthLoginState{Provider: name, Verifier: oauth2.GenerateVerifier(), Nonce: nonce, Next: safeOAuthNext(r.URL.Query().Get("next"), mode), Mode: mode, InviteHash: secretHash(r.URL.Query().Get("invite"))}
	if mode == "link" || mode == "repository" {
		principal, err := h.human(r)
		if err != nil {
			h.oauthFailure(w, r, "session_expired", "sign-in", "/app/account")
			return
		}
		login.LinkAccountID = &principal.Actor.ID
		if mode == "repository" {
			workspace := r.URL.Query().Get("workspace")
			if err := h.allowWorkspace(r, principal, workspace, false); err != nil {
				writeStoreError(w, err)
				return
			}
			login.WorkspaceID = &workspace
		}
	}
	if err = h.service.Store.beginOAuth(r.Context(), state, browser, login); err != nil {
		writeStoreError(w, err)
		return
	}
	h.setOAuthCookie(w, browser, 600)
	options := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(login.Verifier), oauth2.SetAuthURLParam("nonce", nonce)}
	if mode == "repository" {
		options = append(options, oauth2.SetAuthURLParam("scope", "repo"))
	}
	if name == "google" {
		options = append(options, oauth2.SetAuthURLParam("prompt", "select_account"))
	}
	w.Header().Del("Content-Type")
	http.Redirect(w, r, provider.config.AuthCodeURL(state, options...), http.StatusSeeOther)
}

func (h *HTTPServer) oauthCallback(w http.ResponseWriter, r *http.Request, name string, provider *oauthProvider) {
	cookie, err := r.Cookie(h.oauthCookieName())
	state := r.URL.Query().Get("state")
	if err != nil || cookie.Value == "" || state == "" {
		h.oauthFailure(w, r, "session_expired", "sign-in", "")
		return
	}
	login, err := h.service.Store.consumeOAuth(r.Context(), state, cookie.Value, name)
	if err != nil {
		h.oauthFailure(w, r, "session_expired", "sign-in", "")
		return
	}
	h.setOAuthCookie(w, "", -1)
	fail := func(code string) { h.oauthFailure(w, r, code, login.Mode, login.Next) }
	if r.URL.Query().Get("error") != "" {
		fail("access_denied")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		fail("sign_in_failed")
		return
	}
	if login.LinkAccountID != nil {
		principal, err := h.human(r)
		if err != nil || principal.Actor.ID != *login.LinkAccountID {
			fail("session_expired")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient)
	token, err := provider.config.Exchange(ctx, code, oauth2.VerifierOption(login.Verifier))
	if err != nil {
		fail("sign_in_failed")
		return
	}
	if login.Mode == "repository" {
		h.finishGitHubConnection(w, r, ctx, login, token)
		return
	}
	identity, err := provider.identity(ctx, token, login.Nonce)
	if err != nil || identity.Provider != name {
		fail("unverified_email")
		return
	}
	session, err := h.service.Store.signinOAuth(ctx, identity, login, h.config.RequireInvite)
	if errors.Is(err, errOAuthAccountExists) {
		fail("account_exists")
		return
	}
	if errors.Is(err, ErrForbidden) {
		fail("access_restricted")
		return
	}
	if err != nil {
		fail("sign_in_failed")
		return
	}
	h.setHumanCookie(w, session, 7*24*time.Hour)
	w.Header().Del("Content-Type")
	http.Redirect(w, r, strings.TrimRight(h.config.PublicURL, "/")+safeOAuthNext(login.Next, login.Mode), http.StatusSeeOther)
}

func ValidateOAuthCredentials(name string, c OAuthCredentials) error {
	if (c.ClientID == "") != (c.ClientSecret == "") {
		return fmt.Errorf("%s OAuth requires both client ID and client secret", name)
	}
	return nil
}
