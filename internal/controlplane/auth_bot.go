package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var turnstileHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (h *HTTPServer) verifyBot(w http.ResponseWriter, r *http.Request, token string) bool {
	if h.config.Auth.TurnstileSecret == "" {
		return true
	}
	if token == "" || len(token) > 2048 {
		writeError(w, http.StatusBadRequest, errors.New("complete the security check"))
		return false
	}
	values := url.Values{"secret": {h.config.Auth.TurnstileSecret}, "response": {token}, "remoteip": {requestIP(r)}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(values.Encode()))
	if err != nil {
		writeStoreError(w, err)
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := credentialHTTPClient(turnstileHTTPClient).Do(req)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("security check temporarily unavailable"))
		return false
	}
	defer res.Body.Close()
	var out struct {
		Success  bool   `json:"success"`
		Hostname string `json:"hostname"`
		Action   string `json:"action"`
	}
	origin, _ := url.Parse(h.config.PublicURL)
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&out) != nil || !out.Success || out.Hostname != origin.Hostname() || out.Action != "auth" {
		writeError(w, http.StatusBadRequest, errors.New("security check expired; try again"))
		return false
	}
	return true
}
