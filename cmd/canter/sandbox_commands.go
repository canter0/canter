package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/canter0/canter/sdk/remote"
)

var sandboxCLIID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

const sandboxUsage = `Usage: canter sandbox <command> [flags]

Commands:
  list                          list sandboxes in the workspace
  create --file spec.json       create a sandbox (spec requires idempotencyKey) [--wait]
  get --id                      show one sandbox
  wait --id --phase PHASE       wait for ready|asleep|deleted [--timeout]
  exec --id --command CMD       run a command and follow logs [--detach]
  job --id --job JOB            show a job receipt
  logs --id --job JOB           stream job logs to stdout [--stream --offset]
  stop --id --job JOB           stop a job
  upload --id --path P --file F upload a local file
  download --id --path P --file F  download to a local file [--force]
  sleep --id                    suspend the sandbox
  wake --id                     resume the sandbox
  delete --id                   delete the sandbox
  save --id [--name N] [--keep] suspend and save the environment
  snapshots [--source ID] [--before CURSOR]  list saved environments
  templates                     list sandbox templates
  template --name N --snapshot ID  create a template from a snapshot
  restore --id --snapshot ID --generation N  restore an asleep sandbox

Shared flags:
  --api-url        Canter control-plane URL (default CANTER_API_URL or http://127.0.0.1:8081)
  --env-file       file holding CANTER_AGENT_ACCESS_TOKEN / CANTER_WORKSPACE_ID
  --workspace      workspace ID (default CANTER_WORKSPACE_ID or the token's installation)
  --timeout        operation timeout (default 15m)

Access: connect an agent with "canter agent connect"; refresh with
"canter agent refresh". Never pass a token on the command line — use
CANTER_AGENT_ACCESS_TOKEN or --env-file. create requires a stable caller
idempotencyKey so interrupted requests replay the exact same body; a changed
body under the same key conflicts. All commands use this token's workspace
and permission scope only.
`

// sandboxCommand routes `canter sandbox <subcommand>` — it runs before
// provider envfile loading so no provider credentials are needed.
func sandboxCommand(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, sandboxUsage)
		return errors.New("a sandbox command is required")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(sandboxUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	known := map[string]bool{
		"list": true, "create": true, "get": true, "wait": true, "exec": true,
		"job": true, "logs": true, "stop": true, "upload": true, "download": true,
		"sleep": true, "wake": true, "delete": true, "save": true,
		"snapshots": true, "templates": true, "template": true, "restore": true,
	}
	if !known[sub] {
		fmt.Fprint(os.Stderr, sandboxUsage)
		return fmt.Errorf("unknown sandbox command %q", sub)
	}
	return runSandboxSubcommand(sub, rest)
}

// sandboxFlags are the shared per-command flags.
type sandboxFlags struct {
	fs        *flag.FlagSet
	apiURL    *string
	envFile   *string
	workspace *string
	timeout   *time.Duration
}

func newSandboxFlags(name string) *sandboxFlags {
	fs := flag.NewFlagSet("sandbox "+name, flag.ContinueOnError)
	f := &sandboxFlags{fs: fs}
	f.apiURL = fs.String("api-url", defaultAPIURL(), "Canter control-plane URL")
	f.envFile = fs.String("env-file", "", "file with CANTER_AGENT_ACCESS_TOKEN / CANTER_WORKSPACE_ID")
	f.workspace = fs.String("workspace", "", "workspace ID")
	f.timeout = fs.Duration("timeout", 15*time.Minute, "operation timeout")
	return f
}

func (f *sandboxFlags) parse(args []string) error {
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	if f.fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", strings.Join(f.fs.Args(), " "))
	}
	return nil
}

