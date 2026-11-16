package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/canter0/canter/internal/controlplane"
	"github.com/canter0/canter/internal/provider/compute"
)

// canter-ci-enforcer is the independent CI lease watchdog. It runs in its own
// failure domain with a private journal; it is never started by the control
// plane and performs no database queries.
func main() {
	stateDir := flag.String("state-dir", "", "private enforcer state directory (required)")
	listen := flag.String("listen", "127.0.0.1:8097", "listen address")
	tokenFile := flag.String("token-file", "", "operator bearer token file (required, >=32 bytes)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (required for non-loopback listen)")
	tlsKey := flag.String("tls-key", "", "TLS key file (required for non-loopback listen)")
	githubAppID := flag.Int64("github-app-id", 0, "GitHub App ID for runner management (optional)")
	githubKeyFile := flag.String("github-key-file", "", "GitHub App RSA private key file (optional, paired with --github-app-id)")
	flag.Parse()

	if *stateDir == "" || *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "canter-ci-enforcer: --state-dir and --token-file are required")
		os.Exit(2)
	}
	token, err := readPrivateFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "canter-ci-enforcer: cannot read token file")
		os.Exit(1)
	}
	token = []byte(strings.TrimSpace(string(token)))
	if len(token) < 32 {
		fmt.Fprintln(os.Stderr, "canter-ci-enforcer: operator token must be at least 32 bytes")
		os.Exit(1)
	}

	host, _, err := net.SplitHostPort(*listen)
	if err != nil || host == "" {
		fmt.Fprintln(os.Stderr, "canter-ci-enforcer: invalid --listen address")
		os.Exit(2)
	}
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	// TLS flags are honored on loopback too; non-loopback listen REQUIRES a
	// paired certificate and key.
	if (*tlsCert == "") != (*tlsKey == "") {
		fmt.Fprintln(os.Stderr, "canter-ci-enforcer: --tls-cert and --tls-key must be paired")
		os.Exit(2)
	}
	var tlsConfig *tls.Config
	if *tlsCert != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "canter-ci-enforcer: cannot load TLS key pair")
			os.Exit(1)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	} else if !loopback {
		fmt.Fprintln(os.Stderr, "canter-ci-enforcer: non-loopback listen requires --tls-cert and --tls-key")
		os.Exit(2)
	}

	computeClient, err := compute.NewFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "canter-ci-enforcer: compute credentials are incomplete")
		os.Exit(1)
	}
	if region := strings.TrimSpace(os.Getenv("CANTER_CI_COMPUTE_REGION")); region != "" {
		computeClient = computeClient.WithRegion(region)
	}

	var github controlplane.CIConfig
	if *githubAppID > 0 || *githubKeyFile != "" {
		if *githubAppID <= 0 || *githubKeyFile == "" {
			fmt.Fprintln(os.Stderr, "canter-ci-enforcer: --github-app-id and --github-key-file must be paired")
			os.Exit(2)
		}
		key, err := readPrivateFile(*githubKeyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "canter-ci-enforcer: cannot read GitHub key file")
			os.Exit(1)
		}
		github = controlplane.CIConfig{AppID: *githubAppID, PrivateKeyPEM: key}
	}

	server, err := controlplane.NewCIEnforcerServer(controlplane.CIEnforcerServerConfig{
		StateDir: *stateDir, Token: token, Compute: computeClient, GitHub: github,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "canter-ci-enforcer: "+err.Error())
		os.Exit(1)
	}
	defer server.Close()

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      65 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		TLSConfig:         tlsConfig,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go server.Run(ctx)

	errCh := make(chan error, 1)
	go func() {
		if tlsConfig != nil {
			errCh <- httpServer.ListenAndServeTLS("", "")
		} else {
			errCh <- httpServer.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "canter-ci-enforcer: "+err.Error())
			os.Exit(1)
		}
	}
}

// readPrivateFile reads a regular, non-symlink file bounded to 64 KiB for
// operator secrets through an O_NOFOLLOW descriptor with Fstat validation:
// regular file, mode 0600, owned by euid, single link. Anything else is
// refused rather than reading ambient files.
func readPrivateFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 64<<10 {
		return nil, fmt.Errorf("secret file must be a private regular file of at most 64KiB")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return nil, fmt.Errorf("secret file has wrong owner or link count")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 64<<10 {
		return nil, fmt.Errorf("secret file exceeds bound")
	}
	return raw, nil
}
