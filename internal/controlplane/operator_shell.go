package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Shared by all operator workers in this process, not one slot per conversation.
// Production additionally bounds process memory and global concurrency through
// the socket-activated service in deploy/canter-harness.{socket,@.service}.
var operatorShellSlot = make(chan struct{}, 1)

type operatorShellRequest struct {
	Command string            `json:"command"`
	Files   map[string]string `json:"files"`
	Scratch map[string]string `json:"scratch"`
}

type operatorShellResult struct {
	Stdout           string            `json:"stdout"`
	Stderr           string            `json:"stderr"`
	ExitCode         int               `json:"exitCode"`
	Scratch          map[string]string `json:"scratch,omitempty"`
	PersistenceError string            `json:"persistenceError,omitempty"`
	Failed           bool              `json:"failed,omitempty"`
	Metrics          struct {
		PeakRSSKiB int64 `json:"peakRssKiB"`
	} `json:"metrics"`
}

func (c OperatorConfig) shellReady() bool { return c.ShellSocket != "" || c.ShellRunner != "" }

// CheckShell verifies the configured bridge at startup, before advertising it.
func (c OperatorConfig) CheckShell(ctx context.Context) error {
	if !c.shellReady() {
		return nil
	}
	result, err := c.runShell(ctx, operatorShellRequest{Command: "printf canter-ready"})
	if err != nil {
		return err
	}
	if result.Failed || result.ExitCode != 0 || result.Stdout != "canter-ready" {
		return fmt.Errorf("workspace command environment failed its startup check")
	}
	return nil
}

func validateOperatorScratch(files map[string]string) error {
	if len(files) > 64 {
		return fmt.Errorf("too many scratch files")
	}
	size := 0
	for name, value := range files {
		if len(name) > 256 || !strings.HasPrefix(name, "/scratch/") || path.Clean(name) != name || strings.ContainsRune(name, 0) {
			return fmt.Errorf("invalid scratch path")
		}
		size += len(name) + len(value)
	}
	if size > 256<<10 {
		return fmt.Errorf("scratch files exceed 256 KiB")
	}
	return nil
}

type boundedOperatorBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedOperatorBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("command response limit exceeded")
	}
	return b.Buffer.Write(p)
}

func (c OperatorConfig) runShell(ctx context.Context, request operatorShellRequest) (operatorShellResult, error) {
	var result operatorShellResult
	if !c.shellReady() {
		return result, fmt.Errorf("the workspace command environment is unavailable")
	}
	if strings.TrimSpace(request.Command) == "" || len(request.Command) > 16000 {
		return result, fmt.Errorf("command must contain 1 to 16000 bytes")
	}
	if err := validateOperatorScratch(request.Scratch); err != nil {
		return result, err
	}
	input, err := json.Marshal(request)
	if err != nil || len(input) > 3<<20 {
		return result, fmt.Errorf("command input exceeds its context budget")
	}
	select {
	case operatorShellSlot <- struct{}{}:
		defer func() { <-operatorShellSlot }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	output := &boundedOperatorBuffer{limit: 2 << 20}
	if c.ShellSocket != "" {
		connection, dialErr := (&net.Dialer{}).DialContext(ctx, "unix", c.ShellSocket)
		if dialErr != nil {
			return result, fmt.Errorf("the workspace command environment could not start")
		}
		defer connection.Close()
		stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
		defer stop()
		deadline, _ := ctx.Deadline()
		_ = connection.SetDeadline(deadline)
		if _, err = io.Copy(connection, bytes.NewReader(input)); err == nil {
			err = connection.(*net.UnixConn).CloseWrite()
		}
		if err == nil {
			_, err = io.Copy(output, io.LimitReader(connection, int64(output.limit)+1))
		}
	} else {
		// Development uses the identical protocol in a disposable process. No
		// shell interpolation, inherited credentials, host writes, or networking.
		runner, pathErr := filepath.Abs(c.ShellRunner)
		if pathErr != nil {
			return result, fmt.Errorf("invalid workspace command runner")
		}
		node := c.ShellNode
		if node == "" {
			node = "node"
		}
		command := exec.CommandContext(ctx, node, "--permission", "--allow-fs-read="+filepath.Dir(runner), "--max-old-space-size=96", runner)
		command.Dir = filepath.Dir(runner)
		command.Env = []string{"LANG=C.UTF-8", "TZ=UTC"}
		command.Stdin = bytes.NewReader(input)
		command.Stdout = output
		command.Stderr = io.Discard
		command.WaitDelay = time.Second
		err = command.Run()
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("the workspace command was stopped or reached its time limit")
	}
	if err != nil || json.Unmarshal(output.Bytes(), &result) != nil {
		return result, fmt.Errorf("the workspace command could not finish within its resource limits")
	}
	if len(result.Stdout) > 64<<10 || len(result.Stderr) > 64<<10 {
		return operatorShellResult{}, fmt.Errorf("command output exceeded its limit")
	}
	if err = validateOperatorScratch(result.Scratch); err != nil {
		return operatorShellResult{}, err
	}
	return result, nil
}
