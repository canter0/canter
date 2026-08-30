package sdk

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

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
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Sprintf("public health returned HTTP %d", response.StatusCode)
	}
	return response.StatusCode, "public endpoint is reachable"
}
