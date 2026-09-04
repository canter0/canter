// Package remote is the transport client for Canter's durable control plane.
// It is independent of the CLI and never reads or writes credentials itself.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
)

var ErrAuthorizationPending = errors.New("agent authorization is still pending")

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string, httpClient *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("Canter API URL must be an absolute http or https URL without credentials, query, or fragment")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("Canter API URL must use https outside loopback development")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	} else {
		clone := *httpClient
		httpClient = &clone
	}
	// Never replay bearer or refresh credentials to a redirect target. Callers
	// must opt into the new canonical Canter URL explicitly.
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: baseURL, http: httpClient}, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) BaseURL() string { return c.baseURL }
func (c *Client) MCPURL() string  { return c.baseURL + "/mcp" }

type Authority struct {
	Inspect   bool   `json:"inspect"`
	Draft     bool   `json:"draft"`
	ApplyMode string `json:"applyMode"`
}

type BeginInput struct {
	Name      string    `json:"name"`
	Harness   string    `json:"harness"`
	Authority Authority `json:"authority"`
}

type DeviceAuthorization struct {
	DeviceCode      string    `json:"deviceCode"`
	UserCode        string    `json:"userCode"`
	VerificationURI string    `json:"verificationUri"`
	ExpiresAt       time.Time `json:"expiresAt"`
	IntervalSeconds int       `json:"intervalSeconds"`
}

type Installation struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspaceId"`
	Name        string     `json:"name"`
	Harness     string     `json:"harness"`
	Authority   Authority  `json:"authority"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastSeenAt  *time.Time `json:"lastSeenAt,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

