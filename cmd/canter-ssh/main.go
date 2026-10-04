// canter-ssh serves Canter's public, read-only terminal experience.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"charm.land/ssh"
	"github.com/canter0/canter/internal/sshsite"
)

func main() {
	address := flag.String("listen", "127.0.0.1:23234", "SSH listen address")
	hostKey := flag.String("host-key", ".tmp-ops/ssh-site/host_ed25519", "persistent SSH host key path")
	flag.Parse()
	server, err := sshsite.NewServer(*address, *hostKey)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	fmt.Printf("Canter terminal listening on %s\n", *address)
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}
}
