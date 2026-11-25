package controlplane

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/canter0/canter/pricing"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

var (
	errCINotConfigured = errors.New("The CI GitHub App is not configured.")
	errCINoGitHub      = errors.New("Connect GitHub before installing the CI GitHub App.")
)

type ciGitHubClient struct {
	appID int64
	key   *rsa.PrivateKey
	base  string
	hc    *http.Client
}

type ciGitHubError struct {
	status int
}

func (e *ciGitHubError) Error() string {
	return fmt.Sprintf("GitHub API request failed (%d)", e.status)
}

func parseCIRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid private key")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = parsed
	} else if iface, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		key, ok = iface.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("unsupported private key type")
		}
	} else {
		return nil, errors.New("invalid private key")
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("CI GitHub App key must be RSA 2048 bits or larger")
	}
	return key, nil
}

func ValidateCIConfig(cfg CIConfig) error {
	if cfg.AppID == 0 && cfg.AppSlug == "" && len(cfg.PrivateKeyPEM) == 0 && len(cfg.WebhookSecret) == 0 {
		return nil
	}
	if cfg.AppID <= 0 {
		return errors.New("CANTER_CI_GITHUB_APP_ID")
	}
	if !githubAppSlug.MatchString(cfg.AppSlug) {
		return errors.New("CANTER_CI_GITHUB_APP_SLUG")
	}
	if _, err := parseCIRSAKey(cfg.PrivateKeyPEM); err != nil {
		return errors.New("CANTER_CI_GITHUB_APP_PRIVATE_KEY_FILE")
	}
	if len(cfg.WebhookSecret) < 32 {
		return errors.New("CANTER_CI_GITHUB_WEBHOOK_SECRET")
	}
	return nil
}

func (h *HTTPServer) ciHTTPClient() *http.Client {
	cfg := h.config.CI
	var hc *http.Client
	if cfg.HTTPClient != nil {
		clone := *cfg.HTTPClient
		hc = &clone
	} else {
		hc = &http.Client{}
	}
	if hc.Timeout <= 0 || hc.Timeout > 20*time.Second {
		hc.Timeout = 20 * time.Second
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return hc
}

func (h *HTTPServer) ciGitHubBase() string {
	base := strings.TrimRight(h.config.CI.APIBaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	return base
}

func (h *HTTPServer) ciGitHub() (*ciGitHubClient, error) {
	cfg := h.config.CI
	if err := ValidateCIConfig(cfg); err != nil {
		return nil, errCINotConfigured
	}
	key, err := parseCIRSAKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, errCINotConfigured
	}
	return &ciGitHubClient{appID: cfg.AppID, key: key, base: h.ciGitHubBase(), hc: h.ciHTTPClient()}, nil
}

func (h *HTTPServer) ciGitHubTransport() *ciGitHubClient {
	return &ciGitHubClient{base: h.ciGitHubBase(), hc: h.ciHTTPClient()}
}

func (h *HTTPServer) ciConfigured() bool {
	return h.config.CI.AppID > 0 && ValidateCIConfig(h.config.CI) == nil
}

func (c *ciGitHubClient) appJWT() (string, error) {
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`, now.Add(-60*time.Second).Unix(), now.Add(540*time.Second).Unix(), c.appID)))
	signing := header + "." + claims
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (c *ciGitHubClient) call(ctx context.Context, method, path, bearer string, body any, out any) error {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "://") {
		return errors.New("invalid GitHub API path")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	token := bearer
	if token == "app" {
		token, err = c.appJWT()
		if err != nil {
			return err
		}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 8<<20 {
		return errors.New("GitHub API response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &ciGitHubError{status: resp.StatusCode}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type ciAppInstallation struct {
	ID      int64 `json:"id"`
	AppID   int64 `json:"app_id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

type ciGitHubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type ciOrgMembership struct {
	Role  string `json:"role"`
	State string `json:"state"`
}

type ciInstallationToken struct {
	Token string `json:"token"`
}

type ciGitHubRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

type ciRepoList struct {
	TotalCount   int            `json:"total_count"`
	Repositories []ciGitHubRepo `json:"repositories"`
}

type ciRepoInfo struct {
	ID      int64 `json:"id"`
	Private *bool `json:"private"`
}

func (c *ciGitHubClient) installationToken(ctx context.Context, installationID int64) (string, error) {
	var out ciInstallationToken
	if err := c.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), "app", nil, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("GitHub returned no installation token")
	}
	return out.Token, nil
}

