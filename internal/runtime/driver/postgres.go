package driver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func (p Postgres) Ensure(ctx context.Context, service sdk.RuntimeService) (Result, error) {
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

func (p Postgres) install(ctx context.Context) error {
	if _, err := exec.LookPath("psql"); err == nil {
		return nil
	}
	if err := run(ctx, "apt-get", "update", "-qq"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "apt-get", "install", "-y", "-qq", "postgresql")
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install postgres: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (p Postgres) credentials(service string) (postgresCredentials, error) {
	root := p.Root
	if root == "" {
		root = "/var/lib/canter-node/services"
	}
	directory := filepath.Join(root, service)
	path := filepath.Join(directory, "credentials.json")
	if payload, err := os.ReadFile(path); err == nil {
		var credentials postgresCredentials
		if err := json.Unmarshal(payload, &credentials); err != nil {
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
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return postgresCredentials{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return postgresCredentials{}, err
	}
	return credentials, nil
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	payload, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(payload)))
	}
	return string(payload), nil
}
