package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/canter0/canter/sdk"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "blackbox-firecracker-mysql:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: blackbox-firecracker-mysql <contract|compile|render>")
	}
	switch args[0] {
	case "contract":
		system, err := BuildSystem()
		if err != nil {
			return err
		}
		return writeStdoutYAML(system)
	case "compile":
		system, err := BuildSystem()
		if err != nil {
			return err
		}
		graph, err := sdk.CompileSystem(system)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(graph, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, "%s\n", b)
		return err
	case "render":
		return renderCommand(args[1:])
	default:
		return fmt.Errorf("unknown command %q (want contract, compile, or render)", args[0])
	}
}

func renderCommand(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	output := fs.String("output", "canter.yaml", "output path, or - for stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := RenderSandbox()
	if err != nil {
		return err
	}
	b, err := marshalYAML(spec)
	if err != nil {
		return err
	}
	if *output == "-" {
		_, err = os.Stdout.Write(b)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*output, b, 0o644); err != nil {
		return err
	}
	fmt.Println(*output)
	return nil
}

func writeStdoutYAML(value any) error {
	b, err := marshalYAML(value)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}
