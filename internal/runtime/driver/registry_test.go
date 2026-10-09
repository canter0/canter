package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

type fakeDriver struct{}

func (fakeDriver) Ensure(context.Context, sdk.RuntimeService) (Result, error) {
	return Result{URL: "scheme://private", Endpoint: "private:1234"}, nil
}
func (fakeDriver) Execute(_ context.Context, _ sdk.RuntimeService, action sdk.RuntimeAction) (sdk.RuntimeActionResult, error) {
	return sdk.RuntimeActionResult{ID: action.ID, Phase: "completed"}, nil
}

func TestRegistryCreatesGenericServiceBinding(t *testing.T) {
	registry := NewRegistry()
	registry.Register("database", "example", fakeDriver{})
	bindings, observed, err := registry.Ensure(context.Background(), sdk.RuntimePlan{Services: []sdk.RuntimeService{{Name: "primary-data", Binding: "CANTER_SERVICE_PRIMARY_DATA_URL", Kind: "database", Engine: "example"}}})
	if err != nil {
		t.Fatal(err)
	}
	if bindings["CANTER_SERVICE_PRIMARY_DATA_URL"] != "scheme://private" {
		t.Fatalf("bindings=%v", bindings)
	}
	if len(observed) != 1 || observed[0].Binding != "CANTER_SERVICE_PRIMARY_DATA_URL" || observed[0].Endpoint != "private:1234" || observed[0].Phase != "ready" {
		t.Fatalf("observed=%+v", observed)
	}
}

func TestRegistryRoutesTypedActionToCapabilityDriver(t *testing.T) {
	registry := NewRegistry()
	registry.Register("database", "example", fakeDriver{})
	plan := sdk.RuntimePlan{Services: []sdk.RuntimeService{{Name: "data", Binding: "CANTER_SERVICE_DATA_URL", Kind: "database", Engine: "example"}}}
	result, err := registry.Execute(context.Background(), plan, sdk.RuntimeAction{ID: "action", Service: "data", Kind: "database.expand-migration"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "action" || result.Phase != "completed" {
		t.Fatalf("result=%+v", result)
	}
}

func TestPostgresCredentialsAreStableAndPrivate(t *testing.T) {
	driver := Postgres{Root: t.TempDir()}
	first, err := driver.credentials("database")
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.credentials("database")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Password == "" {
		t.Fatalf("credentials did not persist: first=%+v second=%+v", first, second)
	}
}

func TestPostgresConcurrentCredentialInitializationReturnsPublishedSecret(t *testing.T) {
	root := t.TempDir()
	driver := Postgres{Root: root}
	const workers = 24
	start := make(chan struct{})
	results := make(chan postgresCredentials, workers)
	errors := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			credentials, err := driver.credentials("database")
			if err != nil {
				errors <- err
				return
			}
			results <- credentials
		}()
	}
	close(start)
	var first postgresCredentials
	for range workers {
		select {
		case err := <-errors:
			t.Fatal(err)
		case credentials := <-results:
			if first.Password == "" {
				first = credentials
			} else if credentials != first {
				t.Fatalf("concurrent credential initialization returned different secrets: %q and %q", first.Password, credentials.Password)
			}
		}
	}
	persisted, err := driver.credentials("database")
	if err != nil {
		t.Fatal(err)
	}
	if persisted != first {
		t.Fatalf("returned credentials differ from published credentials: returned=%+v persisted=%+v", first, persisted)
	}
}

