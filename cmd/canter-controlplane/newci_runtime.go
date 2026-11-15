package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/canter0/canter/internal/controlplane"
)

// ciStartupConfig is the CI startup slice loaded from the environment before
// the HTTP listener binds. Managed carries the opt-in runtime configuration;
// CacheRoot and PublicURL configure the optional cache backend independently
// of the managed mode. No funding, budget, or quota values are ever read here.
type ciStartupConfig struct {
	Managed   controlplane.CIManagedRuntimeConfig
	CacheRoot string
	PublicURL string
}

// ciStartupConfigFromEnv loads CI startup configuration. The managed mode is
// trimmed but case-sensitive; empty or "off" ignores every other managed
// field and reads no files. Errors name only the environment variable, never
// a path, file content, token, or raw mode value.
func ciStartupConfigFromEnv(getenv func(string) string) (ciStartupConfig, error) {
	var out ciStartupConfig
	mode := strings.TrimSpace(getenv("CANTER_CI_MANAGED_MODE"))
	switch mode {
	case "", "off":
		mode = string(controlplane.CIManagedOff)
	case string(controlplane.CIManagedRecover), string(controlplane.CIManagedRun):
	default:
		return out, fmt.Errorf("CANTER_CI_MANAGED_MODE is invalid")
	}
	cacheRoot := strings.TrimSpace(getenv("CANTER_CI_CACHE_ROOT"))
	publicURL := strings.TrimSpace(getenv("CANTER_CI_PUBLIC_URL"))
	out.CacheRoot = cacheRoot
	out.PublicURL = publicURL
	// The cache root is validated whenever configured, in any mode. The
	// public URL is required and shape-checked only when it is actually
	// consumed: run mode, or any mode with a cache root. A bare "off" mode
	// with no cache ignores stale URL values entirely.
	needPublic := mode == string(controlplane.CIManagedRun) || cacheRoot != ""
	if cacheRoot != "" && !filepath.IsAbs(cacheRoot) {
		return out, fmt.Errorf("CANTER_CI_CACHE_ROOT must be an absolute path")
	}
	if needPublic {
		if publicURL == "" {
			return out, fmt.Errorf("CANTER_CI_PUBLIC_URL is required")
		}
		if err := ciStartupHTTPSOrigin(publicURL); err != nil {
			return out, fmt.Errorf("CANTER_CI_PUBLIC_URL must be an explicit HTTPS origin")
		}
	}
	if mode == string(controlplane.CIManagedOff) {
		// Off ignores stale inactive managed options and performs zero file
		// opens.
		return out, nil
	}
	out.Managed.Mode = controlplane.CIManagedMode(mode)
	out.Managed.PublicURL = publicURL
	enforcerURL := strings.TrimSpace(getenv("CANTER_CI_ENFORCER_URL"))
	if enforcerURL == "" {
		return out, fmt.Errorf("CANTER_CI_ENFORCER_URL is required")
	}
	out.Managed.EnforcerURL = enforcerURL
	tokenPath := strings.TrimSpace(getenv("CANTER_CI_ENFORCER_TOKEN_FILE"))
	if tokenPath == "" {
		return out, fmt.Errorf("CANTER_CI_ENFORCER_TOKEN_FILE is required")
	}
	tokenRaw, err := ciStartupReadFile("CANTER_CI_ENFORCER_TOKEN_FILE", tokenPath, 4096)
	if err != nil {
		return out, err
	}
	token := strings.TrimSpace(string(tokenRaw))
	clear(tokenRaw)
	if len(token) < 32 {
		return out, fmt.Errorf("CANTER_CI_ENFORCER_TOKEN_FILE is invalid")
	}
	out.Managed.EnforcerToken = []byte(token)
	if mode == string(controlplane.CIManagedRecover) {
		// Recovery requires only the enforcer URL and token; profile and key
		// fields are ignored and never opened.
		return out, nil
	}
	profilePath := strings.TrimSpace(getenv("CANTER_CI_MANAGED_PROFILE_FILE"))
	if profilePath == "" {
		return out, fmt.Errorf("CANTER_CI_MANAGED_PROFILE_FILE is required")
	}
	profile, err := ciStartupReadFile("CANTER_CI_MANAGED_PROFILE_FILE", profilePath, 32768)
	if err != nil {
		return out, err
	}
	if len(profile) < 1 {
		return out, fmt.Errorf("CANTER_CI_MANAGED_PROFILE_FILE is invalid")
	}
	out.Managed.ProfileJSON = profile
	keyPath := strings.TrimSpace(getenv("CANTER_CI_ADMISSION_KEY_FILE"))
	if keyPath == "" {
		return out, fmt.Errorf("CANTER_CI_ADMISSION_KEY_FILE is required")
	}
	key, err := ciStartupReadFile("CANTER_CI_ADMISSION_KEY_FILE", keyPath, 32)
	if err != nil {
		return out, err
	}
	if len(key) != 32 {
		clear(key)
		return out, fmt.Errorf("CANTER_CI_ADMISSION_KEY_FILE is invalid")
	}
	out.Managed.SigningKey = key
	return out, nil
}

// ciStartupHTTPSOrigin performs the loader-level shape check for the explicit
// CI external origin: absolute HTTPS URL with a host and no userinfo, query,
// or fragment. Full origin normalization happens in the runtime and cache
// constructors.
func ciStartupHTTPSOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return fmt.Errorf("invalid")
	}
	return nil
}

// ciStartupReadFile reads one private configuration file with no symlink,
// exact owner-only permissions, and a bounded size. Every failure is
// sanitized to the environment variable name.
func ciStartupReadFile(envName, path string, max int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s must be an absolute path", envName)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s could not be read", envName)
	}
	perm := info.Mode().Perm()
	if perm != 0o400 && perm != 0o600 {
		return nil, fmt.Errorf("%s must be a private regular file (0400 or 0600)", envName)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("%s could not be read", envName)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s could not be read", envName)
	}
	if p := opened.Mode().Perm(); p != 0o400 && p != 0o600 {
		return nil, fmt.Errorf("%s must be a private regular file (0400 or 0600)", envName)
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, fmt.Errorf("%s could not be read", envName)
	}
	return raw, nil
}
