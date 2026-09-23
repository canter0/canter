package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/canter0/canter/internal/controlplane"
	"github.com/canter0/canter/internal/envfile"
	"github.com/canter0/canter/sdk"
)

func main() {
	if _, err := envfile.Load(); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databaseURL := os.Getenv("CANTER_DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("CANTER_DATABASE_URL is required")
	}
	store, err := controlplane.Open(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		log.Fatal(err)
	}
	if invite := os.Getenv("CANTER_BETA_INVITE"); invite != "" {
		if err = store.SeedInvite(ctx, invite, "environment beta invite"); err != nil {
			log.Fatal(err)
		}
	}
	client, err := sdk.NewFromEnv()
	if err != nil {
		log.Fatalf("initialize real Canter engine: %v", err)
	}
	nodeGatewayURL := os.Getenv("CANTER_NODE_GATEWAY_URL")
	service := &controlplane.Service{Store: store, Engine: client, NodeGateway: client, NodeGatewayURL: nodeGatewayURL}
	workerID, _ := os.Hostname()
	dispatcher := &controlplane.Dispatcher{Store: store, Engine: client, WorkerID: "control-plane/" + workerID}

	var nodeBinary []byte
	if nodePath := os.Getenv("CANTER_NODE_BINARY_PATH"); nodePath != "" {
		nodeBinary, err = os.ReadFile(nodePath)
		if err != nil {
			log.Fatalf("read CANTER_NODE_BINARY_PATH: %v", err)
		}
	}
	initialDispatcher := &controlplane.InitialDeploymentDispatcher{Store: store, Service: service, Engine: client, NodeBinary: nodeBinary, WorkerID: "control-plane/initial/" + workerID}

	addr := os.Getenv("CANTER_CONTROLPLANE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8081"
	}
	publicURL := os.Getenv("CANTER_PUBLIC_URL")
	if publicURL == "" {
		publicURL = "http://127.0.0.1:3000"
	}
	cookieSecure, err := cookieSecurity(publicURL, os.Getenv("CANTER_COOKIE_SECURE"))
	if err != nil {
		log.Fatal(err)
	}
	if nodeGatewayURL != "" {
		parsed, parseErr := url.Parse(nodeGatewayURL)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			log.Fatal("CANTER_NODE_GATEWAY_URL must be an absolute HTTPS URL")
		}
	}
	googleOAuth := controlplane.OAuthCredentials{ClientID: os.Getenv("CANTER_GOOGLE_CLIENT_ID"), ClientSecret: os.Getenv("CANTER_GOOGLE_CLIENT_SECRET")}
	githubOAuth := controlplane.OAuthCredentials{ClientID: os.Getenv("CANTER_GITHUB_CLIENT_ID"), ClientSecret: os.Getenv("CANTER_GITHUB_CLIENT_SECRET")}
	githubApp := controlplane.OAuthCredentials{ClientID: os.Getenv("CANTER_GITHUB_APP_CLIENT_ID"), ClientSecret: os.Getenv("CANTER_GITHUB_APP_CLIENT_SECRET")}
	for name, credentials := range map[string]controlplane.OAuthCredentials{"Google": googleOAuth, "GitHub": githubOAuth, "GitHub App": githubApp} {
		if err := controlplane.ValidateOAuthCredentials(name, credentials); err != nil {
			log.Fatal(err)
		}
	}
	billing := controlplane.NewBillingGateway(controlplane.BillingConfig{
		PortalConfigurationID: os.Getenv("CANTER_STRIPE_PORTAL_CONFIGURATION_ID"),
		Enabled:               strings.EqualFold(os.Getenv("CANTER_BILLING_ENABLED"), "true"),
		SecretKey:             os.Getenv("CANTER_STRIPE_SECRET_KEY"), WebhookSecret: os.Getenv("CANTER_STRIPE_WEBHOOK_SECRET"), IngestToken: os.Getenv("CANTER_BILLING_INGEST_TOKEN"),
		PaygPriceID: os.Getenv("CANTER_STRIPE_PAYG_PRICE_ID"), ProPriceID: os.Getenv("CANTER_STRIPE_PRO_PRICE_ID"), ProUsagePriceID: os.Getenv("CANTER_STRIPE_PRO_USAGE_PRICE_ID"),
		MeterID: os.Getenv("CANTER_STRIPE_METER_ID"), MeterEventName: os.Getenv("CANTER_STRIPE_METER_EVENT_NAME"),
	})
	store.ConfigureBilling(billing)
	if billing.Config.Enabled {
		if !billing.Ready() {
			log.Fatal("billing is enabled but its configuration is incomplete")
		}
		if err := billing.ValidatePrices(ctx); err != nil {
			log.Fatalf("validate billing catalog: %v", err)
		}
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := store.CollectUsage(ctx, client); err != nil && ctx.Err() == nil {
						log.Printf("billing resource collection: %v", err)
					}
					if err := billing.DispatchUsage(ctx, store); err != nil && ctx.Err() == nil {
						log.Printf("billing usage dispatch: %v", err)
					}
				}
			}
		}()
	}
	go func() {
		if err := dispatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("execution dispatcher stopped: %v", err)
			stop()
		}
	}()
	go func() {
		if err := initialDispatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("initial deployment dispatcher stopped: %v", err)
			stop()
		}
	}()
	operator := controlplane.OperatorConfig{APIKey: os.Getenv("OPENROUTER_API_KEY"), BaseURL: os.Getenv("CANTER_OPERATOR_BASE_URL"), Model: os.Getenv("CANTER_OPERATOR_MODEL"), TitleModel: os.Getenv("CANTER_OPERATOR_TITLE_MODEL"), StaticBinary: os.Getenv("CANTER_STATIC_SERVER_BINARY"), ShellRunner: os.Getenv("CANTER_OPERATOR_SHELL_RUNNER"), ShellNode: os.Getenv("CANTER_OPERATOR_SHELL_NODE"), ShellSocket: os.Getenv("CANTER_OPERATOR_SHELL_SOCKET"), ReasoningEffort: os.Getenv("CANTER_OPERATOR_REASONING_EFFORT")}
	operator.ExaAPIKey = os.Getenv("EXA_API_KEY")
	if operator.ShellSocket == "" && operator.ShellRunner == "" {
		if info, err := os.Stat("/run/canter-harness.sock"); err == nil && info.Mode()&os.ModeSocket != 0 {
			operator.ShellSocket = "/run/canter-harness.sock"
		}
		if operator.ShellSocket == "" && runtime.GOOS != "linux" {
			if _, err := os.Stat("harness/node_modules/just-bash/package.json"); err == nil {
				operator.ShellRunner = "harness/runner.mjs"
			}
		}
	}
	// Linux production must use the memory-capped socket service. A direct
	// runner is an explicit development option, never an ambient host shell.
	if operator.ShellRunner != "" && runtime.GOOS == "linux" && operator.ShellSocket == "" {
		log.Fatal("use CANTER_OPERATOR_SHELL_SOCKET for the memory-capped Linux command service")
	}
	if err := operator.CheckShell(ctx); err != nil {
		log.Fatalf("workspace command environment: %v", err)
	}
	if operator.StaticBinary == "" {
		if _, err := os.Stat("bin/canter-static-linux"); err == nil {
			operator.StaticBinary = "bin/canter-static-linux"
		}
	}
	if operator.BaseURL == "" {
		operator.BaseURL = "https://openrouter.ai/api/v1"
	}
	if operator.Model == "" {
		operator.Model = "openai/gpt-5.6-luna"
	}
	var vault *controlplane.SecretVault
	if keyFile := os.Getenv("CANTER_SECRETS_KEY_FILE"); keyFile != "" {
		info, statErr := os.Stat(keyFile)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			log.Fatal("CANTER_SECRETS_KEY_FILE must be a private regular file (0600)")
		}
		raw, readErr := os.ReadFile(keyFile)
		if readErr != nil {
			log.Fatal("cannot read secrets keyring")
		}
		vault, err = controlplane.NewSecretVault(raw)
		clear(raw)
		if err != nil {
			log.Fatal(err)
		}
	}
	handler := controlplane.NewHTTPServer(service, controlplane.HTTPConfig{PublicURL: publicURL, CookieSecure: cookieSecure, RequireInvite: strings.EqualFold(os.Getenv("CANTER_REQUIRE_INVITE"), "true"), GoogleOAuth: googleOAuth, GitHubOAuth: githubOAuth, GitHubApp: githubApp, GitHubAppSlug: os.Getenv("CANTER_GITHUB_APP_SLUG"), Billing: billing, Operator: operator, Secrets: vault})
	if operator.Ready() || vault != nil {
		for i := 0; i < 2; i++ {
			go func() {
				if err := (&controlplane.OperatorRuntime{Server: handler.(*controlplane.HTTPServer), Config: operator}).Run(ctx); err != nil && ctx.Err() == nil {
					log.Printf("workspace agent dispatcher stopped: %v", err)
					stop()
				}
			}()
		}
		log.Printf("Canter workspace agent enabled with model %s", operator.Model)
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("Canter control plane listening on %s", addr)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func cookieSecurity(publicURL, configured string) (bool, error) {
	parsed, err := url.Parse(publicURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false, fmt.Errorf("CANTER_PUBLIC_URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme == "https" {
		// HTTPS is the source of truth. A forgotten or stale environment flag must
		// never downgrade a production session cookie.
		return true, nil
	}
	return strings.EqualFold(configured, "true"), nil
}