// client resolves the token + workspace and builds the scoped client.
func (f *sandboxFlags) client(ctx context.Context) (*remote.SandboxClient, error) {
	values, err := explicitEnvValues(*f.envFile)
	if err != nil {
		return nil, err
	}
	apiURL := *f.apiURL
	if !flagWasSet(f.fs, "api-url") && values["CANTER_API_URL"] != "" {
		apiURL = values["CANTER_API_URL"]
	}
	token := strings.TrimSpace(os.Getenv("CANTER_AGENT_ACCESS_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(values["CANTER_AGENT_ACCESS_TOKEN"])
	}
	if token == "" {
		return nil, errors.New("no Canter access token: set CANTER_AGENT_ACCESS_TOKEN or use --env-file (see canter agent connect)")
	}
	base, err := remote.New(apiURL, nil)
	if err != nil {
		return nil, err
	}
	workspace := strings.TrimSpace(*f.workspace)
	if workspace == "" {
		workspace = strings.TrimSpace(os.Getenv("CANTER_WORKSPACE_ID"))
	}
	if workspace == "" {
		workspace = strings.TrimSpace(values["CANTER_WORKSPACE_ID"])
	}
	if workspace == "" {
		identity, err := base.WhoAmI(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace from token: %w", err)
		}
		workspace = identity.Installation.WorkspaceID
	}
	if workspace == "" {
		return nil, errors.New("no workspace: set --workspace or CANTER_WORKSPACE_ID")
	}
	return base.Sandboxes(workspace, token)
}

func sandboxContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if timeout > 0 {
		deadline, cancel := context.WithTimeout(ctx, timeout)
		return deadline, func() { cancel(); stop() }
	}
	return ctx, stop
}

func sandboxPrintJSON(out any) error {
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

// sandboxReadFile reads a bounded JSON spec file through an opened regular
// file — a growing file can never exceed the limit.
func sandboxReadFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return raw, nil
}

// sandboxStrictJSON decodes a spec with no unknown fields and a single
// top-level value.
func sandboxStrictJSON(raw []byte, out any, label string) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("parse %s: %w", label, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("%s contains trailing data after the JSON value", label)
	}
	return nil
}

// sandboxDownloadFile writes a remote file atomically: temp sibling + rename,
// never clobbering an existing file unless force.
func sandboxDownloadFile(dest string, force bool, write func(io.Writer) error) error {
	if !force {
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("%s exists; pass --force to replace it", dest)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(dest), ".canter-download-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := write(temp); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if force {
		return os.Rename(tempPath, dest)
	}
	// os.Link fails atomically if any destination entry exists, including a
	// dangling symlink — a destination created during the download can never
	// be silently replaced.
	if err := os.Link(tempPath, dest); err != nil {
		return err
	}
	return nil // deferred os.Remove(tempPath) removes only the temporary name
}