func (c *ciGitHubClient) appInstallation(ctx context.Context, installationID int64) (ciAppInstallation, error) {
	var out ciAppInstallation
	if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", installationID), "app", nil, &out); err != nil {
		return out, err
	}
	if out.AppID != c.appID {
		return out, errors.New("installation belongs to a different GitHub App")
	}
	if out.ID != installationID || out.Account.ID <= 0 || out.Account.Login == "" {
		return out, errors.New("invalid installation account")
	}
	return out, nil
}

func (c *ciGitHubClient) personalInstallation(ctx context.Context, user ciGitHubUser) (ciAppInstallation, error) {
	var out ciAppInstallation
	path := "/users/" + url.PathEscape(user.Login) + "/installation"
	if err := c.call(ctx, http.MethodGet, path, "app", nil, &out); err != nil {
		return out, err
	}
	if out.ID <= 0 || out.AppID != c.appID || out.Account.Type != "User" || out.Account.ID != user.ID || out.Account.Login == "" {
		return out, errors.New("invalid personal GitHub App installation")
	}
	return out, nil
}

func (c *ciGitHubClient) userIdentity(ctx context.Context, userToken string) (ciGitHubUser, error) {
	var out ciGitHubUser
	if err := c.call(ctx, http.MethodGet, "/user", userToken, nil, &out); err != nil {
		return out, err
	}
	if out.ID <= 0 || out.Login == "" {
		return out, errors.New("invalid GitHub user identity")
	}
	return out, nil
}

func (c *ciGitHubClient) orgMembership(ctx context.Context, installationToken, org, login string) (ciOrgMembership, error) {
	var out ciOrgMembership
	err := c.call(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/memberships/%s", url.PathEscape(org), url.PathEscape(login)), installationToken, nil, &out)
	return out, err
}

func (c *ciGitHubClient) repoInfo(ctx context.Context, fullName, bearer string) (ciRepoInfo, error) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 {
		return ciRepoInfo{}, errors.New("invalid repository name")
	}
	var out ciRepoInfo
	err := c.call(ctx, http.MethodGet, "/repos/"+url.PathEscape(parts[0])+"/"+url.PathEscape(parts[1]), bearer, nil, &out)
	return out, err
}

func (c *ciGitHubClient) installationRepositories(ctx context.Context, installationToken string) ([]ciGitHubRepo, error) {
	var all []ciGitHubRepo
	total := -1
	for page := 1; page <= 10; page++ {
		var out ciRepoList
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), installationToken, nil, &out); err != nil {
			return nil, err
		}
		if total < 0 {
			total = out.TotalCount
			if total > 1000 {
				return nil, errors.New("GitHub App installation selects more than 1000 repositories")
			}
		}
		all = append(all, out.Repositories...)
		if len(out.Repositories) < 100 || len(all) >= total {
			break
		}
	}
	if len(all) > 1000 {
		return nil, errors.New("GitHub App installation selects more than 1000 repositories")
	}
	return all, nil
}

func (h *HTTPServer) ciConnect(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string, linkPersonal bool) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	role, err := workspaceRole(r.Context(), h.service.Store.pool, workspaceID, p.Account.ID, false)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if role != "owner" {
		writeStoreError(w, ErrForbidden)
		return
	}
	client, err := h.ciGitHub()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCINotConfigured)
		return
	}
	_, userToken, err := h.githubAccess(r.Context(), p.Account.ID, workspaceID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if userToken == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{
			"code": "ci_github_repository_connection_required", "message": errCINoGitHub.Error(),
		}})
		return
	}
	if linkPersonal {
		user, err := client.userIdentity(r.Context(), userToken)
		if err != nil {
			writeError(w, http.StatusBadGateway, errors.New("GitHub user identity could not be verified."))
			return
		}
		installation, err := client.personalInstallation(r.Context(), user)
		if err != nil {
			var apiError *ciGitHubError
			if errors.As(err, &apiError) && apiError.status == http.StatusNotFound {
				writeError(w, http.StatusNotFound, errors.New("Canter CI is not installed on your personal GitHub account."))
				return
			}
			writeError(w, http.StatusBadGateway, errors.New("The personal GitHub App installation could not be verified."))
			return
		}
		if h.ciBindGitHubInstallation(w, r, client, workspaceID, p.Account.ID, "", installation, user) {
			writeJSON(w, http.StatusOK, map[string]bool{"linked": true})
		}
		return
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		writeStoreError(w, err)
		return
	}
	stateValue := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(stateValue))
	stateHash := hex.EncodeToString(sum[:])
	if err = h.service.Store.CreateCIConnectState(r.Context(), workspaceID, p.Account.ID, stateHash, time.Now().Add(10*time.Minute)); err != nil {
		writeStoreError(w, err)
		return
	}
	installURL := "https://github.com/apps/" + h.config.CI.AppSlug + "/installations/new?state=" + stateValue
	writeJSON(w, http.StatusOK, map[string]any{"url": installURL})
}

