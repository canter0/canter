package controlplane

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestCIEvidenceHTTPPythonPoster proves the guest poster wire end to end:
// a committed managed admission's evidence header token, delivered to
// `python3 -I canter-ci-evidence.py post` only through an anonymous pipe
// and the request body only through stdin, must produce HTTP 204 against
// the real store/router and persist guest/workflow rows. Purpose-mismatched
// or foreign credentials exit 65 and write nothing.
func TestCIEvidenceHTTPPythonPoster(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	srv := fx.evidenceServer(t)
	defer srv.Close()

	w := fx.post(t, fx.req, fx.capability(t, 3600, nil), nil)
	if w.Code != 200 {
		t.Fatalf("admission: %d %s", w.Code, w.Body.String())
	}
	header := w.Header().Get(ciEvidenceHeader)
	if header == "" {
		t.Fatal("managed admission missing evidence header")
	}
	capability, err := parseCIEvidenceCapability(fx.signingKey, header, fx.s.now())
	if err != nil {
		t.Fatalf("header parse: %v", err)
	}
	scope, err := fx.s.loadCIEvidenceScope(context.Background(), capability)
	if err != nil {
		t.Fatal(err)
	}
	job := scope.Job

	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "canter-ci-evidence.py"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("evidence script: %v", err)
	}
	endpoint := srv.URL + "/v1/ci/runner-evidence"

	post := func(token string, body []byte) (int, string, string) {
		readFD, writeFD, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writeFD.WriteString(token); err != nil {
			t.Fatal(err)
		}
		writeFD.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "python3", "-I", script, "post",
			"--endpoint", endpoint, "--token-fd", "3",
			"--allow-loopback-http")
		cmd.ExtraFiles = []*os.File{readFD}
		cmd.Stdin = bytes.NewReader(body)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		// Run starts synchronously and always joins the child; the context
		// deadline kills and reaps it on timeout with no orphan worker.
		err = cmd.Run()
		readFD.Close()
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode(), stdout.String(), stderr.String()
			}
			t.Fatalf("poster run: %v", err)
		}
		return 0, stdout.String(), stderr.String()
	}

	body := ciEvidenceValidBody(t, fx)
	code, out, errOut := post(header, body)
	if code != 0 {
		t.Fatalf("valid poster exit %d stderr %q", code, errOut)
	}
	if bytes.Contains([]byte(out+errOut), []byte(header[:16])) {
		t.Fatal("poster echoed credential material")
	}
	metrics, err := fx.s.CIJobMetrics(context.Background(), fx.workspace, job)
	if err != nil || len(metrics) != 1 || metrics[0].Source != "guest" {
		t.Fatalf("metric rows: %v %+v", err, metrics)
	}
	tests, err := fx.s.CIJobTests(context.Background(), fx.workspace, job)
	if err != nil || len(tests) != 1 || tests[0].Source != "workflow" ||
		tests[0].DurationMs != nil {
		t.Fatalf("test rows: %v %+v", err, tests)
	}

	// Purpose confusion: the admission capability is not an evidence token.
	admissionTok := fx.capability(t, 3600, nil)
	code, _, _ = post(admissionTok, ciEvidenceValidBody(t, fx))
	if code != 65 {
		t.Fatalf("admission-token poster exit %d, want 65", code)
	}
	// Foreign workspace claims inside a correctly-shaped token.
	foreign := ciEvidenceTestClaims(fx.s.now())
	foreign.Workspace = "ws_other"
	foreignTok, err := mintCIEvidenceCapability(fx.signingKey, foreign, fx.s.now())
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ = post(foreignTok, ciEvidenceValidBody(t, fx))
	if code != 65 {
		t.Fatalf("foreign-token poster exit %d, want 65", code)
	}
	metrics, err = fx.s.CIJobMetrics(context.Background(), fx.workspace, job)
	if err != nil || len(metrics) != 1 {
		t.Fatalf("negative posts wrote rows: %v %+v", err, metrics)
	}
	tests, err = fx.s.CIJobTests(context.Background(), fx.workspace, job)
	if err != nil || len(tests) != 1 {
		t.Fatalf("negative posts wrote rows: %v %+v", err, tests)
	}
}
