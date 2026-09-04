package computeclass

import "testing"

func TestNormalizePublicFailureUpgradesExactLegacyClassError(t *testing.T) {
	got, ok := NormalizePublicFailure(`unsupported compute class "shared"`)
	if !ok || got != `unsupported_compute_class: host class "shared" is unsupported; supported classes: c1, c2, c3` {
		t.Fatalf("legacy failure normalized to %q, ok=%t", got, ok)
	}
	for _, unsafe := range []string{
		`provider request failed`,
		`unsupported compute class "shared" leaked-provider-detail`,
		`prefix: unsupported compute class "shared"`,
	} {
		if got, ok := NormalizePublicFailure(unsafe); ok || got != "" {
			t.Fatalf("unsafe failure was made public: %q -> %q", unsafe, got)
		}
	}
}
