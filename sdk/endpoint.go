package sdk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const endpointBodyLimit = 1 << 20

// Health checks share connections across polls and never leave the approved
// endpoint by following a redirect from a tenant-controlled application.
var endpointTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 2
	return transport
}()

func endpointHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     endpointTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func readEndpointBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, endpointBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > endpointBodyLimit {
		return nil, fmt.Errorf("public endpoint response exceeded the 1 MiB limit")
	}
	return raw, nil
}

// PublicEndpointObservation distinguishes internal process health from the
// reachability a real user or agent sees through the managed public endpoint.
type PublicEndpointObservation struct {
	SchemaVersion string    `json:"schemaVersion"`
	System        string    `json:"system"`
	Version       string    `json:"version,omitempty"`
	Phase         string    `json:"phase"`
	URL           string    `json:"url,omitempty"`
	StatusCode    int       `json:"statusCode,omitempty"`
	Message       string    `json:"message,omitempty"`
	ObservedAt    time.Time `json:"observedAt"`
}

// VerifyPublicEndpoint performs the exact deterministic HTTP assertion bound
// into an initial-deployment proposal. It is separate from WaitPublicEndpoint,
// which waits on the release manifest's health path.
func (c *Client) VerifyPublicEndpoint(ctx context.Context, system System, version string, verification ChangeVerification) (PublicEndpointObservation, error) {
	if verification.Method == "" {
		verification.Method = http.MethodGet
	}
	if verification.ExpectedStatus == 0 {
		verification.ExpectedStatus = http.StatusOK
	}
	if verification.Method != http.MethodGet || !strings.HasPrefix(verification.Path, "/") || verification.ExpectedStatus < 100 || verification.ExpectedStatus > 599 {
		return PublicEndpointObservation{}, fmt.Errorf("verification requires GET, an absolute path, and a valid expected status")
	}
	state, err := c.SystemHostStatus(ctx, system)
	if err != nil {
		return PublicEndpointObservation{}, err
	}
	if len(state.Resources) != 1 || state.Resources[0].Address == "" {
		return PublicEndpointObservation{}, fmt.Errorf("system host has no public address")
	}
	port, err := systemPublicPort(system)
	if err != nil {
		return PublicEndpointObservation{}, err
	}
	url := "http://" + net.JoinHostPort(state.Resources[0].Address, fmt.Sprint(port)) + verification.Path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PublicEndpointObservation{}, err
	}
	response, err := endpointHTTPClient(5 * time.Second).Do(request)
	if err != nil {
		return PublicEndpointObservation{}, err
	}
	defer response.Body.Close()
	body, err := readEndpointBody(response.Body)
	if err != nil {
		return PublicEndpointObservation{}, err
	}
	observation := PublicEndpointObservation{SchemaVersion: "v1", System: system.Metadata.Name, Version: version, Phase: "failed", URL: url, StatusCode: response.StatusCode, ObservedAt: time.Now().UTC()}
	switch {
	case response.StatusCode != verification.ExpectedStatus:
		observation.Message = fmt.Sprintf("expected HTTP %d, got HTTP %d", verification.ExpectedStatus, response.StatusCode)
	case verification.BodyContains != "" && !bytes.Contains(body, []byte(verification.BodyContains)):
		observation.Message = "response body did not contain the approved marker"
	default:
		observation.Phase = "ready"
		observation.Message = "approved public verification passed"
	}
	if _, recordErr := c.recordEndpointObservation(ctx, system, observation); recordErr != nil {
		return PublicEndpointObservation{}, recordErr
	}
	if observation.Phase != "ready" {
		return observation, fmt.Errorf("public verification failed: %s", observation.Message)
	}
	return observation, nil
}

type ReleaseView struct {
	Release        ObservedRelease           `json:"release"`
	PublicEndpoint PublicEndpointObservation `json:"publicEndpoint"`
}

func (c *Client) InspectRelease(ctx context.Context, system System) (ReleaseView, error) {
	release, err := c.ReleaseStatus(ctx, system)
	if err != nil {
		return ReleaseView{}, err
	}
	observation, err := c.ObservePublicEndpoint(ctx, system, release)
	if err != nil {
		return ReleaseView{}, err
	}
	return ReleaseView{Release: release, PublicEndpoint: observation}, nil
}

func (c *Client) ObservePublicEndpoint(ctx context.Context, system System, release ObservedRelease) (PublicEndpointObservation, error) {
	now := time.Now().UTC()
	observation := PublicEndpointObservation{SchemaVersion: "v1", System: system.Metadata.Name, Version: release.RunningVersion, Phase: "waiting", ObservedAt: now}
	if release.Phase != "running" || !release.Healthy || release.RunningVersion == "" {
		observation.Message = "release is not internally healthy"
		return c.recordEndpointObservation(ctx, system, observation)
	}
	state, err := c.SystemHostStatus(ctx, system)
	if err != nil {
		return PublicEndpointObservation{}, fmt.Errorf("inspect public endpoint host: %w", err)
	}
	if len(state.Resources) != 1 || state.Resources[0].Address == "" {
		observation.Message = "host has no public address"
		return c.recordEndpointObservation(ctx, system, observation)
	}
	port, err := systemPublicPort(system)
	if err != nil {
		return PublicEndpointObservation{}, err
	}
	path := "/health"
	var manifest ReleaseManifest
	if found, getErr := c.m1.GetOptional(ctx, releaseKey(system, release.RunningVersion), &manifest); getErr != nil {
		return PublicEndpointObservation{}, getErr
	} else if found && strings.HasPrefix(manifest.HealthPath, "/") {
		path = manifest.HealthPath
	}
	observation.URL = "http://" + net.JoinHostPort(state.Resources[0].Address, fmt.Sprint(port)) + path
	status, message := observeHTTP(ctx, observation.URL)
	observation.StatusCode = status
	observation.Message = message
	if status >= 200 && status < 300 {
		observation.Phase = "ready"
	}
	return c.recordEndpointObservation(ctx, system, observation)
}

func (c *Client) WaitPublicEndpoint(ctx context.Context, system System) (ReleaseView, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var last ReleaseView
	for {
		view, err := c.InspectRelease(ctx, system)
		if err != nil {
			return last, err
		}
		last = view
		if view.PublicEndpoint.Phase == "ready" {
			return view, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("public endpoint did not become ready: %s: %w", last.PublicEndpoint.Message, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Client) recordEndpointObservation(ctx context.Context, system System, observation PublicEndpointObservation) (PublicEndpointObservation, error) {
	key := strings.TrimRight(system.Spec.M1.Prefix, "/") + "/public-endpoint.json"
	if err := c.m1.PutJSON(ctx, key, observation); err != nil {
		return PublicEndpointObservation{}, fmt.Errorf("record public endpoint observation: %w", err)
	}
	return observation, nil
}

func observeHTTP(ctx context.Context, endpoint string) (int, string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err.Error()
	}
	client := endpointHTTPClient(3 * time.Second)
	response, err := client.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	// Small health bodies can be drained cheaply so the connection is reusable;
	// large or streaming bodies remain bounded by this request's deadline.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Sprintf("public health returned HTTP %d", response.StatusCode)
	}
	return response.StatusCode, "public endpoint is reachable"
}
