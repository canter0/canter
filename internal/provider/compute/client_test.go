package compute

import "testing"

func TestNormalizeImage(t *testing.T) {
	if normalizeImage("Ubuntu-24.04-amd64") != normalizeImage("ubuntu-24.04") {
		t.Fatal("public image alias does not resolve to the provider-neutral form")
	}
}
