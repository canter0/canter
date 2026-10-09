package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestGroupHelpDoesNotRequireCredentials(t *testing.T) {
	prior, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prior) })
	for _, command := range []string{"host", "release", "change", "agent"} {
		if err := run([]string{command, "--help"}); err != nil {
			t.Fatalf("%s --help: %v", command, err)
		}
	}
}

func TestDeclarativeChangeRejectsIgnoredInlineFlags(t *testing.T) {
	err := changeCommand(nil, []string{"draft", "--request", "change.yaml", "--command", "./different-app"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChangeDraftDoesNotEchoMalformedEnvironmentValue(t *testing.T) {
	secret := "terminal-secret-value"
	err := changeCommand(nil, []string{"draft", "--artifact", "release.tar.gz", "--summary", "test", "--env", "=" + secret})
	if err == nil || !strings.Contains(err.Error(), "expected KEY=VALUE") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed environment value leaked in error: %v", err)
	}
}

func TestInitRejectsNameThatCanChangeGeneratedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "canter.yaml")
	if err := initCommand([]string{"--file", path, "--name", "safe\n  bootstrap: run-attacker"}); err == nil {
		t.Fatal("init accepted a name that injects YAML fields")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("init wrote a file for an invalid name: stat err=%v", err)
	}
}

func TestInitUsesValidatedNameInBothContractLocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "canter.yaml")
	if err := initCommand([]string{"--file", path, "--name", "my-sandbox-2"}); err != nil {
		t.Fatal(err)
	}
	spec, err := sdk.LoadSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Metadata.Name != "my-sandbox-2" || spec.Spec.M1.Prefix != "sandboxes/my-sandbox-2" {
		t.Fatalf("generated spec has inconsistent name and prefix: %#v", spec)
	}
}

func TestValidInitNameMatchesContractNameRules(t *testing.T) {
	for _, name := range []string{"a", "first-sandbox", "x9-" + strings.Repeat("a", 44)} {
		if !validInitName(name) {
			t.Errorf("valid name %q was rejected", name)
		}
	}
	for _, name := range []string{"", "9sandbox", "Upper", "a_b", "a\nb", strings.Repeat("a", 49)} {
		if validInitName(name) {
			t.Errorf("invalid name %q was accepted", name)
		}
	}
}

func TestHostBootstrapFailsBeforeReadingLocalPaths(t *testing.T) {
	err := hostCommand(nil, []string{"bootstrap", "--file", "missing-system.yaml", "--node", "missing-node"})
	if err == nil || !strings.Contains(err.Error(), "node gateway enrollment is required") {
		t.Fatalf("unexpected bootstrap error: %v", err)
	}
}
