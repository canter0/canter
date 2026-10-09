package driver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/canter0/canter/sdk"
)

type Postgres struct {
	Root string
}

type postgresCredentials struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
}

var postgresIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var migrationIdentifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

func (p Postgres) Ensure(ctx context.Context, service sdk.RuntimeService) (Result, error) {
	if !migrationIdentifier.MatchString(service.Name) {
		return Result{}, fmt.Errorf("invalid service name %q", service.Name)
	}
	if service.Instances != 1 {
		return Result{}, fmt.Errorf("postgres driver currently supports one instance")
	}
	if err := p.install(ctx); err != nil {
		return Result{}, err
	}
	credentials, err := p.credentials(service.Name)
	if err != nil {
		return Result{}, err
	}
	if err := run(ctx, "systemctl", "enable", "--now", "postgresql"); err != nil {
		return Result{}, err
	}
	roleExists, err := output(ctx, "runuser", "-u", "postgres", "--", "psql", "-tAc", "SELECT 1 FROM pg_roles WHERE rolname='"+credentials.User+"'")
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(roleExists) != "1" {
		statement := fmt.Sprintf(`CREATE ROLE "%s" LOGIN PASSWORD '%s'`, credentials.User, credentials.Password)
		if err := run(ctx, "runuser", "-u", "postgres", "--", "psql", "-v", "ON_ERROR_STOP=1", "-c", statement); err != nil {
			return Result{}, err
		}
	}
	databaseExists, err := output(ctx, "runuser", "-u", "postgres", "--", "psql", "-tAc", "SELECT 1 FROM pg_database WHERE datname='"+credentials.Database+"'")
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(databaseExists) != "1" {
		if err := run(ctx, "runuser", "-u", "postgres", "--", "createdb", "--owner", credentials.User, credentials.Database); err != nil {
			return Result{}, err
		}
	}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		if run(deadline, "pg_isready", "-q", "-h", "127.0.0.1", "-p", "5432", "-d", credentials.Database) == nil {
			break
		}
		select {
		case <-deadline.Done():
			return Result{}, fmt.Errorf("postgres did not become ready: %w", deadline.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	connection := &url.URL{Scheme: "postgres", User: url.UserPassword(credentials.User, credentials.Password), Host: "127.0.0.1:5432", Path: "/" + credentials.Database}
	query := connection.Query()
	query.Set("sslmode", "disable")
	connection.RawQuery = query.Encode()
	return Result{URL: connection.String(), Endpoint: "127.0.0.1:5432"}, nil
}

func (p Postgres) Execute(ctx context.Context, service sdk.RuntimeService, action sdk.RuntimeAction) (sdk.RuntimeActionResult, error) {
	result := sdk.RuntimeActionResult{SchemaVersion: "v1", ID: action.ID, System: action.System, Service: service.Name, Kind: action.Kind}
	if action.Kind != "database.expand-migration" {
		return result, fmt.Errorf("postgres driver does not support action %q", action.Kind)
	}
	migrationID := action.Parameters["migrationId"]
	digest := action.Parameters["digest"]
	sql := action.Parameters["sql"]
	if migrationID == "" || digest == "" || sql == "" {
		return result, fmt.Errorf("migration action is incomplete")
	}
	if !migrationIdentifier.MatchString(migrationID) || len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return result, fmt.Errorf("migration action has an invalid id or digest")
	}
	decodedDigest, err := hex.DecodeString(digest)
	if err != nil {
		return result, fmt.Errorf("migration action has an invalid digest")
	}
	sum := sha256.Sum256([]byte(sql))
	if !bytes.Equal(decodedDigest, sum[:]) {
		return result, fmt.Errorf("migration SQL does not match its digest")
	}
	credentials, err := p.credentials(service.Name)
	if err != nil {
		return result, err
	}
	environment := append(os.Environ(), "PGPASSWORD="+credentials.Password)
	queryArgs := []string{"-h", "127.0.0.1", "-U", credentials.User, "-d", credentials.Database, "-tAc"}
	if err := runWithEnv(ctx, environment, "psql", append(queryArgs, `CREATE TABLE IF NOT EXISTS canter_schema_migrations (id TEXT PRIMARY KEY, digest TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`)...); err != nil {
		return result, err
	}
	existing, err := outputWithEnv(ctx, environment, "psql", append(queryArgs, "SELECT digest FROM canter_schema_migrations WHERE id='"+migrationID+"'")...)
	if err != nil {
		return result, err
	}
	if existing = strings.TrimSpace(existing); existing != "" {
		if existing != digest {
			return result, fmt.Errorf("migration %s was already applied with a different digest", migrationID)
		}
		result.Phase = "completed"
		result.Duplicate = true
		result.Message = "migration was already applied"
		result.CompletedAt = time.Now().UTC()
		return result, nil
	}
	transaction := "BEGIN;\nSELECT pg_advisory_xact_lock(1128353364);\n" + sql + "\nINSERT INTO canter_schema_migrations(id,digest) VALUES('" + migrationID + "','" + digest + "');\nCOMMIT;\n"
	if err := runInputWithEnv(ctx, environment, transaction, "psql", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1", "-U", credentials.User, "-d", credentials.Database); err != nil {
		return result, err
	}
	result.Phase = "completed"
	result.Message = "expand-only migration committed"
	result.CompletedAt = time.Now().UTC()
	return result, nil
}

func (p Postgres) install(ctx context.Context) error {
	if _, err := exec.LookPath("psql"); err == nil {
		return nil
	}
	if err := run(ctx, "apt-get", "update", "-qq"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "apt-get", "install", "-y", "-qq", "postgresql")
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if output, err := combinedOutput(cmd); err != nil {
		return fmt.Errorf("install postgres: %w: %s", err, output)
	}
	return nil
}

func (p Postgres) credentials(service string) (postgresCredentials, error) {
	if !migrationIdentifier.MatchString(service) {
		return postgresCredentials{}, fmt.Errorf("invalid service name %q", service)
	}
	root := p.Root
	if root == "" {
		root = "/var/lib/canter-node/services"
	}
	directory := filepath.Join(root, service)
	path := filepath.Join(directory, "credentials.json")
	if file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0); err == nil {
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return postgresCredentials{}, statErr
		}
		if !info.Mode().IsRegular() {
			_ = file.Close()
			return postgresCredentials{}, fmt.Errorf("credentials file is not a regular file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			_ = file.Close()
			return postgresCredentials{}, fmt.Errorf("credentials file permissions are too broad")
		}
		payload, readErr := io.ReadAll(io.LimitReader(file, 4097))
		closeErr := file.Close()
		if readErr != nil {
			return postgresCredentials{}, readErr
		}
		if closeErr != nil {
			return postgresCredentials{}, closeErr
		}
		if len(payload) > 4096 {
			return postgresCredentials{}, fmt.Errorf("credentials file is too large")
		}
		var credentials postgresCredentials
		if err := json.Unmarshal(payload, &credentials); err != nil {
			return postgresCredentials{}, err
		}
		if err := validatePostgresCredentials(service, credentials); err != nil {
			return postgresCredentials{}, err
		}
		return credentials, nil
	} else if !os.IsNotExist(err) {
		return postgresCredentials{}, err
	}
	identifier := "canter_" + strings.ReplaceAll(service, "-", "_")
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return postgresCredentials{}, err
	}
	credentials := postgresCredentials{User: identifier, Password: hex.EncodeToString(secret), Database: identifier}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return postgresCredentials{}, err
	}
	payload, err := json.Marshal(credentials)
	if err != nil {
		return postgresCredentials{}, err
	}
	temporary, err := os.CreateTemp(directory, ".credentials-*.tmp")
	if err != nil {
		return postgresCredentials{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return postgresCredentials{}, err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return postgresCredentials{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return postgresCredentials{}, err
	}
	if err := temporary.Close(); err != nil {
		return postgresCredentials{}, err
	}
	// Publish without replacement. Concurrent first-time reconciliations must
	// all use the secret that actually won the credentials path; rename would
	// allow each caller to return a different, briefly-current password.
	if err := os.Link(temporaryPath, path); err != nil {
		if os.IsExist(err) {
			return p.credentials(service)
		}
		return postgresCredentials{}, err
	}
	if directoryFile, err := os.Open(directory); err == nil {
		syncErr := directoryFile.Sync()
		closeErr := directoryFile.Close()
		if syncErr != nil {
			return postgresCredentials{}, syncErr
		}
		if closeErr != nil {
			return postgresCredentials{}, closeErr
		}
	} else {
		return postgresCredentials{}, err
	}
	return credentials, nil
}

func validatePostgresCredentials(service string, credentials postgresCredentials) error {
	identifier := "canter_" + strings.ReplaceAll(service, "-", "_")
	if credentials.User != identifier || credentials.Database != identifier || !postgresIdentifier.MatchString(credentials.User) || !postgresIdentifier.MatchString(credentials.Database) {
		return fmt.Errorf("credentials file contains an invalid database identifier")
	}
	if len(credentials.Password) != 48 {
		return fmt.Errorf("credentials file contains an invalid password")
	}
	if _, err := hex.DecodeString(credentials.Password); err != nil {
		return fmt.Errorf("credentials file contains an invalid password")
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if output, err := combinedOutput(cmd); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, output)
	}
	return nil
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	payload, err := commandOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, payload)
	}
	return payload, nil
}

func runWithEnv(ctx context.Context, environment []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = environment
	if output, err := combinedOutput(cmd); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, output)
	}
	return nil
}

func outputWithEnv(ctx context.Context, environment []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = environment
	payload, err := commandOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, payload)
	}
	return payload, nil
}

func runInputWithEnv(ctx context.Context, environment []string, input, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = environment
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = io.Discard
	var stderr boundedOutput
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

const maxCommandErrorOutput = 4 << 10

type boundedOutput struct {
	buffer bytes.Buffer
}

func (o *boundedOutput) Write(payload []byte) (int, error) {
	written := len(payload)
	remaining := maxCommandErrorOutput - o.buffer.Len()
	if remaining > 0 {
		if len(payload) > remaining {
			payload = payload[:remaining]
		}
		_, _ = o.buffer.Write(payload)
	}
	return written, nil
}

func (o *boundedOutput) String() string { return o.buffer.String() }

func combinedOutput(cmd *exec.Cmd) (string, error) {
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return strings.TrimSpace(output.String()), err
}

func commandOutput(cmd *exec.Cmd) (string, error) {
	var stdout, stderr boundedOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String() + "\n" + stderr.String()), err
	}
	return stdout.String(), nil
}
