package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
)

func hostCommand(client *sdk.Client, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: canter host <bootstrap|expose|status|destroy>")
	}
	switch args[0] {
	case "bootstrap":
		fs := flag.NewFlagSet("host bootstrap", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		nodePath := fs.String("node", "bin/canter-node-linux-amd64", "Linux node runtime binary")
		timeout := fs.Duration("timeout", 5*time.Minute, "host bootstrap deadline")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		nodeBinary, err := os.ReadFile(*nodePath)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		result, err := client.BootstrapSystemHost(ctx, system, nodeBinary)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "expose":
		fs := flag.NewFlagSet("host expose", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		state, err := client.ExposeSystemHost(ctx, system)
		if err != nil {
			return err
		}
		return printJSON(state)
	case "status":
		fs := flag.NewFlagSet("host status", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		state, err := client.SystemHostStatus(ctx, system)
		if err != nil {
			return err
		}
		return printJSON(state)
	case "destroy":
		fs := flag.NewFlagSet("host destroy", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		yes := fs.Bool("yes", false, "confirm host destruction")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !*yes {
			return errors.New("host destroy requires --yes")
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		state, err := client.DestroySystemHost(ctx, system)
		if err != nil {
			return err
		}
		return printJSON(state)
	default:
		return fmt.Errorf("unknown host command %q", args[0])
	}
}

func releaseCommand(client *sdk.Client, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: canter release <publish|status|rollback|restart>")
	}
	switch args[0] {
	case "publish":
		fs := flag.NewFlagSet("release publish", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		artifact := fs.String("artifact", "", "release .tar.gz path")
		command := fs.String("command", "./app", "release command")
		health := fs.String("health", "/health", "HTTP health path")
		port := fs.Int("port", 8080, "public HTTP port")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *artifact == "" {
			return errors.New("release publish requires --artifact")
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		manifest, err := client.PublishRelease(ctx, system, sdk.PublishReleaseInput{ArtifactPath: *artifact, Command: strings.Fields(*command), HealthPath: *health, PublicPort: *port})
		if err != nil {
			return err
		}
		return printJSON(manifest)
	case "status":
		fs := flag.NewFlagSet("release status", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		status, err := client.ReleaseStatus(ctx, system)
		if err != nil {
			return err
		}
		return printJSON(status)
	case "rollback":
		fs := flag.NewFlagSet("release rollback", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		to := fs.String("to", "", "release version")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		manifest, err := client.RollbackRelease(ctx, system, *to)
		if err != nil {
			return err
		}
		return printJSON(manifest)
	case "restart":
		fs := flag.NewFlagSet("release restart", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		control, err := client.RestartRelease(ctx, system)
		if err != nil {
			return err
		}
		return printJSON(control)
	default:
		return fmt.Errorf("unknown release command %q", args[0])
	}
}
