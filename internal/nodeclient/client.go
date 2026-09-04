package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
)

type Client struct {
	base      *url.URL
	tokenFile string
	http      *http.Client
}

func New(gatewayURL, tokenFile string) (*Client, error) {
	return newClient(gatewayURL, tokenFile, &http.Client{Timeout: 45 * time.Second})
}

func NewWithHTTPClient(gatewayURL, tokenFile string, httpClient *http.Client) (*Client, error) {
	return newClient(gatewayURL, tokenFile, httpClient)
}

func newClient(gatewayURL, tokenFile string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("node gateway URL must be an absolute HTTPS URL")
	}
	if tokenFile == "" {
		return nil, fmt.Errorf("node token file is required")
	}
	if httpClient == nil {
		return nil, fmt.Errorf("HTTP client is required")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return &Client{base: u, tokenFile: tokenFile, http: httpClient}, nil
}

func (c *Client) token() (string, error) {
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read node credential: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if !strings.HasPrefix(token, "cn_") {
		return "", fmt.Errorf("node credential has invalid format")
	}
	return token, nil
}

func (c *Client) endpoint(parts ...string) string {
	u := *c.base
	items := append([]string{u.Path, "v1", "node"}, parts...)
	u.Path = path.Join(items...)
	return u.String()
}

func (c *Client) request(ctx context.Context, method, endpoint string, body any, limit int64) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	token, err := c.token()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("node gateway response exceeds limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("node gateway returned HTTP %d", resp.StatusCode)
	}
	return b, nil
}

func (c *Client) Snapshot(ctx context.Context) (sdk.NodeSnapshot, error) {
	b, err := c.request(ctx, http.MethodGet, c.endpoint("snapshot"), nil, 4<<20)
	var out sdk.NodeSnapshot
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}

func (c *Client) Artifact(ctx context.Context, digest string) ([]byte, error) {
	return c.request(ctx, http.MethodGet, c.endpoint("artifacts", digest), nil, 512<<20)
}

func (c *Client) PutObserved(ctx context.Context, observed sdk.ObservedRelease) error {
	_, err := c.request(ctx, http.MethodPut, c.endpoint("observed"), observed, 256<<10)
	return err
}

func (c *Client) AckControl(ctx context.Context, id string) error {
	_, err := c.request(ctx, http.MethodPut, c.endpoint("control-acks", id), nil, 256<<10)
	return err
}

func (c *Client) PutRuntimeActionResult(ctx context.Context, id string, result sdk.RuntimeActionResult) error {
	_, err := c.request(ctx, http.MethodPut, c.endpoint("runtime-actions", id, "result"), result, 256<<10)
	return err
}

type ExchangeResponse struct {
	NodeToken string    `json:"nodeToken"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func ExchangeEnrollment(ctx context.Context, gatewayURL, enrollmentID, enrollmentToken string, httpClient *http.Client) (ExchangeResponse, error) {
	u, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ExchangeResponse{}, fmt.Errorf("node gateway URL must be an absolute HTTPS URL")
	}
	u.Path = path.Join(strings.TrimRight(u.Path, "/"), "v1", "node", "enrollments", enrollmentID, "exchange")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return ExchangeResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+enrollmentToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ExchangeResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ExchangeResponse{}, fmt.Errorf("node enrollment returned HTTP %d", resp.StatusCode)
	}
	var out ExchangeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return out, err
	}
	if !strings.HasPrefix(out.NodeToken, "cn_") {
		return out, fmt.Errorf("node enrollment returned invalid credential")
	}
	return out, nil
}
