package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Check before migrations: activating strict verification without delivery
// configuration would lock existing password users out during a release.
func requireProductionAuth(publicURL string, getenv func(string) string) error {
	origin, err := url.Parse(publicURL)
	if err != nil || origin.Hostname() == "" {
		return fmt.Errorf("invalid authentication public URL")
	}
	host := strings.ToLower(origin.Hostname())
	ip := net.ParseIP(host)
	if host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	missing := []string{}
	for _, key := range []string{"CANTER_SECRETS_KEY_FILE", "RESEND_API_KEY", "RESEND_WEBHOOK_SECRET", "CANTER_TURNSTILE_SITE_KEY", "CANTER_TURNSTILE_SECRET"} {
		if strings.TrimSpace(getenv(key)) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("authentication rollout requires configuration before migration: %s", strings.Join(missing, ", "))
	}
	return nil
}
