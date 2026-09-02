package controlplane

import "testing"

func TestValidatedClientInstance(t *testing.T) {
	if got, err := validatedClientInstance("  conversation-7  "); err != nil || got != "conversation-7" {
		t.Fatalf("valid clientInstance = %q, %v", got, err)
	}
	for _, invalid := range []string{"", "   ", "conversation\n7", string(make([]byte, 201))} {
		if _, err := validatedClientInstance(invalid); err == nil {
			t.Fatalf("accepted invalid clientInstance %q", invalid)
		}
	}
}

func TestValidAgentLabel(t *testing.T) {
	if !validAgentLabel("Codex on laptop", 120) {
		t.Fatal("rejected a valid agent label")
	}
	for _, invalid := range []string{"", "codex\x00desktop", "codex\ndesktop"} {
		if validAgentLabel(invalid, 120) {
			t.Fatalf("accepted invalid agent label %q", invalid)
		}
	}
}