func (h *HTTPServer) ciGitHubCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	installationParam := r.URL.Query().Get("installation_id")
	if state == "" || len(state) > 200 || installationParam == "" {
		writeError(w, http.StatusBadRequest, errors.New("invalid GitHub App callback"))
		return
	}
	installationID, err := strconv.ParseInt(installationParam, 10, 64)
	if err != nil || installationID <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("invalid GitHub App callback"))
		return
	}
	sum := sha256.Sum256([]byte(state))
	stateHash := hex.EncodeToString(sum[:])
	workspaceID, accountID, _, err := h.service.Store.GetCIConnectState(r.Context(), stateHash)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("GitHub App connect state is invalid or expired"))
		return
	}
	role, err := workspaceRole(r.Context(), h.service.Store.pool, workspaceID, accountID, false)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if role != "owner" {
		writeError(w, http.StatusForbidden, errors.New("Only the workspace owner can connect the CI GitHub App."))
		return
	}
	client, err := h.ciGitHub()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCINotConfigured)
		return
	}
	_, userToken, err := h.githubAccess(r.Context(), accountID, workspaceID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if userToken == "" {
		writeError(w, http.StatusConflict, errCINoGitHub)
		return
	}
	installation, err := client.appInstallation(r.Context(), installationID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("GitHub App installation could not be verified."))
		return
	}
	user, err := client.userIdentity(r.Context(), userToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, errors.New("GitHub user identity could not be verified."))
		return
	}
	if h.ciBindGitHubInstallation(w, r, client, workspaceID, accountID, stateHash, installation, user) {
		http.Redirect(w, r, strings.TrimRight(h.config.PublicURL, "/")+"/app/ci", http.StatusSeeOther)
	}
}

func (h *HTTPServer) ciBindGitHubInstallation(w http.ResponseWriter, r *http.Request, client *ciGitHubClient, workspaceID, accountID, stateHash string, installation ciAppInstallation, user ciGitHubUser) bool {
	if installation.SuspendedAt != nil {
		writeError(w, http.StatusConflict, errors.New("GitHub App installation is suspended."))
		return false
	}
	installationID := installation.ID
	instToken, err := client.installationToken(r.Context(), installationID)
	if err != nil {
		writeError(w, http.StatusBadGateway, errors.New("GitHub App installation token could not be created."))
		return false
	}
	switch installation.Account.Type {
	case "User":
		if installation.Account.ID != user.ID {
			writeError(w, http.StatusForbidden, errors.New("The GitHub App installation does not belong to your GitHub account."))
			return false
		}
	case "Organization":
		membership, err := client.orgMembership(r.Context(), instToken, installation.Account.Login, user.Login)
		if err != nil {
			writeError(w, http.StatusForbidden, errors.New("GitHub organization membership could not be verified."))
			return false
		}
		if membership.Role != "admin" || membership.State != "active" {
			writeError(w, http.StatusForbidden, errors.New("Organization GitHub App installations require an active organization admin."))
			return false
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("Unsupported GitHub installation account type."))
		return false
	}
	repos, err := client.installationRepositories(r.Context(), instToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, errors.New("GitHub App installation repositories could not be listed."))
		return false
	}
	if len(repos) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("Select at least one repository for the GitHub App installation."))
		return false
	}
	_, err = h.service.Store.bindCIInstallation(r.Context(), workspaceID, accountID, stateHash, installationID, installation.Account.ID, installation.Account.Login, installation.Account.Type, repos)
	if err != nil {
		switch {
		case errors.Is(err, errCIConnectState):
			writeError(w, http.StatusBadRequest, errors.New("GitHub App connect state is invalid or expired"))
		case errors.Is(err, errCINotWorkspaceOwner):
			writeError(w, http.StatusForbidden, errors.New("Only the workspace owner can connect the CI GitHub App."))
		case errors.Is(err, errCIAccountDisabled):
			writeError(w, http.StatusForbidden, errors.New("Account is disabled."))
		case errors.Is(err, errCIInstallationBound):
			writeError(w, http.StatusConflict, errors.New("This GitHub App installation is already connected to a different workspace or account."))
		default:
			writeStoreError(w, err)
		}
		return false
	}
	_ = h.service.Store.Audit(r.Context(), workspaceID, sdkActorForAccount(accountID), "ci.github.connected", installation.Account.Login, map[string]any{"installationId": installationID, "repositories": len(repos)})
	return true
}

