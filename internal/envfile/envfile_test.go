package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPreservesLiteralCredentialsAndExistingInterpolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.env")
	if err := os.WriteFile(path, []byte("CANTER_AUDIT_SINGLE='prefix$CANTER_AUDIT_SUFFIX'\nCANTER_AUDIT_DOUBLE=\"prefix$CANTER_AUDIT_SUFFIX\"\nCANTER_AUDIT_UNQUOTED=prefix$CANTER_AUDIT_SUFFIX\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANTER_ENV_FILE", path)
	t.Setenv("CANTER_AUDIT_SUFFIX", "secret")
	restoreEnv(t, "CANTER_AUDIT_SINGLE")
	restoreEnv(t, "CANTER_AUDIT_DOUBLE")
	restoreEnv(t, "CANTER_AUDIT_UNQUOTED")
	if err := os.Unsetenv("CANTER_AUDIT_SINGLE"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("CANTER_AUDIT_DOUBLE"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("CANTER_AUDIT_UNQUOTED"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CANTER_AUDIT_SINGLE"); got != "prefix$CANTER_AUDIT_SUFFIX" {
		t.Fatalf("single-quoted value = %q, want literal dollar expression", got)
	}
	if got := os.Getenv("CANTER_AUDIT_DOUBLE"); got != "prefixsecret" {
		t.Fatalf("double-quoted value = %q, want expanded value", got)
	}
	if got := os.Getenv("CANTER_AUDIT_UNQUOTED"); got != "prefixsecret" {
		t.Fatalf("unquoted value = %q, want existing interpolation", got)
	}
}

func TestLoadRejectsSpecialAndOversizedFiles(t *testing.T) {
	if err := loadFile(t.TempDir()); err == nil {
		t.Fatal("directory was accepted as an environment file")
	}

	path := filepath.Join(t.TempDir(), "oversized.env")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxEnvFileSize + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := loadFile(path); err == nil {
		t.Fatal("oversized environment file was accepted")
	}
}

func restoreEnv(t *testing.T, key string) {
	t.Helper()
	old, existed := os.LookupEnv(key)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
