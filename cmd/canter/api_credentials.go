package main

import (
	"errors"
	"os"
	"strings"
	"unicode/utf8"
)

// errNoAPICredential reports that no usable credential was configured; it is
// separate from malformed input so callers can keep their own guidance.
var errNoAPICredential = errors.New("no Canter credential configured")

var errMalformedAPICredential = errors.New("Canter credential value is malformed")

// validAPICredentialToken accepts only printable ASCII without whitespace so
// a valid value is always a usable HTTP bearer token. The error is generic:
// it never echoes the credential.
func validAPICredentialToken(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// resolveAPICredential picks the bearer credential for general resource APIs.
// Personal API keys (CANTER_API_KEY) act with the account's current workspace
// role; agent installation access tokens (CANTER_AGENT_ACCESS_TOKEN) act with
// the installation's granted authority. Ambient environment beats the
// explicit env-file, and a personal key beats an agent token at each level.
// Blank trimmed values are ignored, and a malformed token is rejected with a
// generic error that never echoes the value. Nothing is persisted or logged.
func resolveAPICredential(values map[string]string) (token string, personal bool, err error) {
	candidates := []struct {
		value    string
		personal bool
	}{
		{os.Getenv("CANTER_API_KEY"), true},
		{os.Getenv("CANTER_AGENT_ACCESS_TOKEN"), false},
		{values["CANTER_API_KEY"], true},
		{values["CANTER_AGENT_ACCESS_TOKEN"], false},
	}
	for _, candidate := range candidates {
		value := strings.TrimSpace(candidate.value)
		if value == "" {
			continue
		}
		if !validAPICredentialToken(value) {
			return "", false, errMalformedAPICredential
		}
		return value, candidate.personal, nil
	}
	return "", false, errNoAPICredential
}

// resolvePersonalAPICredential resolves only a personal API key for
// account-identity endpoints that do not accept agent tokens. It inspects
// CANTER_API_KEY alone: an ambient agent token must not mask an explicit
// file-provided personal key.
func resolvePersonalAPICredential(values map[string]string) (string, error) {
	for _, candidate := range []string{os.Getenv("CANTER_API_KEY"), values["CANTER_API_KEY"]} {
		value := strings.TrimSpace(candidate)
		if value == "" {
			continue
		}
		if !validAPICredentialToken(value) {
			return "", errMalformedAPICredential
		}
		return value, nil
	}
	return "", errors.New("no personal API key: set CANTER_API_KEY or use --env-file (create a key in Settings > API keys)")
}