func runSandboxSubcommand(sub string, args []string) error {
	f := newSandboxFlags(sub)
	var (
		id, jobID, phase, command, directory, file, path, name, source, before, snapshot, stream string
		wait, detach, keep, force                                                                bool
		offset                                                                                   int64
		generation, timeoutSeconds                                                               int
		jobFile                                                                                  string
	)
	switch sub {
	case "create":
		f.fs.StringVar(&file, "file", "", "create spec JSON file (requires idempotencyKey)")
		f.fs.BoolVar(&wait, "wait", false, "wait for the sandbox to become ready")
	case "exec":
		f.fs.StringVar(&id, "id", "", "sandbox ID")
		f.fs.StringVar(&command, "command", "", "command to run")
		f.fs.StringVar(&directory, "directory", "", "working directory")
		f.fs.StringVar(&jobFile, "job-file", "", "full exec spec JSON file")
		f.fs.IntVar(&timeoutSeconds, "timeout-seconds", 0, "job timeout seconds")
		f.fs.BoolVar(&detach, "detach", false, "return the job receipt without following logs")
	case "job", "stop":
		f.fs.StringVar(&id, "id", "", "sandbox ID")
		f.fs.StringVar(&jobID, "job", "", "job ID")
	case "logs":
		f.fs.StringVar(&id, "id", "", "sandbox ID")
		f.fs.StringVar(&jobID, "job", "", "job ID")
		f.fs.StringVar(&stream, "stream", "combined", "combined|stdout|stderr")
		f.fs.Int64Var(&offset, "offset", 0, "resume byte offset")
	case "upload", "download":
		f.fs.StringVar(&id, "id", "", "sandbox ID")
		f.fs.StringVar(&path, "path", "", "remote file path")
		f.fs.StringVar(&file, "file", "", "local file path")
		if sub == "download" {
			f.fs.BoolVar(&force, "force", false, "replace an existing local file")
		}
	case "wait":
		f.fs.StringVar(&id, "id", "", "sandbox ID")
		f.fs.StringVar(&phase, "phase", "", "ready|asleep|deleted")
	case "get", "sleep", "wake", "delete":
		f.fs.StringVar(&id, "id", "", "sandbox ID")
	case "save":
		f.fs.StringVar(&id, "id", "", "sandbox ID")
		f.fs.StringVar(&name, "name", "", "saved environment name")
		f.fs.BoolVar(&keep, "keep", false, "retain the saved environment")
	case "snapshots":
		f.fs.StringVar(&source, "source", "", "filter by source sandbox ID")
		f.fs.StringVar(&before, "before", "", "page cursor")
	case "template":
		f.fs.StringVar(&name, "name", "", "template name")
		f.fs.StringVar(&snapshot, "snapshot", "", "snapshot ID")
	case "restore":
		f.fs.StringVar(&id, "id", "", "sandbox ID")
		f.fs.StringVar(&snapshot, "snapshot", "", "snapshot ID")
		f.fs.IntVar(&generation, "generation", 0, "expected generation")
	}
	if err := f.parse(args); err != nil {
		return err
	}
	if *f.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	requireID := func(value, label string) (string, error) {
		if value == "" {
			return "", fmt.Errorf("--%s is required", label)
		}
		return value, nil
	}
	// Validate flags and parse local specs before any credential or network call.
	var (
		createInput remote.SandboxCreateInput
		jobInput    remote.SandboxExecInput
	)
	switch sub {
	case "create":
		if file == "" {
			return errors.New("--file is required")
		}
		raw, err := sandboxReadFile(file, 192<<10)
		if err != nil {
			return err
		}
		if err := sandboxStrictJSON(raw, &createInput, file); err != nil {
			return err
		}
	case "exec":
		if id == "" {
			return errors.New("--id is required")
		}
		if jobFile != "" && (flagWasSet(f.fs, "command") || flagWasSet(f.fs, "directory") || flagWasSet(f.fs, "timeout-seconds")) {
			return errors.New("--job-file cannot be combined with --command, --directory, or --timeout-seconds")
		}
		if jobFile == "" && command == "" {
			return errors.New("--command or --job-file is required")
		}
		if jobFile != "" {
			raw, err := sandboxReadFile(jobFile, 96<<10)
			if err != nil {
				return err
			}
			if err := sandboxStrictJSON(raw, &jobInput, jobFile); err != nil {
				return err
			}
		}
	case "job", "stop":
		if id == "" || jobID == "" {
			return errors.New("--id and --job are required")
		}
	case "logs":
		if id == "" || jobID == "" {
			return errors.New("--id and --job are required")
		}
		if stream != "combined" && stream != "stdout" && stream != "stderr" {
			return errors.New("--stream must be combined|stdout|stderr")
		}
		if offset < 0 {
			return errors.New("--offset must be nonnegative")
		}
	case "upload", "download":
		if id == "" || path == "" || file == "" {
			return errors.New("--id, --path, and --file are required")
		}
	case "wait":
		if id == "" {
			return errors.New("--id is required")
		}
		if phase != "ready" && phase != "asleep" && phase != "deleted" {
			return errors.New("--phase must be ready|asleep|deleted")
		}
	case "get", "sleep", "wake", "delete":
		if id == "" {
			return errors.New("--id is required")
		}
	case "save":
		if id == "" {
			return errors.New("--id is required")
		}
	case "template":
		if name == "" || snapshot == "" {
			return errors.New("--name and --snapshot are required")
		}
	case "restore":
		if id == "" || snapshot == "" {
			return errors.New("--id and --snapshot are required")
		}
		if generation < 1 {
			return errors.New("--generation must be a positive integer")
		}
	}
	for _, candidate := range []struct{ value, label string }{
		{id, "id"}, {jobID, "job"}, {snapshot, "snapshot"}, {source, "source"}, {before, "before"},
	} {
		if candidate.value != "" && !sandboxCLIID.MatchString(candidate.value) {
			return fmt.Errorf("invalid --%s %q", candidate.label, candidate.value)
		}
	}
	ctx, stop := sandboxContext(*f.timeout)
	defer stop()
	client, err := f.client(ctx)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		result, err := client.List(ctx)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "create":
		created, err := client.Create(ctx, createInput)
		if err != nil {
			return err
		}
		if wait {
			fmt.Fprintf(os.Stderr, "created %s; waiting for ready\n", created.ID)
			created, err = client.Wait(ctx, created.ID, "ready")
			if err != nil {
				return err
			}
		}
		return sandboxPrintJSON(created)
	case "get":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		result, err := client.Get(ctx, sid)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "wait":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		result, err := client.Wait(ctx, sid, phase)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "exec":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		input := jobInput
		if jobFile == "" {
			input = remote.SandboxExecInput{Command: command, Directory: directory, TimeoutSeconds: timeoutSeconds}
		}
		if input.Command == "" {
			return errors.New("exec requires a command")
		}
		job, err := client.StartJob(ctx, sid, input)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "job %s\n", job.JobID)
		if detach {
			return sandboxPrintJSON(job)
		}
		var final remote.SandboxJob
		streamErr := client.StreamLogs(ctx, sid, job.JobID, "combined", 0, func(event remote.SandboxLogEvent) error {
			if event.Event == "output" {
				n, err := os.Stdout.Write(event.Data)
				if err != nil {
					return err
				}
				if n != len(event.Data) {
					return io.ErrShortWrite
				}
				return nil
			}
			if event.Event == "status" {
				final.JobID = event.JobID
				final.Status = event.Status
				final.ExitCode = event.ExitCode
				final.LogTruncated = event.LogTruncated
			}
			return nil
		})
		if streamErr != nil {
			return streamErr
		}
		fmt.Fprintf(os.Stderr, "job %s %s\n", job.JobID, final.Status)
		if final.ExitCode != nil && *final.ExitCode != 0 {
			return fmt.Errorf("job %s exited %d", job.JobID, *final.ExitCode)
		}
		if final.Status != "completed" || final.ExitCode == nil || *final.ExitCode != 0 {
			return fmt.Errorf("job %s did not complete successfully (status %s)", job.JobID, final.Status)
		}
		return nil
	case "job":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		jid, err := requireID(jobID, "job")
		if err != nil {
			return err
		}
		result, err := client.Job(ctx, sid, jid)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "stop":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		jid, err := requireID(jobID, "job")
		if err != nil {
			return err
		}
		result, err := client.StopJob(ctx, sid, jid)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "logs":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		jid, err := requireID(jobID, "job")
		if err != nil {
			return err
		}
		return client.StreamLogs(ctx, sid, jid, stream, offset, func(event remote.SandboxLogEvent) error {
			if event.Event == "output" {
				n, err := os.Stdout.Write(event.Data)
				if err != nil {
					return err
				}
				if n != len(event.Data) {
					return io.ErrShortWrite
				}
			}
			return nil
		})
	case "upload":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		sourceFile, err := os.Open(file)
		if err != nil {
			return err
		}
		defer sourceFile.Close()
		result, err := client.Upload(ctx, sid, path, sourceFile)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "download":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		return sandboxDownloadFile(file, force, func(dest io.Writer) error {
			return client.Download(ctx, sid, path, dest)
		})
	case "sleep":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		return client.Sleep(ctx, sid)
	case "wake":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		return client.Wake(ctx, sid)
	case "delete":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		return client.Delete(ctx, sid)
	case "save":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		result, err := client.Save(ctx, sid, name, keep)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "snapshots":
		result, err := client.Snapshots(ctx, source, before)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "templates":
		result, err := client.Templates(ctx)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "template":
		result, err := client.CreateTemplate(ctx, name, snapshot)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	case "restore":
		sid, err := requireID(id, "id")
		if err != nil {
			return err
		}
		result, err := client.Restore(ctx, sid, snapshot, generation)
		if err != nil {
			return err
		}
		return sandboxPrintJSON(result)
	}
	return nil
}
