package main

import (
	"os"
	"strings"
	"testing"
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
