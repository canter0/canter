package sdk

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const APIVersion = "canter.dev/v1alpha1"

var (
	safeName      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	safeM1Segment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ValidateM1Prefix validates an object-store namespace without imposing a
// product-specific layout. Control-plane callers additionally bind valid
// prefixes to a workspace; standalone SDK callers retain their existing
// freedom to choose any safe relative namespace.
func ValidateM1Prefix(prefix string) error {
	if prefix == "" || len(prefix) > 512 || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("m1 prefix must be a safe relative path of at most 512 characters")
	}
	for _, segment := range strings.Split(prefix, "/") {
		if !safeM1Segment.MatchString(segment) || segment == "." || segment == ".." {
			return fmt.Errorf("m1 prefix contains unsafe segment %q", segment)
		}
	}
	return nil
}

type Spec struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Desired  `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type Desired struct {
	Intent  string      `yaml:"intent" json:"intent"`
	Compute ComputeSpec `yaml:"compute" json:"compute"`
	M1      M1Spec      `yaml:"m1" json:"m1"`
	Policy  Policy      `yaml:"policy" json:"policy"`
}

type ComputeSpec struct {
	Class     string `yaml:"class" json:"class"`
	Image     string `yaml:"image" json:"image"`
	Replicas  int    `yaml:"replicas" json:"replicas"`
	Bootstrap string `yaml:"bootstrap,omitempty" json:"bootstrap,omitempty"`
}

type M1Spec struct {
	Prefix string `yaml:"prefix" json:"prefix"`
}

type Policy struct {
	MaxReplicas int `yaml:"maxReplicas" json:"maxReplicas"`
}

func LoadSpec(path string) (Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := yaml.Unmarshal(b, &s); err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

func (s Spec) Validate() error {
	var problems []string
	if s.APIVersion != APIVersion {
		problems = append(problems, "apiVersion must be "+APIVersion)
	}
	if s.Kind != "Sandbox" {
		problems = append(problems, "kind must be Sandbox")
	}
	if !safeName.MatchString(s.Metadata.Name) {
		problems = append(problems, "metadata.name must be lowercase, start with a letter, and contain at most 48 letters, digits, or hyphens")
	}
	if strings.TrimSpace(s.Spec.Intent) == "" {
		problems = append(problems, "spec.intent is required")
	}
	if s.Spec.Compute.Class == "" {
		problems = append(problems, "spec.compute.class is required")
	}
	if s.Spec.Compute.Image == "" {
		problems = append(problems, "spec.compute.image is required")
	}
	if s.Spec.Compute.Replicas < 1 {
		problems = append(problems, "spec.compute.replicas must be at least 1")
	}
	if s.Spec.Policy.MaxReplicas < 1 {
		problems = append(problems, "spec.policy.maxReplicas must be at least 1")
	}
	if s.Spec.Compute.Replicas > s.Spec.Policy.MaxReplicas {
		problems = append(problems, "requested replicas exceed policy.maxReplicas")
	}
	if err := ValidateM1Prefix(s.Spec.M1.Prefix); err != nil {
		problems = append(problems, "spec.m1.prefix must be a safe relative prefix")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

const StarterYAML = `apiVersion: canter.dev/v1alpha1
kind: Sandbox
metadata:
  name: first-sandbox
spec:
  intent: Run a disposable Linux workspace and prove that it booted.
  compute:
    class: c1
    image: ubuntu-24.04
    replicas: 1
    bootstrap: |
      echo "canter is alive" > /tmp/canter-message
  m1:
    prefix: sandboxes/first-sandbox
  policy:
    maxReplicas: 1
`