var errCIInstallationBound = errors.New("GitHub App installation is already bound")

func sdkActorForAccount(accountID string) sdk.ActorRef {
	return sdk.ActorRef{Kind: "human", ID: accountID}
}

type ciWebhookEnvelope struct {
	Action       string `json:"action"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository *struct {
		ID int64 `json:"id"`
	} `json:"repository"`
	WorkflowJob *ciWorkflowJobPayload `json:"workflow_job"`
}

type ciWorkflowJobPayload struct {
	ID           int64            `json:"id"`
	RunID        int64            `json:"run_id"`
	Name         string           `json:"name"`
	WorkflowName string           `json:"workflow_name"`
	HeadSHA      string           `json:"head_sha"`
	HeadBranch   string           `json:"head_branch"`
	Status       string           `json:"status"`
	Conclusion   *string          `json:"conclusion"`
	StartedAt    *string          `json:"started_at"`
	CompletedAt  *string          `json:"completed_at"`
	Labels       []string         `json:"labels"`
	Steps        []ciWorkflowStep `json:"steps"`
}

type ciWorkflowStep struct {
	Name        string  `json:"name"`
	Number      int     `json:"number"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
}

func (h *HTTPServer) ciGitHubPublic(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) >= 1 && parts[0] == "cache" {
		if h.config.CI.Cache == nil {
			writeError(w, http.StatusNotFound, ErrNotFound)
			return
		}
		h.config.CI.Cache.ServeHTTP(w, r)
		return
	}
	if len(parts) == 2 && parts[0] == "github" && parts[1] == "callback" && r.Method == http.MethodGet {
		h.ciGitHubCallback(w, r)
		return
	}
	if len(parts) == 2 && parts[0] == "github" && parts[1] == "webhook" && r.Method == http.MethodPost {
		h.ciGitHubWebhook(w, r)
		return
	}
	if len(parts) == 1 && (parts[0] == "runner-admissions" || parts[0] == "runner-bootstrap" || parts[0] == "runner-evidence") &&
		h.config.CI.ManagedService != nil {
		s := h.config.CI.ManagedService
		if s.mode == "" || s.mode == CIManagedOff || s.mode == CIManagedRecover ||
			(parts[0] != "runner-evidence" && r.Method != http.MethodPost) {
			writeError(w, http.StatusNotFound, ErrNotFound)
			return
		}
		ctx, release, ok := s.acquire(r.Context())
		if !ok {
			ciAdmissionUnavailable(w)
			return
		}
		defer release()
		stopBody := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
		defer stopBody()
		r = r.WithContext(ctx)
		switch parts[0] {
		case "runner-admissions":
			h.ciRunnerAdmissionHTTP(w, r)
		case "runner-bootstrap":
			h.ciRunnerBootstrapHTTP(w, r)
		case "runner-evidence":
			h.ciRunnerEvidenceHTTP(w, r)
		}
		return
	}
	if len(parts) == 1 && parts[0] == "runner-admissions" && r.Method == http.MethodPost && h.ciAdmissionConfigured() {
		h.ciRunnerAdmissionHTTP(w, r)
		return
	}
	if len(parts) == 1 && parts[0] == "runner-bootstrap" && r.Method == http.MethodPost && h.ciBootstrapConfigured() {
		h.ciRunnerBootstrapHTTP(w, r)
		return
	}
	if len(parts) == 1 && parts[0] == "runner-evidence" && h.ciEvidenceRouteConfigured() {
		h.ciRunnerEvidenceHTTP(w, r)
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) ciGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := h.config.CI.WebhookSecret
	if len(secret) < 32 {
		writeError(w, http.StatusServiceUnavailable, errCINotConfigured)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("could not read webhook body"))
		return
	}
	if len(body) > 1<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, errors.New("webhook body too large"))
		return
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(signature, "sha256=") || len(signature) != 71 {
		writeError(w, http.StatusUnauthorized, errors.New("missing webhook signature"))
		return
	}
	provided, err := hex.DecodeString(signature[7:])
	if err != nil || len(provided) != sha256.Size {
		writeError(w, http.StatusUnauthorized, errors.New("invalid webhook signature"))
		return
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if subtle.ConstantTimeCompare(provided, mac.Sum(nil)) != 1 {
		writeError(w, http.StatusUnauthorized, errors.New("webhook signature mismatch"))
		return
	}
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" || len(deliveryID) > 200 {
		writeError(w, http.StatusBadRequest, errors.New("invalid webhook delivery id"))
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "" || len(event) > 80 {
		writeError(w, http.StatusBadRequest, errors.New("invalid webhook event"))
		return
	}
	if !json.Valid(body) {
		writeError(w, http.StatusBadRequest, errCIWebhookInvalid)
		return
	}
	bodyHash := sha256.Sum256(body)
	bodySHA := hex.EncodeToString(bodyHash[:])
	tx, err := h.service.Store.pool.Begin(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var inserted string
	err = tx.QueryRow(r.Context(), `INSERT INTO ci_webhook_deliveries(delivery_id,body_sha256,event) VALUES($1,$2,$3) ON CONFLICT(delivery_id) DO NOTHING RETURNING delivery_id`, deliveryID, bodySHA, event).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		var existingBody, existingEvent string
		if qerr := tx.QueryRow(r.Context(), `SELECT body_sha256,event FROM ci_webhook_deliveries WHERE delivery_id=$1`, deliveryID).Scan(&existingBody, &existingEvent); qerr != nil {
			writeStoreError(w, qerr)
			return
		}
		if existingBody == bodySHA && existingEvent == event {
			_ = tx.Commit(r.Context())
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true})
			return
		}
		writeError(w, http.StatusConflict, errors.New("webhook delivery id was already used with a different payload"))
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err = h.ciApplyWebhookEvent(r.Context(), tx, event, body); err != nil {
		if errors.Is(err, errCIWebhookInvalid) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

var errCIWebhookInvalid = errors.New("invalid webhook payload")

func (h *HTTPServer) ciApplyWebhookEvent(ctx context.Context, tx pgx.Tx, event string, body []byte) error {
	switch event {
	case "installation":
		return h.ciWebhookInstallation(ctx, tx, body)
	case "workflow_job":
		return h.ciWebhookWorkflowJob(ctx, tx, body)
	default:
		return nil
	}
}

func (h *HTTPServer) ciWebhookInstallation(ctx context.Context, tx pgx.Tx, body []byte) error {
	var env struct {
		Action       string `json:"action"`
		Installation *struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return errCIWebhookInvalid
	}
	if env.Installation == nil || env.Installation.ID <= 0 {
		return errCIWebhookInvalid
	}
	status := ""
	switch env.Action {
	case "suspend":
		status = "suspended"
	case "deleted":
		status = "removed"
	case "unsuspend", "created":
		status = "active"
	default:
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE ci_github_installations SET status=$2,updated_at=now() WHERE github_installation_id=$1 AND status<>'removed'`, env.Installation.ID, status)
	return err
}

func (h *HTTPServer) ciWebhookWorkflowJob(ctx context.Context, tx pgx.Tx, body []byte) error {
	var env ciWebhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return errCIWebhookInvalid
	}
	if env.Installation == nil || env.Installation.ID <= 0 || env.Repository == nil || env.Repository.ID <= 0 || env.WorkflowJob == nil {
		return errCIWebhookInvalid
	}
	job := env.WorkflowJob
	if job.ID <= 0 || job.RunID <= 0 {
		return errCIWebhookInvalid
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "canter-ci/job/"+strconv.FormatInt(env.Installation.ID, 10)+"/"+strconv.FormatInt(env.Repository.ID, 10)+"/"+strconv.FormatInt(job.ID, 10)); err != nil {
		return err
	}
	var installationID, workspaceID string
	err := tx.QueryRow(ctx, `SELECT id,workspace_id FROM ci_github_installations WHERE github_installation_id=$1 AND status='active'`, env.Installation.ID).Scan(&installationID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var fullName string
	var repoEnabled bool
	err = tx.QueryRow(ctx, `SELECT full_name,enabled FROM ci_repositories WHERE installation_id=$1 AND github_repository_id=$2`, installationID, env.Repository.ID).Scan(&fullName, &repoEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !repoEnabled {
		return nil
	}
	sizeID, blockedReason, hasCanter := ciResolveJobLabels(job.Labels)
	if !hasCanter {
		return nil
	}
	status := ciMapJobStatus(job.Status)
	if status == "" {
		return nil
	}
	if blockedReason != "" && status == "queued" {
		status = "blocked"
	}
	ciJob, steps, err := h.ciBuildJobDocument(fullName, job, sizeID, status, blockedReason)
	if err != nil {
		return err
	}
	return h.ciUpsertJob(ctx, tx, workspaceID, installationID, env.Repository.ID, job, ciJob, steps, status)
}

func ciMapJobStatus(status string) string {
	switch status {
	case "queued":
		return "queued"
	case "in_progress":
		return "running"
	case "completed":
		return "completed"
	}
	return ""
}

func ciResolveJobLabels(labels []string) (sizeID, blockedReason string, hasCanter bool) {
	sizes := map[string]bool{}
	unsupported := false
	catalog := pricing.CurrentCI()
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		matched := false
		for _, size := range catalog.Sizes {
			if label == size.Label {
				sizes[size.ID] = true
				hasCanter = true
				matched = true
			}
		}
		if matched {
			continue
		}
		if label == "canter" {
			sizes["linux-x64-4"] = true
			hasCanter = true
			continue
		}
		if strings.HasPrefix(label, "canter") {
			unsupported = true
			hasCanter = true
		}
	}
	if !hasCanter {
		return "", "", false
	}
	if unsupported {
		return "", "Unsupported Canter runner label.", true
	}
	if len(sizes) > 1 {
		return "", "Conflicting Canter runner labels.", true
	}
	for id := range sizes {
		return id, "", true
	}
	return "", "", true
}

func ciBoundField(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) > max {
		value = value[:max]
		for len(value) > 0 && !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func ciParseTime(value *string) *time.Time {
	if value == nil || *value == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, *value); err == nil {
		return &t
	}
	return nil
}

func (h *HTTPServer) ciBuildJobDocument(fullName string, job *ciWorkflowJobPayload, sizeID, status, blockedReason string) (CIJob, []CIJobStep, error) {
	steps := []CIJobStep{}
	for _, step := range job.Steps {
		if len(steps) >= 100 {
			break
		}
		entry := CIJobStep{Name: ciBoundField(step.Name, 512), Number: step.Number, Status: ciBoundField(step.Status, 64)}
		if step.Conclusion != nil {
			c := ciBoundField(*step.Conclusion, 64)
			entry.Conclusion = &c
		}
		entry.StartedAt = ciParseTime(step.StartedAt)
		entry.CompletedAt = ciParseTime(step.CompletedAt)
		steps = append(steps, entry)
	}
	started := ciParseTime(job.StartedAt)
	completed := ciParseTime(job.CompletedAt)
	var elapsed int64
	if started != nil {
		end := time.Now().UTC()
		if completed != nil {
			end = *completed
		}
		if d := end.Sub(*started).Seconds(); d > 0 {
			elapsed = int64(d)
		}
	}
	githubURL := fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", fullName, job.RunID, job.ID)
	queueReason := ""
	switch {
	case blockedReason != "":
		queueReason = blockedReason
	case status == "queued":
		queueReason = "The CI runner backend has not been activated."
	case status == "running":
		queueReason = "Reported by GitHub but not running on a Canter-managed runner; it is not billed."
	}
	ciJob := CIJob{
		ID:              "",
		Repository:      fullName,
		Name:            ciBoundField(job.Name, 512),
		WorkflowName:    ciBoundField(job.WorkflowName, 512),
		Ref:             ciBoundField(job.HeadBranch, 255),
		HeadSHA:         ciBoundField(job.HeadSHA, 64),
		GitHubJobID:     job.ID,
		GitHubRunID:     job.RunID,
		GitHubURL:       githubURL,
		Status:          status,
		Size:            sizeID,
		CreatedAt:       time.Now().UTC(),
		StartedAt:       started,
		CompletedAt:     completed,
		ElapsedSeconds:  elapsed,
		BillableSeconds: 0,
		CostMicros:      0,
		QueueReason:     queueReason,
	}
	if job.Conclusion != nil {
		ciJob.Conclusion = ciBoundField(*job.Conclusion, 64)
	}
	return ciJob, steps, nil
}

func (h *HTTPServer) ciUpsertJob(ctx context.Context, tx pgx.Tx, workspaceID, installationID string, repoID int64, payload *ciWorkflowJobPayload, ciJob CIJob, steps []CIJobStep, status string) error {
	var existingID, existingStatus string
	var raw []byte
	var existingCreatedAt time.Time
	var existingRunnerID *string
	err := tx.QueryRow(ctx, `SELECT id,status,document,created_at,runner_id FROM ci_jobs WHERE installation_id=$1 AND github_job_id=$2 FOR UPDATE`, installationID, payload.ID).Scan(&existingID, &existingStatus, &raw, &existingCreatedAt, &existingRunnerID)
	if err == nil {
		terminal := existingStatus == "completed" || existingStatus == "cancelled"
		incomingActive := status == "queued" || status == "running" || status == "blocked"
		if terminal && incomingActive {
			return nil
		}
		var doc ciJobDocument
		if err = json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		ciJob.ID = existingID
		ciJob.CreatedAt = doc.Job.CreatedAt
		if ciJob.CreatedAt.IsZero() {
			ciJob.CreatedAt = existingCreatedAt
		}
		ciJob.RunnerID = existingRunnerID
		if existingRunnerID != nil {
			// A runner-bound job keeps the admission-recorded source clock
			// and size, and must not inherit the unmanaged-runner warning.
			if doc.Job.StartedAt != nil {
				ciJob.StartedAt = doc.Job.StartedAt
			}
			if doc.Job.Size != "" {
				ciJob.Size = doc.Job.Size
			}
			if status == "running" {
				ciJob.QueueReason = ""
			}
		}
		ciJob.BillableSeconds = doc.Job.BillableSeconds
		ciJob.CostMicros = doc.Job.CostMicros
		if ciJob.HeadSHA == "" {
			ciJob.HeadSHA = doc.Job.HeadSHA
		}
		if ciJob.Name == "" {
			ciJob.Name = doc.Job.Name
		}
		if ciJob.WorkflowName == "" {
			ciJob.WorkflowName = doc.Job.WorkflowName
		}
		if ciJob.Ref == "" {
			ciJob.Ref = doc.Job.Ref
		}
		if ciJob.Size == "" {
			ciJob.Size = doc.Job.Size
		}
		if len(steps) == 0 {
			steps = doc.Steps
		}
		if status == "queued" && existingStatus != "queued" {
			return nil
		}
		if status == "blocked" && existingStatus != "queued" && existingStatus != "blocked" {
			return nil
		}
		doc.Job = ciJob
		doc.Steps = steps
		updated, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE ci_jobs SET status=$3,document=$4,updated_at=now() WHERE workspace_id=$1 AND id=$2`, workspaceID, existingID, status, updated)
		return err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	enabled, err := h.service.Store.productEnabled(ctx, tx, workspaceID, "ci")
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	id, err := newID("cijob_")
	if err != nil {
		return err
	}
	ciJob.ID = id
	doc := ciJobDocument{Job: ciJob, Steps: steps}
	raw, err = json.Marshal(doc)
	if err != nil {
		return err
	}
	var runnerID any
	_, err = tx.Exec(ctx, `INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,runner_id,document,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now()) ON CONFLICT(installation_id,github_job_id) DO NOTHING`, id, workspaceID, installationID, repoID, payload.ID, payload.RunID, status, runnerID, raw, ciJob.CreatedAt)
	return err
}
