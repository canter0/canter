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
	go func() {
		if err := dispatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("execution dispatcher stopped: %v", err)
			stop()
		}
	}()
	var nodeBinary []byte
	if nodePath := os.Getenv("CANTER_NODE_BINARY_PATH"); nodePath != "" {
		nodeBinary, err = os.ReadFile(nodePath)
		if err != nil {
			log.Fatalf("read CANTER_NODE_BINARY_PATH: %v", err)
		}
	}
	initialDispatcher := &controlplane.InitialDeploymentDispatcher{Store: store, Service: service, Engine: client, NodeBinary: nodeBinary, WorkerID: "control-plane/initial/" + workerID}
	go func() {
		if err := initialDispatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("initial deployment dispatcher stopped: %v", err)
			stop()
		}
	}()
	addr := os.Getenv("CANTER_CONTROLPLANE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8081"
	}
	publicURL := os.Getenv("CANTER_PUBLIC_URL")
	if publicURL == "" {
		publicURL = "http://localhost:3001"
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
	handler := controlplane.NewHTTPServer(service, controlplane.HTTPConfig{PublicURL: publicURL, CookieSecure: cookieSecure, RequireInvite: strings.EqualFold(os.Getenv("CANTER_REQUIRE_INVITE"), "true")})
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
