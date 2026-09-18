package controlplane

import (
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/oauth2"
)

var githubAppSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

func (h *HTTPServer) githubConnectionDefaults() githubConnection {
	state := githubConnection{Enabled: h.oauth["github"] != nil || h.oauth["github-app"] != nil, AppEnabled: h.oauth["github-app"] != nil}
	if state.AppEnabled && githubAppSlug.MatchString(h.config.GitHubAppSlug) {
		state.InstallURL = "https://github.com/apps/" + h.config.GitHubAppSlug + "/installations/new"
	}
	return state
}

func githubRefreshBinding(account, workspace string) []byte {
	return []byte("refresh\x00" + account + "\x00" + workspace)
}
func openGitHubToken(aead cipher.AEAD, sealed, binding []byte) ([]byte, error) {
	if len(sealed) < aead.NonceSize() {
		return nil, errGitHubReconnect
	}
	value, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], binding)
	if err != nil || len(value) == 0 {
		return nil, errGitHubReconnect
	}
	return value, nil
}
func sealGitHubToken(aead cipher.AEAD, value string, binding []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(value), binding), nil
}
func (h *HTTPServer) sealGitHubTokens(provider, account, workspace string, token *oauth2.Token) ([]byte, []byte, *time.Time, error) {
	aead, err := h.githubCipher(provider)
	if err != nil || token.AccessToken == "" {
		return nil, nil, nil, errGitHubReconnect
	}
	access, err := sealGitHubToken(aead, token.AccessToken, githubTokenBinding(account, workspace))
	if err != nil {
		return nil, nil, nil, err
	}
	if provider != "github-app" {
		return access, nil, nil, nil
	}
	// Expiring user tokens are required for this app; refuse malformed or
	// accidentally non-expiring credentials rather than saving them silently.
	seconds, err := strconv.ParseFloat(fmt.Sprint(token.Extra("refresh_token_expires_in")), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || math.Trunc(seconds) != seconds || seconds <= 0 || seconds > 366*24*60*60 || token.RefreshToken == "" || !token.Expiry.After(time.Now()) {
		return nil, nil, nil, errGitHubReconnect
	}
	refresh, err := sealGitHubToken(aead, token.RefreshToken, githubRefreshBinding(account, workspace))
	expiry := time.Now().Add(time.Duration(seconds) * time.Second)
	return access, refresh, &expiry, err
}