type Session struct {
	ID             string    `json:"id"`
	InstallationID string    `json:"installationId"`
	ClientInstance string    `json:"clientInstance,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	LastSeenAt     time.Time `json:"lastSeenAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

type TokenPair struct {
	AccessToken  string       `json:"accessToken"`
	TokenType    string       `json:"tokenType"`
	ExpiresIn    int          `json:"expiresIn"`
	RefreshToken string       `json:"refreshToken"`
	Installation Installation `json:"installation"`
	Session      Session      `json:"session"`
}

type Identity struct {
	Installation Installation `json:"installation"`
	Session      Session      `json:"session"`
}

type Bootstrap struct {
	ProtocolVersion    string            `json:"protocolVersion"`
	Installation       Installation      `json:"installation"`
	Session            Session           `json:"session"`
	Workspace          json.RawMessage   `json:"workspace"`
	Systems            []json.RawMessage `json:"systems"`
	PendingChanges     []json.RawMessage `json:"pendingChanges"`
	InitialDeployments []json.RawMessage `json:"initialDeployments"`
	Capabilities       json.RawMessage   `json:"capabilities"`
	Incidents          []json.RawMessage `json:"incidents"`
}

type DeploymentArtifact struct {
	WorkspaceID string    `json:"workspaceId"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	Filename    string    `json:"filename,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

type InitialDeploymentRelease struct {
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	HealthPath  string            `json:"healthPath"`
	PublicPort  int               `json:"publicPort"`
}

type DraftInitialDeploymentInput struct {
	Summary        string                   `json:"summary"`
	System         sdk.System               `json:"system"`
	ArtifactSHA256 string                   `json:"artifactSha256"`
	Release        InitialDeploymentRelease `json:"release"`
	Verification   sdk.ChangeVerification   `json:"verification"`
}

type InitialDeployment struct {
	ID          string            `json:"id"`
	WorkspaceID string            `json:"workspaceId"`
	System      string            `json:"system"`
	Summary     string            `json:"summary"`
	Phase       string            `json:"phase"`
	Digest      string            `json:"digest"`
	Plan        json.RawMessage   `json:"plan"`
	Operations  []json.RawMessage `json:"operations"`
	Evidence    []json.RawMessage `json:"evidence"`
	Failure     string            `json:"failure,omitempty"`
}

type InitialDeploymentExecution struct {
	ID           string `json:"id"`
	WorkspaceID  string `json:"workspaceId"`
	DeploymentID string `json:"deploymentId"`
	System       string `json:"system"`
	Phase        string `json:"phase"`
	Attempts     int    `json:"attempts"`
	Failure      string `json:"failure,omitempty"`
}

func (c *Client) BeginDeviceAuthorization(ctx context.Context, input BeginInput) (DeviceAuthorization, error) {
	var result DeviceAuthorization
	err := c.do(ctx, http.MethodPost, "/v1/device/authorizations", "", input, &result)
	return result, err
}

func (c *Client) ExchangeDevice(ctx context.Context, deviceCode, clientInstance string) (TokenPair, error) {
	var result TokenPair
	err := c.do(ctx, http.MethodPost, "/v1/device/token", "", map[string]string{"deviceCode": deviceCode, "clientInstance": clientInstance}, &result)
	return result, err
}

func (c *Client) PollDevice(ctx context.Context, authorization DeviceAuthorization, clientInstance string) (TokenPair, error) {
	interval := time.Duration(authorization.IntervalSeconds) * time.Second
	if interval < 250*time.Millisecond {
		interval = 2 * time.Second
	}
	for {
		pair, err := c.ExchangeDevice(ctx, authorization.DeviceCode, clientInstance)
		if err == nil {
			return pair, nil
		}
		if !errors.Is(err, ErrAuthorizationPending) {
			return TokenPair{}, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return TokenPair{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) Refresh(ctx context.Context, refreshToken, clientInstance string) (TokenPair, error) {
	var result TokenPair
	err := c.do(ctx, http.MethodPost, "/v1/agent/token/refresh", "", map[string]string{"refreshToken": refreshToken, "clientInstance": clientInstance}, &result)
	return result, err
}

func (c *Client) WhoAmI(ctx context.Context, accessToken string) (Identity, error) {
	var result Identity
	err := c.do(ctx, http.MethodGet, "/v1/agent/whoami", accessToken, nil, &result)
	return result, err
}

func (c *Client) Bootstrap(ctx context.Context, accessToken string) (Bootstrap, error) {
	var result Bootstrap
	err := c.do(ctx, http.MethodGet, "/v1/agent/bootstrap", accessToken, nil, &result)
	return result, err
}

func (c *Client) UploadArtifact(ctx context.Context, accessToken, workspaceID, filename, contentType string, artifact io.Reader) (DeploymentArtifact, error) {
	var result DeploymentArtifact
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/workspaces/"+url.PathEscape(workspaceID)+"/artifacts", artifact)
	if err != nil {
		return result, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-Canter-Filename", filename)
	response, err := c.http.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return result, err
	}
	if err := decodeResponse(response.StatusCode, raw, &result); err != nil {
		return DeploymentArtifact{}, err
	}
	return result, nil
}

func (c *Client) DraftInitialDeployment(ctx context.Context, accessToken, workspaceID string, input DraftInitialDeploymentInput) (InitialDeployment, error) {
	var result InitialDeployment
	err := c.do(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/initial-deployments", accessToken, input, &result)
	return result, err
}

func (c *Client) ListInitialDeployments(ctx context.Context, accessToken, workspaceID string) ([]InitialDeployment, error) {
	var result struct {
		InitialDeployments []InitialDeployment `json:"initialDeployments"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/initial-deployments", accessToken, nil, &result)
	return result.InitialDeployments, err
}

func (c *Client) InspectInitialDeployment(ctx context.Context, accessToken, workspaceID, deploymentID string) (InitialDeployment, error) {
	var result InitialDeployment
	err := c.do(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/initial-deployments/"+url.PathEscape(deploymentID), accessToken, nil, &result)
	return result, err
}

func (c *Client) InspectInitialDeploymentExecution(ctx context.Context, accessToken, executionID string) (InitialDeploymentExecution, error) {
	var result InitialDeploymentExecution
	err := c.do(ctx, http.MethodGet, "/v1/initial-deployment-executions/"+url.PathEscape(executionID), accessToken, nil, &result)
	return result, err
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Canter API returned %d: %s", e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, path, accessToken string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	return decodeResponse(response.StatusCode, raw, output)
}

func decodeResponse(status int, raw []byte, output any) error {
	if status == http.StatusPreconditionRequired {
		return ErrAuthorizationPending
	}
	if status < 200 || status >= 300 {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		message := strings.TrimSpace(envelope.Error.Message)
		if message == "" {
			message = strings.TrimSpace(string(raw))
		}
		if message == "" {
			message = http.StatusText(status)
		}
		return &APIError{Status: status, Message: message}
	}
	if output == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return fmt.Errorf("decode Canter API response: %w", err)
	}
	return nil
}
