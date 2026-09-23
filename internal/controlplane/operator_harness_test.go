package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func harnessTestConfig(t *testing.T) OperatorConfig {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("install Node >=22.13 to enable real-shell integration tests")
	}
	runner, err := filepath.Abs("../../harness/runner.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(runner), "node_modules/just-bash/package.json")); err != nil {
		t.Skip("run npm ci --prefix harness to enable real-shell integration tests")
	}
	return OperatorConfig{ShellNode: node, ShellRunner: runner}
}

func TestOperatorHarnessPoliciesAndContext(t *testing.T) {
	runtime := OperatorRuntime{Config: OperatorConfig{ShellRunner: "configured"}}
	for _, tool := range runtime.tools() {
		if _, ok := operatorPolicy(tool.Name); !ok {
			t.Fatalf("tool lacks an execution policy: %s", tool.Name)
		}
	}
	if _, ok := operatorPolicy("canter_create_vps"); ok {
		t.Fatal("unknown mutation accepted")
	}
	if policy, _ := operatorPolicy("canter_bash"); policy.RetrySafe {
		t.Fatal("scratch appends cannot be blindly retried")
	}
	if policy, _ := operatorPolicy("canter_create_task"); !policy.RequireDraft {
		t.Fatal("task creation bypasses draft authority")
	}
	if value := operatorExcerpt("ab你好", 4); !utf8.ValidString(value) || value != "ab" {
		t.Fatalf("invalid UTF-8 boundary: %q", value)
	}

	messages := []modelMessage{{Role: "system", Content: "policy"}, {Role: "user", Content: "my project"}}
	for i := 0; i < 12; i++ {
		call := operatorTestCall("canter_show_billing", "{}")
		messages = append(messages, modelMessage{Role: "assistant", ToolCalls: []modelToolCall{call}}, modelMessage{Role: "tool", ToolCallID: call.ID, Content: strings.Repeat("a", 12000)})
	}
	messages = append(messages, modelMessage{Role: "user", Content: "Use Toronto instead"})
	bounded, err := operatorBoundContext(messages, "run_context")
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != len(messages) || bounded[len(bounded)-1].Content != "Use Toronto instead" {
		t.Fatal("context pruning lost tool pairing or current request")
	}
	if !strings.Contains(bounded[3].Content, "/results/") || len(messages[3].Content) != 12000 {
		t.Fatal("original evidence was mutated or recovery reference was lost")
	}
}

