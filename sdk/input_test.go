package sdk

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeInput(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContractLoadersRejectUnknownFieldsAndTrailingDocuments(t *testing.T) {
	validSpecYAML := StarterYAML
	validSystem, err := NewSystem("input-test", "test the input parser").OnHost("c1", 1, 1024, 384).
		WithM1("systems/input-test").Provide(SystemService{
		Name: "web", Kind: "web", Isolation: "process", Instances: 1,
		Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080},
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	systemYAML, err := yaml.Marshal(validSystem)
	if err != nil {
		t.Fatal(err)
	}
	validChange := StarterChangeYAML
	for name, load := range map[string]struct {
		valid   string
		unknown string
		loader  func(string) error
	}{
		"spec":   {valid: validSpecYAML, unknown: "\nunknownField: ignored\n", loader: func(path string) error { _, err := LoadSpec(path); return err }},
		"system": {valid: string(systemYAML), unknown: "\nunknownField: ignored\n", loader: func(path string) error { _, err := LoadSystem(path); return err }},
		"change": {valid: validChange, unknown: "\n  unexpected: ignored\n", loader: func(path string) error { _, err := LoadChangeRequest(path); return err }},
	} {
		t.Run(name+" valid", func(t *testing.T) {
			if err := load.loader(writeInput(t, load.valid)); err != nil {
				t.Fatalf("valid fixture rejected: %v", err)
			}
		})
		t.Run(name+" unknown", func(t *testing.T) {
			if err := load.loader(writeInput(t, load.valid+load.unknown)); err == nil {
				t.Fatal("unknown field was accepted")
			}
		})
		t.Run(name+" documents", func(t *testing.T) {
			if err := load.loader(writeInput(t, load.valid+"\n---\n{}")); err == nil {
				t.Fatal("second YAML document was accepted")
			}
		})
	}
}
