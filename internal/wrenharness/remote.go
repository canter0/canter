package wrenharness

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
)

//go:embed bridge.py
var bridgeSource []byte
var safeProcessID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,79}$`)
var safeDeliveryID = regexp.MustCompile(`^[0-9]{20}$`)
var ErrProcessExited = errors.New("ACP harness process exited; inspect its saved state before starting another")

func validDirectory(value string) bool {
	return value == "/workspace" || strings.HasPrefix(value, "/workspace/") && path.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

// BrokerCredential is a short-lived per-job platform capability, never a master
// provider key. The server must independently enforce model, time and call caps.
type BrokerCredential struct {
	BaseURL         string `json:"-"`
	Token           string `json:"-"`
	Model           string
	ExpiresAt       time.Time
	MaxRequests     int
	MaxOutputTokens int
}
type OpenCodeLaunch struct {
	ProcessID      string
	Directory      string
	Broker         BrokerCredential
	TimeoutSeconds int
}
type Remote struct {
	computer  *wrencomputer.Client
	processID string
	directory string
}

// The original guest returned invalid_file for both an absent path and a
// directory. For optional adapter files only, allow a creation attempt; the
// guest's atomic write still rejects directories and out-of-scope paths.
func optionalFileAbsent(err error) bool {
	var guestError *wrencomputer.Error
	return errors.As(err, &guestError) && (guestError.Code == "not_found" || guestError.Code == "invalid_file")
}

func OpenCodeConfig(credential BrokerCredential) ([]byte, error) {
	u, err := url.Parse(credential.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("the model broker requires an HTTPS base URL")
	}
	if !strings.HasPrefix(credential.Token, "wren_broker_") || len(credential.Token) < 32 || strings.ContainsAny(credential.Token, "\r\n \t") {
		return nil, fmt.Errorf("a job-scoped model broker credential is required")
	}
	if credential.Model == "" || len(credential.Model) > 120 || strings.ContainsAny(credential.Model, "\r\n") {
		return nil, fmt.Errorf("the platform must select the coding model")
	}
	if !credential.ExpiresAt.After(time.Now()) || credential.ExpiresAt.After(time.Now().Add(time.Hour+time.Minute)) || credential.MaxRequests < 1 || credential.MaxRequests > 200 {
		return nil, fmt.Errorf("the model broker grant must have bounded calls and at most one hour remaining")
	}
	output := credential.MaxOutputTokens
	if output == 0 {
		output = 16384
	}
	if output < 1 || output > 16384 {
		return nil, fmt.Errorf("the model output limit must fit the platform grant")
	}
	return json.Marshal(map[string]any{"$schema": "https://opencode.ai/config.json", "enabled_providers": []string{"canter"}, "model": "canter/" + credential.Model, "small_model": "canter/" + credential.Model, "autoupdate": false, "share": "disabled", "permission": "ask", "provider": map[string]any{"canter": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "Canter managed coding", "options": map[string]string{"baseURL": credential.BaseURL, "apiKey": "{env:CANTER_MODEL_BROKER_TOKEN}"}, "models": map[string]any{credential.Model: map[string]any{"name": "Canter managed coding", "limit": map[string]int{"context": 128000, "output": output}}}}}})
}
func StartOpenCode(ctx context.Context, computer *wrencomputer.Client, launch OpenCodeLaunch) (*Remote, error) {
	if computer == nil || !safeProcessID.MatchString(launch.ProcessID) || !validDirectory(launch.Directory) {
		return nil, fmt.Errorf("a valid authorized computer and process scope are required")
	}
	config, err := OpenCodeConfig(launch.Broker)
	if err != nil {
		return nil, err
	}
	timeout := launch.TimeoutSeconds
	if timeout == 0 {
		timeout = 1800
	}
	if timeout < 30 || timeout > 3600 {
		return nil, fmt.Errorf("harness lifetime must fit its broker grant")
	}
	directory := "/workspace/.canter/harness/" + launch.ProcessID
	files := map[string][]byte{"/workspace/.canter/harness-bridge.py": bridgeSource, directory + "/opencode.json": config}
	launchData, err := json.Marshal(map[string]any{"processId": launch.ProcessID, "cwd": launch.Directory, "argv": []string{"opencode", "acp"}, "timeoutSeconds": timeout, "expiresAt": launch.Broker.ExpiresAt.Format(time.RFC3339Nano), "environment": map[string]string{"OPENCODE_CONFIG": directory + "/opencode.json", "HOME": directory + "/home", "XDG_CONFIG_HOME": directory + "/home/.config", "XDG_DATA_HOME": directory + "/home/.local/share", "XDG_CACHE_HOME": directory + "/home/.cache", "CANTER_MODEL_BROKER_TOKEN": launch.Broker.Token, "OPENCODE_DISABLE_AUTOUPDATE": "true", "OPENCODE_DISABLE_SHARE": "true"}})
	if err != nil {
		return nil, err
	}
	previous, readErr := computer.ReadFile(ctx, directory+"/launch.json", 0, 2<<20)
	if readErr == nil && !bytes.Equal(previous.Data, launchData) {
		return nil, fmt.Errorf("harness process identity already belongs to another launch")
	}
	if readErr == nil {
		if _, e := computer.Job(ctx, launch.ProcessID); e == nil {
			return &Remote{computer: computer, processID: launch.ProcessID, directory: directory}, nil
		} else {
			var guestError *wrencomputer.Error
			if !errors.As(e, &guestError) || guestError.Code != "not_found" {
				return nil, e
			}
		}
	}
	if readErr != nil {
		if !optionalFileAbsent(readErr) {
			return nil, readErr
		}
	}
	files[directory+"/launch.json"] = launchData
	// This is explicit scope-local material. No ~/.opencode or host auth file is
	// read/copied, and the only secret delivered to the guest expires with the job.
	for name, data := range files {
		if _, err = computer.WriteFile(ctx, name, data); err != nil {
			return nil, err
		}
	}
	if _, err = computer.StartJob(ctx, wrencomputer.JobRequest{ID: launch.ProcessID, Command: "python3 /workspace/.canter/harness-bridge.py " + directory, Directory: launch.Directory, TimeoutSeconds: min(timeout, int(math.Ceil(time.Until(launch.Broker.ExpiresAt).Seconds())))}); err != nil {
		return nil, err
	}
	return &Remote{computer: computer, processID: launch.ProcessID, directory: directory}, nil
}

// AttachRemote reconnects to an existing process and durable inbox/events. It
// never starts a replacement subprocess or silently repeats a prompt.
func AttachRemote(computer *wrencomputer.Client, processID string) (*Remote, error) {
	if computer == nil || !safeProcessID.MatchString(processID) {
		return nil, fmt.Errorf("invalid harness process")
	}
	return &Remote{computer: computer, processID: processID, directory: "/workspace/.canter/harness/" + processID}, nil
}
func (r *Remote) Send(ctx context.Context, deliveryID string, message json.RawMessage) error {
	if !safeDeliveryID.MatchString(deliveryID) || len(message) > 2<<20 || !json.Valid(message) {
		return fmt.Errorf("invalid ACP delivery")
	}
	target := r.directory + "/in/" + deliveryID + ".json"
	previous, err := r.computer.ReadFile(ctx, target, 0, 2<<20)
	if err == nil {
		if !bytes.Equal(previous.Data, message) {
			return fmt.Errorf("ACP delivery identity already belongs to another message")
		}
		return nil
	}
	if !optionalFileAbsent(err) {
		return err
	}
	_, err = r.computer.WriteFile(ctx, target, message)
	return err
}
func (r *Remote) Poll(ctx context.Context, cursor int64) (Batch, error) {
	if cursor < 0 {
		return Batch{}, fmt.Errorf("invalid ACP event cursor")
	}
	file, err := r.computer.ReadFile(ctx, r.directory+"/events.jsonl", cursor, 4<<20)
	if err != nil {
		if !optionalFileAbsent(err) {
			return Batch{}, err
		}
		job, e := r.computer.Job(ctx, r.processID)
		if e != nil {
			return Batch{}, e
		}
		if job.Status != "starting" && job.Status != "running" {
			return Batch{}, ErrProcessExited
		}
		return Batch{Cursor: cursor}, nil
	}
	if file.Size < cursor {
		return Batch{}, fmt.Errorf("ACP event history was lost or replaced; inspect the session before resuming")
	}
	batch := Batch{Cursor: cursor}
	offset := int64(0)
	for _, line := range bytes.SplitAfter(file.Data, []byte{'\n'}) {
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		offset += int64(len(line))
		var event struct {
			Message Message `json:"message"`
		}
		if json.Unmarshal(line, &event) != nil {
			return Batch{}, fmt.Errorf("invalid saved ACP event")
		}
		batch.Events = append(batch.Events, Received{Cursor: cursor + offset, Message: event.Message})
		batch.Cursor = cursor + offset
	}
	if len(batch.Events) == 0 {
		job, e := r.computer.Job(ctx, r.processID)
		if e != nil {
			return batch, e
		}
		if job.Status != "starting" && job.Status != "running" {
			return batch, ErrProcessExited
		}
	}
	return batch, nil
}
func (r *Remote) Stop(ctx context.Context) error {
	if _, err := r.computer.WriteFile(ctx, r.directory+"/stop", []byte("stop")); err != nil {
		return err
	}
	for i := 0; i < 20; i++ {
		job, err := r.computer.Job(ctx, r.processID)
		if err != nil {
			return err
		}
		if remoteJobStopped(job.Status) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	job, err := r.computer.StopJob(ctx, r.processID)
	if err != nil {
		return err
	}
	if !remoteJobStopped(job.Status) {
		return fmt.Errorf("ACP stop is pending; its process has not exited")
	}
	return nil
}

// Interrupted/unknown state is not proof that the managed process exited.
func remoteJobStopped(status string) bool {
	switch status {
	case "completed", "cancelled", "failed", "timed_out", "not_started":
		return true
	}
	return false
}
