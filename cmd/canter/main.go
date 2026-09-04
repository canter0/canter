package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/canter0/canter/internal/envfile"
	"github.com/canter0/canter/sdk"
)

const version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "canter:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a command is required")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		usage()
		return nil
	}
	if len(args) == 2 && (args[1] == "help" || args[1] == "--help" || args[1] == "-h") {
		switch args[0] {
		case "host":
			fmt.Println(hostUsage)
			return nil
		case "release":
			fmt.Println(releaseUsage)
			return nil
		case "change":
			fmt.Println(changeUsage)
			return nil
		case "agent":
			fmt.Println(agentUsage)
			return nil
		}
	}
	if args[0] == "change" && len(args) > 1 && (args[1] == "init" || args[1] == "schema" || args[1] == "validate") {
		return changeCommand(nil, args[1:])
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "init":
		return initCommand(args[1:])
	case "compile":
		return compileCommand(args[1:])
	case "agent":
		return agentCommand(args[1:])
	case "probe", "plan", "checkpoint", "apply", "status", "destroy", "inspect", "host", "release", "change":
		if _, err := envfile.Load(); err != nil {
			return err
		}
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
	client, err := sdk.NewFromEnv()
	if err != nil {
		return err
	}
	switch args[0] {
	case "probe":
		return probeCommand(client, args[1:])
	case "plan":
		return planCommand(client, args[1:])
	case "checkpoint":
		return checkpointCommand(client, args[1:])
	case "apply":
		return applyCommand(client, args[1:])
	case "status":
		return statusCommand(client, args[1:])
	case "destroy":
		return destroyCommand(client, args[1:])
	case "inspect":
		return inspectCommand(client, args[1:])
	case "host":
		return hostCommand(client, args[1:])
	case "release":
		return releaseCommand(client, args[1:])
	case "change":
		return changeCommand(client, args[1:])
	}
	return nil
}

func inspectCommand(client *sdk.Client, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	file := fs.String("file", "system.yaml", "system contract path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	system, err := sdk.LoadSystem(*file)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	view, err := client.InspectSystem(ctx, system)
	if err != nil {
		return err
	}
	return printJSON(view)
}

func compileCommand(args []string) error {
	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	file := fs.String("file", "system.yaml", "system contract path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	system, err := sdk.LoadSystem(*file)
	if err != nil {
		return err
	}
	graph, err := sdk.CompileSystem(system)
	if err != nil {
		return err
	}
	return printJSON(graph)
}

func initCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	file := fs.String("file", "canter.yaml", "path to write")
	name := fs.String("name", "first-sandbox", "sandbox name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name != "first-sandbox" {
		s := strings.ReplaceAll(sdk.StarterYAML, "first-sandbox", *name)
		return writeExclusive(*file, []byte(s))
	}
	return writeExclusive(*file, []byte(sdk.StarterYAML))
}

func probeCommand(client *sdk.Client, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	report := client.Probe(ctx)
	if *jsonOut {
		return printJSON(report)
	}
	fmt.Printf("model   %s  %dms\n", mark(report.Model.OK), report.Model.Latency.Milliseconds())
	fmt.Printf("compute %s  %dms  resources=%d shapes=%d images=%d networks=%d\n", mark(report.Compute.OK), report.Compute.Latency.Milliseconds(), report.Compute.Servers, report.Compute.Shapes, report.Compute.Images, report.Compute.Networks)
	fmt.Printf("m1      %s  %dms\n", mark(report.M1.OK), report.M1.Latency.Milliseconds())
	fmt.Printf("total        %dms\n", report.ElapsedMS)
	if !report.OK() {
		return errors.New("one or more live probes failed")
	}
	return nil
}

func planCommand(client *sdk.Client, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	file := fs.String("file", "canter.yaml", "spec path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := sdk.LoadSpec(*file)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := client.Plan(ctx, s)
	if err != nil {
		return err
	}
	return printJSON(p)
}

func checkpointCommand(client *sdk.Client, args []string) error {
	fs := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	file := fs.String("file", "canter.yaml", "spec path")
	message := fs.String("message", "agent checkpoint", "checkpoint message")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := sdk.LoadSpec(*file)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cp, err := client.Checkpoint(ctx, s.Metadata.Name, s.Spec.M1.Prefix, *message)
	if err != nil {
		return err
	}
	return printJSON(cp)
}

func applyCommand(client *sdk.Client, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	file := fs.String("file", "canter.yaml", "spec path")
	timeout := fs.Duration("timeout", 3*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := sdk.LoadSpec(*file)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := client.Apply(ctx, s)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func statusCommand(client *sdk.Client, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	file := fs.String("file", "canter.yaml", "spec path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := sdk.LoadSpec(*file)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	state, err := client.Status(ctx, s)
	if err != nil {
		return err
	}
	return printJSON(state)
}

func destroyCommand(client *sdk.Client, args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	file := fs.String("file", "canter.yaml", "spec path")
	yes := fs.Bool("yes", false, "confirm destruction")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*yes {
		return errors.New("destroy requires --yes")
	}
	s, err := sdk.LoadSpec(*file)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	state, err := client.Destroy(ctx, s)
	if err != nil {
		return err
	}
	return printJSON(state)
}

func writeExclusive(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}
func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
func mark(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}
func usage() {
	fmt.Fprintln(os.Stderr, "usage: canter <init|compile|probe|plan|checkpoint|apply|status|destroy|inspect|host|release|change|agent|version>")
	fmt.Fprintln(os.Stderr, "run 'canter <command> -h' for command flags")
}
