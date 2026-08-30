package sdk

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed schema/change-request.v1.schema.json
var changeRequestSchemaJSON []byte

func ChangeRequestSchemaJSON() []byte {
	return append([]byte(nil), changeRequestSchemaJSON...)
}

// ChangeRequest is the transport-neutral, declarative input shared by agents,
// the CLI, and future HTTP/WebMCP adapters. It describes an outcome; it cannot
// provide arbitrary provider operations or credentials.
type ChangeRequest struct {
	APIVersion string            `yaml:"apiVersion" json:"apiVersion"`
	Kind       string            `yaml:"kind" json:"kind"`
	Metadata   Metadata          `yaml:"metadata" json:"metadata"`
	Spec       ChangeRequestSpec `yaml:"spec" json:"spec"`
}

type ChangeRequestSpec struct {
	System       string                    `yaml:"system" json:"system"`
	Summary      string                    `yaml:"summary" json:"summary"`
	Release      ChangeRequestRelease      `yaml:"release" json:"release"`
	Migration    *ChangeRequestMigration   `yaml:"migration,omitempty" json:"migration,omitempty"`
	Verification ChangeRequestVerification `yaml:"verification" json:"verification"`
}

type ChangeRequestRelease struct {
	Artifact    string            `yaml:"artifact" json:"artifact"`
	Command     []string          `yaml:"command" json:"command"`
	Environment map[string]string `yaml:"environment,omitempty" json:"environment,omitempty"`
	HealthPath  string            `yaml:"healthPath" json:"healthPath"`
	PublicPort  int               `yaml:"publicPort" json:"publicPort"`
}

type ChangeRequestMigration struct {
	Path    string `yaml:"path" json:"path"`
	ID      string `yaml:"id" json:"id"`
	Service string `yaml:"service" json:"service"`
}

type ChangeRequestVerification struct {
	Method         string `yaml:"method" json:"method"`
	Path           string `yaml:"path" json:"path"`
	ExpectedStatus int    `yaml:"expectedStatus" json:"expectedStatus"`
	BodyContains   string `yaml:"bodyContains,omitempty" json:"bodyContains,omitempty"`
}

func LoadChangeRequest(path string) (ChangeRequest, error) {
	file, err := os.Open(path)
	if err != nil {
		return ChangeRequest{}, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var request ChangeRequest
	if err := decoder.Decode(&request); err != nil {
		return ChangeRequest{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ChangeRequest{}, fmt.Errorf("parse %s: multiple YAML documents are not allowed", path)
		}
		return ChangeRequest{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := request.Validate(); err != nil {
		return ChangeRequest{}, err
	}
	return request, nil
}

func (r ChangeRequest) Validate() error {
	if r.APIVersion != APIVersion || r.Kind != "Change" {
		return fmt.Errorf("change request requires apiVersion %s and kind Change", APIVersion)
	}
	if !safeName.MatchString(r.Metadata.Name) || !safeName.MatchString(r.Spec.System) {
		return fmt.Errorf("change request requires valid metadata.name and spec.system")
	}
	if strings.TrimSpace(r.Spec.Summary) == "" {
		return fmt.Errorf("change request requires spec.summary")
	}
	release := r.Spec.Release
	if strings.TrimSpace(release.Artifact) == "" || len(release.Command) == 0 || !strings.HasPrefix(release.HealthPath, "/") || release.PublicPort < 1 || release.PublicPort > 65535 {
		return fmt.Errorf("change request release requires artifact, command, absolute healthPath, and valid publicPort")
	}
	for key := range release.Environment {
		if !environmentName.MatchString(key) {
			return fmt.Errorf("invalid environment name %q", key)
		}
	}
	if migration := r.Spec.Migration; migration != nil {
		if strings.TrimSpace(migration.Path) == "" || !safeName.MatchString(migration.ID) || !safeName.MatchString(migration.Service) {
			return fmt.Errorf("change request migration requires path, safe id, and safe service")
		}
	}
	verification := r.Spec.Verification
	if verification.Method == "" {
		verification.Method = "GET"
	}
	if verification.ExpectedStatus == 0 {
		verification.ExpectedStatus = 200
	}
	if verification.Method != "GET" || !strings.HasPrefix(verification.Path, "/") || verification.ExpectedStatus < 100 || verification.ExpectedStatus > 599 {
		return fmt.Errorf("change request verification requires GET, an absolute path, and a valid expectedStatus")
	}
	return nil
}

func (r ChangeRequest) DraftInput(system System) (DraftChangeInput, error) {
	if err := r.Validate(); err != nil {
		return DraftChangeInput{}, err
	}
	if r.Spec.System != system.Metadata.Name {
		return DraftChangeInput{}, fmt.Errorf("change request targets system %q, not %q", r.Spec.System, system.Metadata.Name)
	}
	verification := r.Spec.Verification
	if verification.Method == "" {
		verification.Method = "GET"
	}
	if verification.ExpectedStatus == 0 {
		verification.ExpectedStatus = 200
	}
	input := DraftChangeInput{
		Summary: r.Spec.Summary,
		Release: PublishReleaseInput{
			ArtifactPath: r.Spec.Release.Artifact,
			Command:      append([]string(nil), r.Spec.Release.Command...),
			Environment:  cloneStrings(r.Spec.Release.Environment),
			HealthPath:   r.Spec.Release.HealthPath,
			PublicPort:   r.Spec.Release.PublicPort,
		},
		Verification: ChangeVerification{Method: verification.Method, Path: verification.Path, ExpectedStatus: verification.ExpectedStatus, BodyContains: verification.BodyContains},
	}
	if r.Spec.Migration != nil {
		input.MigrationPath = r.Spec.Migration.Path
		input.MigrationID = r.Spec.Migration.ID
		input.Database = r.Spec.Migration.Service
	}
	return input, nil
}

func (c *Client) DraftChangeRequest(ctx context.Context, system System, request ChangeRequest) (Change, error) {
	input, err := request.DraftInput(system)
	if err != nil {
		return Change{}, err
	}
	return c.DraftChange(ctx, system, input)
}

func cloneStrings(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

const StarterChangeYAML = `apiVersion: canter.dev/v1alpha1
kind: Change
metadata:
  name: release-candidate
spec:
  system: my-application
  summary: Describe the production outcome this Change should create.
  release:
    artifact: release.tar.gz
    command: ["./app"]
    environment: {}
    healthPath: /health
    publicPort: 8080
  verification:
    method: GET
    path: /health
    expectedStatus: 200
`
