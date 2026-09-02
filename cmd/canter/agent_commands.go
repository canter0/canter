package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/canter0/canter/sdk/remote"
)

const agentUsage = `usage: canter agent <command> [flags]

Connect disposable agent conversations to one durable Canter installation.
No command writes credentials unless --env-file is explicitly supplied.

commands:
  connect    begin device authorization, wait for approval, and exchange tokens
  begin      create a device authorization without waiting
  exchange   exchange or poll an existing device code
  refresh    rotate an installation refresh credential into a new session
  whoami     inspect the authenticated installation and current session
  bootstrap  reconstruct durable workspace state in a fresh conversation
  mcp        emit Streamable HTTP MCP connection details

common environment inputs (never written automatically):
  CANTER_API_URL
  CANTER_AGENT_ACCESS_TOKEN
  CANTER_AGENT_REFRESH_TOKEN`

func agentCommand(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(agentUsage)
		return nil
	}
	switch args[0] {
	case "connect":
		return agentConnectCommand(args[1:])
	case "begin":
		return agentBeginCommand(args[1:])
	case "exchange":
		return agentExchangeCommand(args[1:])
	case "refresh":
		return agentRefreshCommand(args[1:])
	case "whoami":
		return agentWhoAmICommand(args[1:])
	case "bootstrap":
		return agentBootstrapCommand(args[1:])
	case "mcp":
		return agentMCPCommand(args[1:])
	default:
		return fmt.Errorf("unknown agent command %q", args[0])
	}
}

