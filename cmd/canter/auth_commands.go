package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/canter0/canter/sdk/remote"
)

const authUsage = `usage: canter auth <command> [flags]

Inspect the account behind a personal API key. Keys are managed in the
browser at Settings > API keys; this command never writes or stores them.

commands:
  whoami    show the account and workspaces for CANTER_API_KEY

common environment inputs (never written automatically):
  CANTER_API_URL
  CANTER_API_KEY`

func authCommand(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(authUsage)
		return nil
	}
	switch args[0] {
	case "whoami":
		return authWhoAmICommand(args[1:])
	default:
		return fmt.Errorf("unknown auth command %q", args[0])
	}
}

func authWhoAmICommand(args []string) error {
	fs := flag.NewFlagSet("auth whoami", flag.ContinueOnError)
	apiURL := fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	envPath := fs.String("env-file", "", "explicit credential file to read")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("auth whoami accepts no positional arguments")
	}
	values, err := explicitEnvValues(*envPath)
	if err != nil {
		return err
	}
	if !flagWasSet(fs, "api-url") && values["CANTER_API_URL"] != "" {
		*apiURL = values["CANTER_API_URL"]
	}
	token, err := resolvePersonalAPICredential(values)
	if err != nil {
		return err
	}
	client, err := remote.New(*apiURL, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	identity, err := client.Me(ctx, token)
	if err != nil {
		return err
	}
	return printJSON(identity)
}