func TestOperatorHarnessRealShellAndCancellation(t *testing.T) {
	config := harnessTestConfig(t)
	result, err := config.runShell(context.Background(), operatorShellRequest{Command: "jq -r '.project' /workspace/context.json > /scratch/project; cat /scratch/project", Files: map[string]string{"/workspace/context.json": `{"project":"flight planner"}`}})
	if err != nil || result.ExitCode != 0 || result.Stdout != "flight planner\n" {
		t.Fatalf("real shell: %+v %v", result, err)
	}
	t.Logf("command peak RSS: %d KiB", result.Metrics.PeakRSSKiB)
	if result.Metrics.PeakRSSKiB >= 192*1024 {
		t.Fatal("shell exceeds production process budget")
	}
	resumed, err := config.runShell(context.Background(), operatorShellRequest{Command: "cat /scratch/project", Scratch: result.Scratch})
	if err != nil || resumed.Stdout != result.Stdout {
		t.Fatalf("snapshot resume: %+v %v", resumed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = config.runShell(ctx, operatorShellRequest{Command: "echo should-not-run"}); err == nil {
		t.Fatal("cancelled command ran")
	}
}

func TestOperatorHarnessSocketProtocol(t *testing.T) {
	config := harnessTestConfig(t)
	// Keep the path short enough for Darwin's Unix socket path limit.
	directory, err := os.MkdirTemp("", "canter-shell-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "runner.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		file, err := connection.File()
		connection.Close()
		if err != nil {
			done <- err
			return
		}
		defer file.Close()
		// Match systemd's accepted socket on both stdin and stdout.
		command := exec.CommandContext(ctx, config.ShellNode, "--permission", "--allow-fs-read="+filepath.Dir(config.ShellRunner), "--max-old-space-size=96", config.ShellRunner)
		command.Env = []string{"LANG=C.UTF-8", "TZ=UTC"}
		command.Stdin, command.Stdout = file, file
		done <- command.Run()
	}()
	if err = (OperatorConfig{ShellSocket: socket}).CheckShell(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOperatorHarnessPersistenceIsolationAndRecovery(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	config := harnessTestConfig(t)
	runtime := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{Operator: config}).(*HTTPServer), Config: config}
	_, err := s.EnqueueOperator(ctx, c, "harness_request_1", "Use Toronto for my flight planner", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	p, err := s.operatorPrincipal(ctx, c, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	call := operatorTestCall("canter_bash", `{"command":"printf 'Toronto' > /scratch/region; cat /scratch/region"}`)
	result, err := runtime.executeTool(ctx, run, c, p, call)
	if err != nil || !strings.Contains(string(result), "Toronto") {
		t.Fatalf("command failed: %s %v", result, err)
	}
	// Simulate a crash after scratch commit but before the tool result commits.
	if _, err = s.pool.Exec(ctx, `UPDATE operator_tool_calls SET result=NULL WHERE run_id=$1 AND call_id=$2`, run.ID, call.ID); err != nil {
		t.Fatal(err)
	}
	result, err = runtime.executeTool(ctx, run, c, p, call)
	if err != nil || !strings.Contains(string(result), "was not repeated") {
		t.Fatalf("ambiguous command was replayed: %s %v", result, err)
	}
	call.ID = "resume_read"
	call.Function.Arguments = `{"command":"cat /scratch/region"}`
	result, err = runtime.executeTool(ctx, run, c, p, call)
	if err != nil || !strings.Contains(string(result), "Toronto") {
		t.Fatalf("saved files lost: %s %v", result, err)
	}

	large, _ := json.Marshal(map[string]string{"evidence": strings.Repeat("line café\n", 30000)})
	file := operatorResultPath(run.ID, "large")
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments,result,result_path) VALUES($1,'large','canter_read_repository_file','{}',$2,$3)`, run.ID, large, file); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"path": file, "limit": 7000})
	page, _, err := runtime.harnessTool(ctx, run, c, p, "canter_read_result", args)
	if err != nil || page.(map[string]any)["complete"] != false {
		t.Fatalf("large evidence could not be paged: %v", err)
	}
	other, err := s.CreateConversation(ctx, c.WorkspaceID, c.AccountID, "conv_other_private", "Other project")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = runtime.harnessTool(ctx, run, other, p, "canter_read_result", args); !errors.Is(err, ErrNotFound) {
		t.Fatalf("result crossed conversations: %v", err)
	}
	a, _, _, err := s.Signup(ctx, "harness-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	other.AccountID = a.ID
	search, _, err := runtime.harnessTool(ctx, run, other, p, "canter_search_history", json.RawMessage(`{"query":"Toronto"}`))
	if err != nil || len(search.(map[string]any)["matches"].([]map[string]any)) != 0 {
		t.Fatalf("private history leaked: %+v %v", search, err)
	}
	if err = s.cancelOperator(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.saveOperatorScratch(ctx, run, map[string]string{"/scratch/late": "bad"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled worker saved files: %v", err)
	}
}

func TestOperatorInboxYieldsBeforePendingMutation(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	_, err := s.EnqueueOperator(ctx, c, "inbox_request_1", "Deploy the old project", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err = s.operatorCheckpoint(ctx, first, []modelMessage{{Role: "user", Content: "Deploy the old project"}, {Role: "assistant", ToolCalls: []modelToolCall{operatorTestCall("canter_create_task", `{"prompt":"Do old work"}`)}}}); err != nil {
		t.Fatal(err)
	}
	second, err := s.EnqueueOperator(ctx, c, "inbox_request_2", "Actually use the flight planner", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Restart migrations must not recreate the obsolete one-active-run index.
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.claimOperator(ctx); err != nil || claimed {
		t.Fatalf("overlapping run claimed: %v", err)
	}
	runtime := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)}
	if err = runtime.work(ctx, &first); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.ListWorkspaceTasks(ctx, c.WorkspaceID)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("superseded mutation executed: %+v %v", tasks, err)
	}
	next, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || next.ID != second.ID {
		t.Fatalf("follow-up lost: %+v %v", next, err)
	}
	history, err := s.operatorHistory(ctx, next, 80)
	if err != nil || history[len(history)-1].Content != "Actually use the flight planner" {
		t.Fatalf("follow-up context order incorrect: %+v %v", history, err)
	}
	principal, err := s.operatorPrincipal(ctx, c, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	call := operatorTestCall("canter_create_task", `{"prompt":"Integrate uploads in the flight planner; verify permissions and upload/download behavior."}`)
	created, err := runtime.executeTool(ctx, next, c, principal, call)
	if err != nil || !strings.Contains(string(created), "executorStarted") {
		t.Fatalf("handoff failed: %s %v", created, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_tool_calls SET result=NULL WHERE run_id=$1 AND call_id=$2`, next.ID, call.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.executeTool(ctx, next, c, principal, call); err != nil {
		t.Fatal(err)
	}
	tasks, err = s.ListWorkspaceTasks(ctx, c.WorkspaceID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("retry duplicated handoff: %+v %v", tasks, err)
	}
}

func TestOperatorUsageStreamAndCurrentRequestPreserved(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Ready\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"total_tokens\":105,\"cost\":0.001}}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	answer, err := (OperatorConfig{BaseURL: provider.URL, Model: "test"}).complete(context.Background(), []modelMessage{{Role: "user", Content: "hi"}}, nil, func(string) error { return nil })
	if err != nil || answer.Usage == nil || answer.Usage.TotalTokens != 105 {
		t.Fatalf("usage lost: %+v %v", answer, err)
	}
	if _, exists := operatorModelPayload(answer)["usage"]; exists {
		t.Fatal("accounting metadata leaked into model input")
	}
	request := strings.Repeat("current requirements ", 1000)
	messages := []modelMessage{{Role: "system", Content: "policy"}, {Role: "user", Content: request}}
	for i := 0; i < 12; i++ {
		messages = append(messages, modelMessage{Role: "tool", ToolCallID: fmt.Sprint(i), Content: strings.Repeat("x", 12000)})
	}
	bounded, err := operatorBoundContext(messages, "run")
	if err != nil || bounded[1].Content != request {
		t.Fatalf("current request was shortened: %v", err)
	}
}

// This opt-in check uses the actual external model and the real command bridge.
// A viewer grant prevents the model from mutating infrastructure or creating
// shared tasks, even if it unexpectedly requests those tools.
func TestOperatorHarnessLiveModel(t *testing.T) {
	if os.Getenv("CANTER_HARNESS_LIVE_MODEL") != "1" {
		t.Skip("live model verification is opt-in")
	}
	s, c, _ := operatorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	config := harnessTestConfig(t)
	config.APIKey = os.Getenv("CANTER_HARNESS_TEST_API_KEY")
	config.BaseURL = os.Getenv("CANTER_HARNESS_TEST_BASE_URL")
	config.Model = os.Getenv("CANTER_HARNESS_TEST_MODEL")
	if !config.Ready() {
		t.Fatal("live model test configuration is incomplete")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameConversation(ctx, c.WorkspaceID, c.AccountID, c.ID, "Harness verification"); err != nil {
		t.Fatal(err)
	}
	runtime := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{Operator: config}).(*HTTPServer), Config: config}
	prompts := []string{
		"Our project is the flight planner. We chose Toronto. Save those confirmed decisions as working context, then use the command environment to write Toronto to /scratch/region.txt and read it back. Report the actual output. This is a context and command test; do not create tasks or infrastructure.",
		"Read /scratch/region.txt again using the command environment, and tell me which project this is for. Do not create tasks or infrastructure.",
	}
	for i, prompt := range prompts {
		if _, err := s.EnqueueOperator(ctx, c, fmt.Sprintf("live_harness_%d", i), prompt, config.Model, nil); err != nil {
			t.Fatal(err)
		}
		run, ok, err := s.claimOperator(ctx)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if err = runtime.work(ctx, &run); err != nil {
			t.Fatal(err)
		}
		var commands int
		if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_tool_calls WHERE run_id=$1 AND name='canter_bash' AND result->>'exitCode'='0'`, run.ID).Scan(&commands); err != nil || commands == 0 {
			t.Fatalf("model did not execute a successful command: %v", err)
		}
	}
	notes, err := s.operatorNotes(ctx, c.ID)
	if err != nil || !strings.Contains(strings.ToLower(string(notes)), "flight") {
		t.Fatalf("model did not persist project context: %s %v", notes, err)
	}
	var scratch map[string]string
	if err = s.pool.QueryRow(ctx, `SELECT scratch FROM operator_working_context WHERE conversation_id=$1`, c.ID).Scan(&scratch); err != nil || strings.TrimSpace(scratch["/scratch/region.txt"]) != "Toronto" {
		t.Fatalf("live command state did not survive: %v", err)
	}
	messages, err := s.OperatorMessages(ctx, c.ID)
	if err != nil || !strings.Contains(strings.ToLower(messages[len(messages)-1].Content), "flight") {
		t.Fatalf("follow-up lost project identity: %v", err)
	}
	t.Logf("verified model %s across two turns: context saved, commands executed, files resumed", config.Model)
}
