package controlplane

import (
	"context"
	"strings"
	"testing"
)

func TestPrepareNodeBootstrapFailsClosedForMissingOrInvalidGatewayURL(t *testing.T) {
	for _, gatewayURL := range []string{
		"",
		"http://control.canter.test",
		"control.canter.test",
		"https://user@control.canter.test",
		"https://control.canter.test?token=bad",
		"https://control.canter.test#fragment",
	} {
		t.Run(gatewayURL, func(t *testing.T) {
			service := &Service{NodeGatewayURL: gatewayURL}
			_, _, err := service.PrepareNodeBootstrap(context.Background(), "wsp_test", "system")
			if err == nil || !strings.Contains(err.Error(), "absolute HTTPS URL") {
				t.Fatalf("gateway URL %q did not fail closed: %v", gatewayURL, err)
			}
		})
	}
}