func TestPostgresCredentialsRejectBroadPermissions(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "database")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"user":"canter_database","password":"` + strings.Repeat("a", 48) + `","database":"canter_database"}`)
	path := filepath.Join(directory, "credentials.json")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Postgres{Root: root}).credentials("database"); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("broadly accessible credentials file error = %v", err)
	}
}

func TestPostgresCredentialsRejectUnsafeNamesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	driver := Postgres{Root: root}
	if _, err := driver.credentials("../outside"); err == nil {
		t.Fatal("unsafe service name was accepted")
	}
	directory := filepath.Join(root, "database")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "credentials.json.tmp")); err != nil {
		t.Fatal(err)
	}
	credentials, err := driver.credentials("database")
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Password == "" {
		t.Fatal("credentials were not created")
	}
	if contents, err := os.ReadFile(target); err != nil || string(contents) != "preserve me" {
		t.Fatalf("predictable temporary-file target changed: contents=%q err=%v", contents, err)
	}
	if err := os.Remove(filepath.Join(directory, "credentials.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "credentials.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.credentials("database"); err == nil {
		t.Fatal("credentials symlink was followed")
	}
}

func TestPostgresCredentialsRejectFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "database")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "credentials.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := (Postgres{Root: root}).credentials("database")
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("FIFO credentials error = %v, want regular file rejection", err)
		}
	case <-time.After(time.Second):
		t.Fatal("opening FIFO credentials blocked")
	}
}

func TestPostgresCredentialsRejectPersistedSQLFragments(t *testing.T) {
	driver := Postgres{Root: t.TempDir()}
	directory := filepath.Join(driver.Root, "database")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"user":"x'; DROP DATABASE postgres;--","password":"secret","database":"db"}`)
	if err := os.WriteFile(filepath.Join(directory, "credentials.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.credentials("database"); err == nil {
		t.Fatal("unsafe persisted credentials were accepted")
	}
}

func TestPostgresMigrationRejectsSQLInterpolatedIdentifiers(t *testing.T) {
	driver := Postgres{Root: t.TempDir()}
	sql := "CREATE TABLE safe (id integer);"
	digest := sha256.Sum256([]byte(sql))
	_, err := driver.Execute(context.Background(), sdk.RuntimeService{Name: "database"}, sdk.RuntimeAction{
		Kind: "database.expand-migration",
		Parameters: map[string]string{
			"migrationId": "x'; DROP TABLE canter_schema_migrations;--",
			"digest":      hex.EncodeToString(digest[:]),
			"sql":         sql,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid id or digest") {
		t.Fatalf("unsafe migration identifier was not rejected: %v", err)
	}
}

func TestPostgresMigrationRejectsNonCanonicalDigest(t *testing.T) {
	driver := Postgres{Root: t.TempDir()}
	sql := "CREATE TABLE safe (id integer);"
	digest := sha256.Sum256([]byte(sql))
	_, err := driver.Execute(context.Background(), sdk.RuntimeService{Name: "database"}, sdk.RuntimeAction{
		Kind: "database.expand-migration",
		Parameters: map[string]string{
			"migrationId": "initial-schema",
			"digest":      strings.ToUpper(hex.EncodeToString(digest[:])),
			"sql":         sql,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid id or digest") {
		t.Fatalf("noncanonical digest error = %v", err)
	}
}

func TestMigrationCommandErrorOutputIsBounded(t *testing.T) {
	err := runInputWithEnv(context.Background(), os.Environ(), "", "/bin/sh", "-c", "head -c 1048576 /dev/zero >&2; exit 1")
	if err == nil {
		t.Fatal("failing migration command was accepted")
	}
	if len(err.Error()) > maxCommandErrorOutput+128 {
		t.Fatalf("command error retained unbounded output: %d bytes", len(err.Error()))
	}
}

func TestGeneralCommandOutputIsBounded(t *testing.T) {
	err := run(context.Background(), "/bin/sh", "-c", "head -c 1048576 /dev/zero >&2; exit 1")
	if err == nil {
		t.Fatal("failing command was accepted")
	}
	if len(err.Error()) > maxCommandErrorOutput+128 {
		t.Fatalf("command error retained unbounded output: %d bytes", len(err.Error()))
	}
}

func TestOutputKeepsSuccessfulStdoutSeparateFromWarnings(t *testing.T) {
	script := filepath.Join(t.TempDir(), "command")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'role warning\\n' >&2\nprintf '1\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := output(t.Context(), script)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1\n" {
		t.Fatalf("successful command output = %q, want stdout token only", got)
	}
}

func TestPostgresEnsureRejectsInvalidServiceBeforeInstalling(t *testing.T) {
	t.Setenv("PATH", "")
	_, err := (Postgres{Root: t.TempDir()}).Ensure(t.Context(), sdk.RuntimeService{Name: "../unsafe", Instances: 1})
	if err == nil || !strings.Contains(err.Error(), "invalid service name") {
		t.Fatalf("invalid service error = %v", err)
	}
}