func agentConnectCommand(args []string) error {
	fs := flag.NewFlagSet("agent connect", flag.ContinueOnError)
	apiURL := fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	name := fs.String("name", "Codex", "durable installation name")
	harness := fs.String("harness", "codex", "agent harness identifier")
	clientInstance := fs.String("client-instance", defaultClientInstance(), "ephemeral conversation or process identifier")
	timeout := fs.Duration("timeout", 10*time.Minute, "authorization deadline")
	envPath := fs.String("env-file", "", "explicit path for persisted credentials (mode 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := remote.New(*apiURL, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	device, err := client.BeginDeviceAuthorization(ctx, remote.BeginInput{Name: *name, Harness: *harness, Authority: remote.Authority{Inspect: true, Draft: true, ApplyMode: "human-approval-required"}})
	if err != nil {
		return err
	}
	if err := printJSON(map[string]any{"status": "waiting-for-human", "userCode": device.UserCode, "verificationUri": device.VerificationURI, "expiresAt": device.ExpiresAt}); err != nil {
		return err
	}
	pair, err := client.PollDevice(ctx, device, *clientInstance)
	if err != nil {
		return err
	}
	return finishAgentTokens(client, pair, *envPath)
}

func agentBeginCommand(args []string) error {
	fs := flag.NewFlagSet("agent begin", flag.ContinueOnError)
	apiURL := fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	name := fs.String("name", "Codex", "durable installation name")
	harness := fs.String("harness", "codex", "agent harness identifier")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := remote.New(*apiURL, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	device, err := client.BeginDeviceAuthorization(ctx, remote.BeginInput{Name: *name, Harness: *harness, Authority: remote.Authority{Inspect: true, Draft: true, ApplyMode: "human-approval-required"}})
	if err != nil {
		return err
	}
	return printJSON(device)
}

func agentExchangeCommand(args []string) error {
	fs := flag.NewFlagSet("agent exchange", flag.ContinueOnError)
	apiURL := fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	deviceCode := fs.String("device-code", "", "device code returned by agent begin")
	clientInstance := fs.String("client-instance", defaultClientInstance(), "ephemeral conversation or process identifier")
	wait := fs.Bool("wait", false, "poll until the human approves or timeout expires")
	timeout := fs.Duration("timeout", 10*time.Minute, "polling deadline")
	envPath := fs.String("env-file", "", "explicit path for persisted credentials (mode 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*deviceCode) == "" {
		return errors.New("agent exchange requires --device-code")
	}
	client, err := remote.New(*apiURL, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var pair remote.TokenPair
	if !*wait {
		pair, err = client.ExchangeDevice(ctx, *deviceCode, *clientInstance)
	} else {
		pair, err = pollDeviceCode(ctx, client, *deviceCode, *clientInstance)
	}
	if err != nil {
		return err
	}
	return finishAgentTokens(client, pair, *envPath)
}

func pollDeviceCode(ctx context.Context, client *remote.Client, deviceCode, clientInstance string) (remote.TokenPair, error) {
	for {
		pair, err := client.ExchangeDevice(ctx, deviceCode, clientInstance)
		if err == nil {
			return pair, nil
		}
		if !errors.Is(err, remote.ErrAuthorizationPending) {
			return remote.TokenPair{}, err
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return remote.TokenPair{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func agentRefreshCommand(args []string) error {
	fs := flag.NewFlagSet("agent refresh", flag.ContinueOnError)
	apiURL := fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	refreshToken := fs.String("refresh-token", os.Getenv("CANTER_AGENT_REFRESH_TOKEN"), "installation refresh token")
	clientInstance := fs.String("client-instance", defaultClientInstance(), "ephemeral conversation or process identifier")
	envPath := fs.String("env-file", "", "explicit credential file to read and update")
	if err := fs.Parse(args); err != nil {
		return err
	}
	apiURLExplicit := flagWasSet(fs, "api-url")
	values, err := explicitEnvValues(*envPath)
	if err != nil {
		return err
	}
	if *refreshToken == "" {
		*refreshToken = values["CANTER_AGENT_REFRESH_TOKEN"]
	}
	if !apiURLExplicit && values["CANTER_API_URL"] != "" {
		*apiURL = values["CANTER_API_URL"]
	}
	if *refreshToken == "" {
		return errors.New("agent refresh requires --refresh-token, CANTER_AGENT_REFRESH_TOKEN, or an explicit --env-file containing it")
	}
	client, err := remote.New(*apiURL, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pair, err := client.Refresh(ctx, *refreshToken, *clientInstance)
	if err != nil {
		return err
	}
	return finishAgentTokens(client, pair, *envPath)
}

func agentWhoAmICommand(args []string) error {
	client, accessToken, _, err := authenticatedRemote("agent whoami", args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	identity, err := client.WhoAmI(ctx, accessToken)
	if err != nil {
		return err
	}
	return printJSON(identity)
}

func agentBootstrapCommand(args []string) error {
	client, accessToken, _, err := authenticatedRemote("agent bootstrap", args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bootstrap, err := client.Bootstrap(ctx, accessToken)
	if err != nil {
		return err
	}
	return printJSON(bootstrap)
}

func agentMCPCommand(args []string) error {
	client, accessToken, envPath, err := authenticatedRemote("agent mcp", args)
	if err != nil {
		return err
	}
	connection := map[string]any{
		"transport": "streamable-http",
		"url":       client.MCPURL(),
		"headers":   map[string]string{"Authorization": "Bearer " + accessToken},
		"tools":     []string{"canter_whoami", "canter_bootstrap", "canter_list_changes", "canter_inspect_system", "canter_draft_change", "canter_inspect_change"},
		"note":      "The access token is short-lived. Run `canter agent refresh` to rotate into a new session.",
	}
	if envPath != "" {
		connection["headers"] = map[string]string{"Authorization": "Bearer ${CANTER_AGENT_ACCESS_TOKEN}"}
		connection["credentialEnvFile"] = envPath
		connection["codex"] = fmt.Sprintf("set -a; . %s; set +a; codex mcp add canter --url %s --bearer-token-env-var CANTER_AGENT_ACCESS_TOKEN", shellQuote(envPath), shellQuote(client.MCPURL()))
	}
	return printJSON(connection)
}

func authenticatedRemote(name string, args []string) (*remote.Client, string, string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	apiURL := fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	accessToken := fs.String("access-token", os.Getenv("CANTER_AGENT_ACCESS_TOKEN"), "short-lived agent access token")
	envPath := fs.String("env-file", "", "explicit credential file to read")
	if err := fs.Parse(args); err != nil {
		return nil, "", "", err
	}
	apiURLExplicit := flagWasSet(fs, "api-url")
	values, err := explicitEnvValues(*envPath)
	if err != nil {
		return nil, "", "", err
	}
	if *accessToken == "" {
		*accessToken = values["CANTER_AGENT_ACCESS_TOKEN"]
	}
	if !apiURLExplicit && values["CANTER_API_URL"] != "" {
		*apiURL = values["CANTER_API_URL"]
	}
	if *accessToken == "" {
		return nil, "", "", errors.New("command requires --access-token, CANTER_AGENT_ACCESS_TOKEN, or an explicit --env-file containing it")
	}
	client, err := remote.New(*apiURL, nil)
	return client, *accessToken, *envPath, err
}

func finishAgentTokens(client *remote.Client, pair remote.TokenPair, envPath string) error {
	if envPath == "" {
		return printJSON(map[string]any{
			"credentials":  pair,
			"mcp":          map[string]any{"transport": "streamable-http", "url": client.MCPURL(), "headers": map[string]string{"Authorization": "Bearer " + pair.AccessToken}},
			"instructions": "Store these credentials in your agent harness. Canter will not persist them unless you rerun with --env-file PATH.",
		})
	}
	if err := persistAgentEnv(envPath, map[string]string{
		"CANTER_API_URL":               client.BaseURL(),
		"CANTER_MCP_URL":               client.MCPURL(),
		"CANTER_AGENT_ACCESS_TOKEN":    pair.AccessToken,
		"CANTER_AGENT_REFRESH_TOKEN":   pair.RefreshToken,
		"CANTER_AGENT_INSTALLATION_ID": pair.Installation.ID,
	}); err != nil {
		return err
	}
	return printJSON(map[string]any{"connected": true, "installation": pair.Installation, "session": pair.Session, "credentialsWrittenTo": envPath, "mcpUrl": client.MCPURL()})
}

func defaultAPIURL() string {
	if value := strings.TrimSpace(os.Getenv("CANTER_API_URL")); value != "" {
		return value
	}
	return "http://127.0.0.1:8081"
}

func defaultClientInstance() string {
	if value := strings.TrimSpace(os.Getenv("CANTER_CLIENT_INSTANCE")); value != "" {
		return value
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return "cli/" + host
}

func explicitEnvValues(path string) (map[string]string, error) {
	values := map[string]string{}
	if path == "" {
		return values, nil
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = strings.ReplaceAll(value[1:len(value)-1], `'"'"'`, `'`)
		} else if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	// Early private-beta credential files used the shorter names. Accept them
	// as a read-only migration path so an installation can rotate into the
	// current contract without an agent having to copy secrets by hand.
	if values["CANTER_AGENT_ACCESS_TOKEN"] == "" {
		values["CANTER_AGENT_ACCESS_TOKEN"] = values["CANTER_ACCESS_TOKEN"]
	}
	if values["CANTER_AGENT_REFRESH_TOKEN"] == "" {
		values["CANTER_AGENT_REFRESH_TOKEN"] = values["CANTER_REFRESH_TOKEN"]
	}
	return values, nil
}

func persistAgentEnv(path string, values map[string]string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("credential path is required")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to write credentials through a symbolic link")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var retained []string
	if raw, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
			key, _, ok := strings.Cut(trimmed, "=")
			if ok {
				key = strings.TrimSpace(key)
				_, managed := values[key]
				legacyManaged := (key == "CANTER_ACCESS_TOKEN" && values["CANTER_AGENT_ACCESS_TOKEN"] != "") ||
					(key == "CANTER_REFRESH_TOKEN" && values["CANTER_AGENT_REFRESH_TOKEN"] != "")
				if managed || legacyManaged {
					continue
				}
			}
			retained = append(retained, line)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := append([]string(nil), retained...)
	if len(lines) > 0 && lines[len(lines)-1] != "" {
		lines = append(lines, "")
	}
	lines = append(lines, "# Canter agent credentials. Treat this file as a secret.")
	for _, key := range keys {
		lines = append(lines, key+"="+shellQuote(values[key]))
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	temporary, err := os.CreateTemp(filepath.Dir(path), ".canter-agent-env-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == name {
			set = true
		}
	})
	return set
}
