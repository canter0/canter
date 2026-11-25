package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/canter0/canter/sdk/remote"
)

const ciUsage = `usage: canter ci <status|inspect> --workspace ID [--job ID] [--api-url URL] [--access-token TOKEN] [--env-file PATH]

Read-only Canter CI inspection. No command mutates CI state.

commands:
  status    workspace CI overview (pool, settings, installations, jobs, features)
  inspect   one CI job with steps, logs, metrics, and tests

common environment inputs (never written automatically):
  CANTER_API_URL
  CANTER_API_KEY                 personal API key acting as your account role
  CANTER_AGENT_ACCESS_TOKEN      agent installation token acting with granted authority`

func ciCommand(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(ciUsage)
		return nil
	}
	switch args[0] {
	case "status":
		return ciStatusCommand(args[1:])
	case "inspect":
		return ciInspectCommand(args[1:])
	default:
		return fmt.Errorf("unknown ci command %q", args[0])
	}
}

func ciStatusCommand(args []string) error {
	if ciHelpRequested(args) {
		fmt.Println(ciUsage)
		return nil
	}
	fs := flag.NewFlagSet("ci status", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace ID")
	fs.String("job", "", "unused on status")
	apiURL := fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	accessToken := fs.String("access-token", "", "short-lived agent access token")
	envPath := fs.String("env-file", "", "explicit credential file to read")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(ciUsage)
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("ci status accepts no positional arguments")
	}
	if *workspace == "" {
		return errors.New("ci status requires --workspace")
	}
	if flagWasSet(fs, "job") {
		return errors.New("--job is only valid with ci inspect")
	}
	client, token, err := ciRemote(fs, apiURL, accessToken, envPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := client.InspectCI(ctx, token, *workspace)
	if err != nil {
		return err
	}
	return printJSON(raw)
}

func ciInspectCommand(args []string) error {
	if ciHelpRequested(args) {
		fmt.Println(ciUsage)
		return nil
	}
	fs := flag.NewFlagSet("ci inspect", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace ID")
	job := fs.String("job", "", "CI job ID")
	apiURL := fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	accessToken := fs.String("access-token", "", "short-lived agent access token")
	envPath := fs.String("env-file", "", "explicit credential file to read")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(ciUsage)
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("ci inspect accepts no positional arguments")
	}
	if *workspace == "" {
		return errors.New("ci inspect requires --workspace")
	}
	if *job == "" {
		return errors.New("ci inspect requires --job")
	}
	client, token, err := ciRemote(fs, apiURL, accessToken, envPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := client.InspectCIJob(ctx, token, *workspace, *job)
	if err != nil {
		return err
	}
	return printJSON(raw)
}

func ciHelpRequested(args []string) bool {
	return len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")
}

// ciRemote mirrors authenticatedRemote's credential precedence without ever
// holding a live credential in a flag default (flag usage output must never
// print one): an explicit --api-url beats an explicit env-file CANTER_API_URL,
// which beats the ambient CANTER_API_URL default; an explicit --access-token
// beats the environment entirely (resolved only when the flag was not
// supplied, so an explicit empty value cannot fall back to ambient). Without
// the flag, credentials come from resolveAPICredential: ambient
// CANTER_API_KEY, ambient CANTER_AGENT_ACCESS_TOKEN, then env-file values of
// each. Both personal keys and agent tokens are accepted. Nothing is
// persisted, refreshed, or logged.
func ciRemote(fs *flag.FlagSet, apiURL, accessToken, envPath *string) (*remote.Client, string, error) {
	apiURLExplicit := flagWasSet(fs, "api-url")
	accessTokenExplicit := flagWasSet(fs, "access-token")
	values, err := explicitEnvValues(*envPath)
	if err != nil {
		return nil, "", err
	}
	if !accessTokenExplicit {
		token, _, err := resolveAPICredential(values)
		if err != nil && !errors.Is(err, errNoAPICredential) {
			return nil, "", err
		}
		*accessToken = token
	} else if *accessToken == "" {
		// An explicit empty flag keeps the historical file-only fallback and
		// never reads ambient credentials.
		*accessToken = values["CANTER_AGENT_ACCESS_TOKEN"]
	}
	if !apiURLExplicit && values["CANTER_API_URL"] != "" {
		*apiURL = values["CANTER_API_URL"]
	}
	if *accessToken == "" {
		return nil, "", errors.New("command requires --access-token, CANTER_API_KEY or CANTER_AGENT_ACCESS_TOKEN, or an explicit --env-file containing one")
	}
	client, err := remote.New(*apiURL, nil)
	if err != nil {
		return nil, "", err
	}
	return client, *accessToken, nil
}
