package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ciEnvMap(entries map[string]string) func(string) string {
	return func(k string) string { return entries[k] }
}

func ciWriteSecret(t *testing.T, dir, name string, data []byte, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	_ = os.Remove(path)
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func ciRunEnv(t *testing.T, dir string) map[string]string {
	t.Helper()
	profile := []byte(`{"version":1,"region":"r1","imageId":"i1","networkId":"n1","bootstrapUrl":"https://ci.example.test/v1/ci/runner-bootstrap","runnerVersion":"2.330.0","runnerSha256":"` + strings.Repeat("a", 64) + `","guestBundleSha256":"` + strings.Repeat("b", 64) + `","machines":[{"sizeId":"linux-x64-2","flavorId":"f2","vcpus":2,"memoryMiB":8192,"diskGiB":80}]}`)
	token := []byte(strings.Repeat("t", 40))
	return map[string]string{
		"CANTER_CI_MANAGED_MODE":         "run",
		"CANTER_CI_MANAGED_PROFILE_FILE": ciWriteSecret(t, dir, "profile.json", profile, 0o600),
		"CANTER_CI_ADMISSION_KEY_FILE":   ciWriteSecret(t, dir, "key.bin", []byte(strings.Repeat("k", 32)), 0o600),
		"CANTER_CI_ENFORCER_URL":         "https://enforcer.example.test",
		"CANTER_CI_ENFORCER_TOKEN_FILE":  ciWriteSecret(t, dir, "token.txt", token, 0o400),
		"CANTER_CI_PUBLIC_URL":           "https://ci.example.test",
	}
}

func TestCIStartupConfigFromEnv(t *testing.T) {
	t.Run("off ignores stale managed fields and reads no files", func(t *testing.T) {
		env := map[string]string{
			"CANTER_CI_MANAGED_MODE":         "off",
			"CANTER_CI_MANAGED_PROFILE_FILE": "/nonexistent/profile.json",
			"CANTER_CI_ADMISSION_KEY_FILE":   "/nonexistent/key.bin",
			"CANTER_CI_ENFORCER_URL":         "not a url",
			"CANTER_CI_ENFORCER_TOKEN_FILE":  "/nonexistent/token.txt",
			"CANTER_CI_PUBLIC_URL":           "also not a url",
		}
		cfg, err := ciStartupConfigFromEnv(ciEnvMap(env))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Managed.Mode != "" || len(cfg.Managed.ProfileJSON) != 0 || len(cfg.Managed.SigningKey) != 0 {
			t.Fatalf("off mode loaded managed config: %+v", cfg.Managed)
		}
	})

	t.Run("empty mode is off", func(t *testing.T) {
		cfg, err := ciStartupConfigFromEnv(ciEnvMap(map[string]string{}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Managed.Mode != "" || cfg.CacheRoot != "" || cfg.PublicURL != "" {
			t.Fatalf("unexpected config: %+v", cfg)
		}
	})

	t.Run("unknown mode fails without raw value", func(t *testing.T) {
		_, err := ciStartupConfigFromEnv(ciEnvMap(map[string]string{
			"CANTER_CI_MANAGED_MODE": "TOP-SECRET-MODE-VALUE",
		}))
		if err == nil || !strings.Contains(err.Error(), "CANTER_CI_MANAGED_MODE") ||
			strings.Contains(err.Error(), "TOP-SECRET-MODE-VALUE") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("run loads exact config", func(t *testing.T) {
		dir := t.TempDir()
		env := ciRunEnv(t, dir)
		cfg, err := ciStartupConfigFromEnv(ciEnvMap(env))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Managed.Mode != "run" || cfg.PublicURL != "https://ci.example.test" ||
			cfg.Managed.PublicURL != "https://ci.example.test" || cfg.Managed.EnforcerURL != "https://enforcer.example.test" {
			t.Fatalf("unexpected config: %+v", cfg.Managed)
		}
		if len(cfg.Managed.SigningKey) != 32 || string(cfg.Managed.SigningKey) != strings.Repeat("k", 32) {
			t.Fatal("signing key not raw 32 bytes")
		}
		if string(cfg.Managed.EnforcerToken) != strings.Repeat("t", 40) {
			t.Fatal("enforcer token mismatch")
		}
		if len(cfg.Managed.ProfileJSON) == 0 {
			t.Fatal("profile not loaded")
		}
	})

	t.Run("key file exactness", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			data []byte
			ok   bool
		}{
			{"exact 32", []byte(strings.Repeat("k", 32)), true},
			{"31 bytes", []byte(strings.Repeat("k", 31)), false},
			{"33 bytes", []byte(strings.Repeat("k", 33)), false},
			{"32 plus newline", append([]byte(strings.Repeat("k", 32)), '\n'), false},
			{"31 chars plus newline is 32 raw bytes", append([]byte("\n"), []byte(strings.Repeat("k", 31))...), true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				env := ciRunEnv(t, dir)
				env["CANTER_CI_ADMISSION_KEY_FILE"] = ciWriteSecret(t, dir, "key2.bin", tc.data, 0o600)
				_, err := ciStartupConfigFromEnv(ciEnvMap(env))
				if tc.ok && err != nil {
					t.Fatalf("expected success: %v", err)
				}
				if !tc.ok && err == nil {
					t.Fatal("expected rejection")
				}
			})
		}
	})

	t.Run("file rejection matrix", func(t *testing.T) {
		dir := t.TempDir()
		// Symlink.
		target := ciWriteSecret(t, dir, "real.bin", []byte(strings.Repeat("k", 32)), 0o600)
		link := filepath.Join(dir, "link.bin")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		// Group/world readable.
		groupReadable := ciWriteSecret(t, dir, "group.bin", []byte(strings.Repeat("k", 32)), 0o600)
		if err := os.Chmod(groupReadable, 0o640); err != nil {
			t.Fatal(err)
		}
		worldReadable := ciWriteSecret(t, dir, "world.bin", []byte(strings.Repeat("k", 32)), 0o600)
		if err := os.Chmod(worldReadable, 0o644); err != nil {
			t.Fatal(err)
		}
		// Directory.
		subdir := filepath.Join(dir, "subdir")
		if err := os.Mkdir(subdir, 0o700); err != nil {
			t.Fatal(err)
		}
		// Oversize profile.
		bigProfile := ciWriteSecret(t, dir, "big.json", make([]byte, 32769), 0o600)
		// Missing.
		missing := filepath.Join(dir, "missing.bin")

		for _, tc := range []struct {
			name string
			env  string
			path string
		}{
			{"symlink", "CANTER_CI_ADMISSION_KEY_FILE", link},
			{"group readable", "CANTER_CI_ADMISSION_KEY_FILE", groupReadable},
			{"world readable", "CANTER_CI_ADMISSION_KEY_FILE", worldReadable},
			{"directory", "CANTER_CI_ADMISSION_KEY_FILE", subdir},
			{"missing", "CANTER_CI_ADMISSION_KEY_FILE", missing},
			{"oversize profile", "CANTER_CI_MANAGED_PROFILE_FILE", bigProfile},
			{"relative path", "CANTER_CI_ADMISSION_KEY_FILE", "relative/key.bin"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				env := ciRunEnv(t, dir)
				env[tc.env] = tc.path
				_, err := ciStartupConfigFromEnv(ciEnvMap(env))
				if err == nil {
					t.Fatal("expected rejection")
				}
				if !strings.Contains(err.Error(), tc.env) {
					t.Fatalf("error must name env var only: %v", err)
				}
			})
		}
	})

	t.Run("0400 accepted", func(t *testing.T) {
		dir := t.TempDir()
		env := ciRunEnv(t, dir)
		env["CANTER_CI_ADMISSION_KEY_FILE"] = ciWriteSecret(t, dir, "key400.bin", []byte(strings.Repeat("k", 32)), 0o400)
		cfg, err := ciStartupConfigFromEnv(ciEnvMap(env))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Managed.SigningKey) != 32 {
			t.Fatal("key not loaded")
		}
	})

	t.Run("recover requires url and token only", func(t *testing.T) {
		dir := t.TempDir()
		tokenPath := ciWriteSecret(t, dir, "token.txt", []byte("  "+strings.Repeat("t", 40)+"\n"), 0o600)
		env := map[string]string{
			"CANTER_CI_MANAGED_MODE":        "recover",
			"CANTER_CI_ENFORCER_URL":        "https://enforcer.example.test",
			"CANTER_CI_ENFORCER_TOKEN_FILE": tokenPath,
		}
		cfg, err := ciStartupConfigFromEnv(ciEnvMap(env))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Managed.Mode != "recover" || len(cfg.Managed.EnforcerToken) != 40 ||
			len(cfg.Managed.ProfileJSON) != 0 || len(cfg.Managed.SigningKey) != 0 {
			t.Fatalf("unexpected recover config: %+v", cfg.Managed)
		}
		// Missing token file fails.
		env["CANTER_CI_ENFORCER_TOKEN_FILE"] = ""
		if _, err := ciStartupConfigFromEnv(ciEnvMap(env)); err == nil {
			t.Fatal("expected missing token rejection")
		}
		env["CANTER_CI_ENFORCER_TOKEN_FILE"] = tokenPath
		env["CANTER_CI_ENFORCER_URL"] = ""
		if _, err := ciStartupConfigFromEnv(ciEnvMap(env)); err == nil {
			t.Fatal("expected missing url rejection")
		}
	})

	t.Run("run requires all fields", func(t *testing.T) {
		dir := t.TempDir()
		env := ciRunEnv(t, dir)
		for _, name := range []string{
			"CANTER_CI_MANAGED_PROFILE_FILE", "CANTER_CI_ADMISSION_KEY_FILE",
			"CANTER_CI_ENFORCER_URL", "CANTER_CI_ENFORCER_TOKEN_FILE", "CANTER_CI_PUBLIC_URL",
		} {
			sub := map[string]string{}
			for k, v := range env {
				sub[k] = v
			}
			delete(sub, name)
			if _, err := ciStartupConfigFromEnv(ciEnvMap(sub)); err == nil {
				t.Fatalf("expected %s requirement", name)
			}
		}
	})

	t.Run("token trimmed and bounded", func(t *testing.T) {
		dir := t.TempDir()
		env := ciRunEnv(t, dir)
		// Short after trim.
		env["CANTER_CI_ENFORCER_TOKEN_FILE"] = ciWriteSecret(t, dir, "tok2.txt", []byte("  short  "), 0o600)
		if _, err := ciStartupConfigFromEnv(ciEnvMap(env)); err == nil {
			t.Fatal("expected short token rejection")
		}
		// Oversize raw.
		env["CANTER_CI_ENFORCER_TOKEN_FILE"] = ciWriteSecret(t, dir, "tok3.txt", make([]byte, 4097), 0o600)
		if _, err := ciStartupConfigFromEnv(ciEnvMap(env)); err == nil {
			t.Fatal("expected oversize token rejection")
		}
	})

	t.Run("public url required for run and cache", func(t *testing.T) {
		dir := t.TempDir()
		env := ciRunEnv(t, dir)
		delete(env, "CANTER_CI_PUBLIC_URL")
		if _, err := ciStartupConfigFromEnv(ciEnvMap(env)); err == nil {
			t.Fatal("run mode requires public url")
		}
		// Off mode with cache root still requires it.
		env2 := map[string]string{
			"CANTER_CI_CACHE_ROOT": dir,
		}
		if _, err := ciStartupConfigFromEnv(ciEnvMap(env2)); err == nil {
			t.Fatal("cache root requires public url")
		}
		// Non-HTTPS rejected.
		env3 := map[string]string{
			"CANTER_CI_CACHE_ROOT": dir, "CANTER_CI_PUBLIC_URL": "http://ci.example.test",
		}
		if _, err := ciStartupConfigFromEnv(ciEnvMap(env3)); err == nil {
			t.Fatal("http public url rejected")
		}
	})

	t.Run("cache root independent of mode", func(t *testing.T) {
		dir := t.TempDir()
		cacheDir := filepath.Join(dir, "cache")
		if err := os.Mkdir(cacheDir, 0o700); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{
			"CANTER_CI_CACHE_ROOT": cacheDir,
			"CANTER_CI_PUBLIC_URL": "https://ci.example.test",
		}
		cfg, err := ciStartupConfigFromEnv(ciEnvMap(env))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.CacheRoot != cacheDir || cfg.PublicURL != "https://ci.example.test" || cfg.Managed.Mode != "" {
			t.Fatalf("unexpected config: %+v", cfg)
		}
		// Relative cache root rejected.
		env["CANTER_CI_CACHE_ROOT"] = "relative/cache"
		if _, err := ciStartupConfigFromEnv(ciEnvMap(env)); err == nil {
			t.Fatal("relative cache root must be rejected")
		}
	})

	t.Run("errors never echo secrets", func(t *testing.T) {
		dir := t.TempDir()
		env := ciRunEnv(t, dir)
		secret := "SUPER-SECRET-KEY-MATERIAL-0123456789"
		env["CANTER_CI_ADMISSION_KEY_FILE"] = ciWriteSecret(t, dir, "key3.bin", []byte(secret+strings.Repeat("x", 10)), 0o600)
		_, err := ciStartupConfigFromEnv(ciEnvMap(env))
		if err == nil {
			t.Fatal("expected rejection")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked file content: %v", err)
		}
	})
}

func TestCIStartupEnforcerLoopbackHTTP(t *testing.T) {
	for value, want := range map[string]bool{"": false, "false": false, "true": true} {
		env := ciRunEnv(t, t.TempDir())
		env["CANTER_CI_ENFORCER_LOOPBACK_HTTP"] = value
		cfg, err := ciStartupConfigFromEnv(ciEnvMap(env))
		if err != nil || cfg.Managed.EnforcerLoopbackHTTP != want {
			t.Fatalf("%q: loopback=%v err=%v", value, cfg.Managed.EnforcerLoopbackHTTP, err)
		}
	}
	env := ciRunEnv(t, t.TempDir())
	env["CANTER_CI_ENFORCER_LOOPBACK_HTTP"] = "yes"
	if _, err := ciStartupConfigFromEnv(ciEnvMap(env)); err == nil || !strings.Contains(err.Error(), "CANTER_CI_ENFORCER_LOOPBACK_HTTP") {
		t.Fatalf("invalid value must be rejected by name: %v", err)
	}
}
