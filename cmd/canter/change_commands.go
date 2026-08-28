package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
)

type repeatedFlag []string

func (r *repeatedFlag) String() string         { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(value string) error { *r = append(*r, value); return nil }

func changeCommand(client *sdk.Client, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: canter change <draft|inspect|authorize|apply>")
	}
	switch args[0] {
	case "draft":
		fs := flag.NewFlagSet("change draft", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		artifact := fs.String("artifact", "", "release .tar.gz path")
		command := fs.String("command", "./app", "release command")
		health := fs.String("health", "/health", "release health path")
		port := fs.Int("port", 8080, "public HTTP port")
		summary := fs.String("summary", "", "human-readable production outcome")
		migration := fs.String("migration", "", "expand-only SQL migration path")
		migrationID := fs.String("migration-id", "", "stable migration identifier")
		database := fs.String("database", "database", "database service name")
		verify := fs.String("verify", "/health", "verification path")
		contains := fs.String("contains", "", "required verification response substring")
		var environments repeatedFlag
		fs.Var(&environments, "env", "release environment KEY=VALUE; repeatable")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *artifact == "" || *summary == "" {
			return errors.New("change draft requires --artifact and --summary")
		}
		if (*migration == "") != (*migrationID == "") {
			return errors.New("--migration and --migration-id must be supplied together")
		}
		environment := make(map[string]string, len(environments))
		for _, assignment := range environments {
			key, value, ok := strings.Cut(assignment, "=")
			if !ok || key == "" {
				return fmt.Errorf("invalid environment assignment %q", assignment)
			}
			environment[key] = value
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		change, err := client.DraftChange(ctx, system, sdk.DraftChangeInput{
			Summary:       *summary,
			Release:       sdk.PublishReleaseInput{ArtifactPath: *artifact, Command: strings.Fields(*command), Environment: environment, HealthPath: *health, PublicPort: *port},
			MigrationPath: *migration, MigrationID: *migrationID, Database: *database,
			Verification: sdk.ChangeVerification{Method: "GET", Path: *verify, ExpectedStatus: 200, BodyContains: *contains},
		})
		if err != nil {
			return err
		}
		return printJSON(change)
	case "inspect":
		fs := flag.NewFlagSet("change inspect", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		id := fs.String("id", "", "change id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("change inspect requires --id")
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		change, err := client.InspectChange(ctx, system, *id)
		if err != nil {
			return err
		}
		return printJSON(change)
	case "authorize":
		fs := flag.NewFlagSet("change authorize", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		id := fs.String("id", "", "change id")
		digest := fs.String("digest", "", "exact reviewed plan digest")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" || *digest == "" {
			return errors.New("change authorize requires --id and --digest")
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		change, err := client.AuthorizeChange(ctx, system, *id, *digest)
		if err != nil {
			return err
		}
		return printJSON(change)
	case "apply":
		fs := flag.NewFlagSet("change apply", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		id := fs.String("id", "", "change id")
		timeout := fs.Duration("timeout", 3*time.Minute, "change execution deadline")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("change apply requires --id")
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		change, applyErr := client.ApplyChange(ctx, system, *id)
		if printErr := printJSON(change); printErr != nil {
			return printErr
		}
		return applyErr
	default:
		return fmt.Errorf("unknown change command %q", args[0])
	}
}
