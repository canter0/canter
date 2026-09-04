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

const changeUsage = "usage: canter change <init|schema|validate|draft|inspect|authorize|apply>"

func (r *repeatedFlag) String() string         { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(value string) error { *r = append(*r, value); return nil }

func changeCommand(client *sdk.Client, args []string) error {
	if len(args) == 0 {
		return errors.New(changeUsage)
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("change init", flag.ContinueOnError)
		file := fs.String("file", "change.yaml", "path to write")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return writeExclusive(*file, []byte(sdk.StarterChangeYAML))
	case "schema":
		if len(args) != 1 {
			return errors.New("change schema accepts no arguments")
		}
		fmt.Println(string(sdk.ChangeRequestSchemaJSON()))
		return nil
	case "validate":
		fs := flag.NewFlagSet("change validate", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		requestPath := fs.String("request", "change.yaml", "declarative Change request path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		system, err := sdk.LoadSystem(*file)
		if err != nil {
			return err
		}
		request, err := sdk.LoadChangeRequest(*requestPath)
		if err != nil {
			return err
		}
		if request.Spec.System != system.Metadata.Name {
			return fmt.Errorf("change request targets system %q, not %q", request.Spec.System, system.Metadata.Name)
		}
		if request.Spec.Scale != nil {
			_, maximum, err := sdk.ScaleCapacity(system, request.Spec.Scale.Service)
			if err != nil {
				return err
			}
			if request.Spec.Scale.Replicas > maximum {
				return fmt.Errorf("requested %d replicas exceed existing host capacity %d", request.Spec.Scale.Replicas, maximum)
			}
			return printJSON(map[string]any{"request": request, "capacityMode": "existing-host", "maximumReplicas": maximum})
		}
		input, err := request.DraftInput(system)
		if err != nil {
			return err
		}
		return printJSON(input)
	case "draft":
		fs := flag.NewFlagSet("change draft", flag.ContinueOnError)
		file := fs.String("file", "system.yaml", "system contract path")
		requestPath := fs.String("request", "", "declarative Change request path")
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
		var inlineFlags []string
		fs.Visit(func(option *flag.Flag) {
			if option.Name != "file" && option.Name != "request" {
				inlineFlags = append(inlineFlags, option.Name)
			}
		})
		if *requestPath != "" && len(inlineFlags) > 0 {
			return errors.New("change draft --request cannot be combined with inline release, migration, or environment flags")
		}
		if *requestPath == "" && (*artifact == "" || *summary == "") {
			return errors.New("change draft requires --request or both --artifact and --summary")
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
		if *requestPath != "" {
			request, err := sdk.LoadChangeRequest(*requestPath)
			if err != nil {
				return err
			}
			change, err := client.DraftChangeRequest(ctx, system, request)
			if err != nil {
				return err
			}
			return printJSON(change)
		}
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
