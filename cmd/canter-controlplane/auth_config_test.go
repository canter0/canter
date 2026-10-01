package main

import "testing"

func TestProductionAuthRequiresDeliveryAndBotConfiguration(t *testing.T) {
	empty := func(string) string { return "" }
	for _, origin := range []string{"http://localhost:3000", "http://127.0.0.1:3000", "http://[::1]:3000"} {
		if err := requireProductionAuth(origin, empty); err != nil {
			t.Fatal(origin, err)
		}
	}
	if requireProductionAuth("https://canter.dev", empty) == nil {
		t.Fatal("production without mail allowed")
	}
	keys := []string{"CANTER_SECRETS_KEY_FILE", "RESEND_API_KEY", "RESEND_WEBHOOK_SECRET", "CANTER_TURNSTILE_SITE_KEY", "CANTER_TURNSTILE_SECRET"}
	for _, missing := range keys {
		read := func(k string) string {
			if k == missing {
				return ""
			}
			return "configured"
		}
		if requireProductionAuth("https://canter.dev", read) == nil {
			t.Fatal("missing key accepted", missing)
		}
	}
	if err := requireProductionAuth("https://canter.dev", func(string) string { return "configured" }); err != nil {
		t.Fatal(err)
	}
}
