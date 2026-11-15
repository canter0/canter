package controlplane

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ciEvidenceTestClaims(now time.Time) ciEvidenceCapability {
	return ciEvidenceCapability{
		Version:              1,
		Workspace:            "ws_evidence",
		Runner:               "runner_evidence",
		AdmissionID:          strings.Repeat("e", 64),
		GitHubInstallationID: 123,
		GitHubRepositoryID:   789,
		GitHubRunnerID:       4242,
		GitHubJobID:          9001,
		DeadlineUnixMs:       now.Add(3600 * time.Second).UnixMilli(),
	}
}

// ciEvidenceMACForTest reproduces the wire MAC for hand-built claim JSON.
func ciEvidenceMACForTest(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ciEvidenceMACDomain + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func ciEvidenceSignRaw(key []byte, raw string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(raw))
	return ciEvidenceTokenPrefix + "." + payload + "." + ciEvidenceMACForTest(key, payload)
}

func TestCIEvidenceTokenRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte("e"), 32)
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	claims := ciEvidenceTestClaims(now)
	tok, err := mintCIEvidenceCapability(key, claims, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, ciEvidenceTokenPrefix+".") {
		t.Fatalf("token missing ce1 prefix: %.20q", tok)
	}
	if len(tok) > ciAdmissionCapabilityMaxBytes {
		t.Fatal("token exceeds capability bound")
	}
	// The wire payload must carry exactly the nine public claims — the
	// private verified marker is never serialized.
	payloadRaw, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payloadRaw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 9 {
		t.Fatalf("payload carries %d keys, want exactly 9", len(fields))
	}
	if _, ok := fields["verified"]; ok {
		t.Fatal("verified marker leaked into token payload")
	}
	got, err := parseCIEvidenceCapability(key, tok, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.verified {
		t.Fatal("parsed capability missing verified marker")
	}
	got.verified = false
	if got != claims {
		t.Fatalf("claims mismatch %+v vs %+v", got, claims)
	}
	// A capability parsed under a different purpose prefix must fail.
	if _, err := parseCIAdmissionCapability(key, tok, now); err == nil {
		t.Fatal("evidence token accepted as admission capability")
	}
}

func TestCIEvidenceTokenPurposeAndKeyIsolation(t *testing.T) {
	key := bytes.Repeat([]byte("e"), 32)
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	claims := ciEvidenceTestClaims(now)
	tok, err := mintCIEvidenceCapability(key, claims, now)
	if err != nil {
		t.Fatal(err)
	}
	// A genuine admission token is not an evidence capability.
	admissionTok, err := mintCIAdmissionCapability(key, ciAdmissionCapability{
		Version: 1, Workspace: claims.Workspace, Runner: claims.Runner,
		AdmissionID: claims.AdmissionID, GitHubRepositoryID: claims.GitHubRepositoryID,
		GitHubRunnerID: claims.GitHubRunnerID, RunnerName: "canter-x",
		MaximumSeconds: 3600, ExpiresAt: now.Add(3700 * time.Second).Unix(),
	}, now, now.Add(800*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCIEvidenceCapability(key, admissionTok, now); err == nil {
		t.Fatal("admission token accepted as evidence capability")
	}
	// Foreign key and short keys fail.
	if _, err := parseCIEvidenceCapability(bytes.Repeat([]byte("x"), 32), tok, now); err == nil {
		t.Fatal("foreign-key token accepted")
	}
	for _, badKey := range [][]byte{nil, {}, bytes.Repeat([]byte("s"), 31)} {
		if _, err := mintCIEvidenceCapability(badKey, claims, now); err == nil {
			t.Fatal("short key minted")
		}
		if _, err := parseCIEvidenceCapability(badKey, tok, now); err == nil {
			t.Fatal("short key parsed")
		}
	}
	// Tampered payload, tag, and wire shapes.
	parts := strings.Split(tok, ".")
	for _, bad := range []string{
		parts[0] + "." + parts[1] + "x." + parts[2],
		parts[0] + "." + parts[1] + "." + parts[2] + "x",
		"v1." + parts[1] + "." + parts[2],
		ciEvidenceTokenPrefix,
		ciEvidenceTokenPrefix + ".",
		ciEvidenceTokenPrefix + ".." + parts[2],
		ciEvidenceTokenPrefix + "." + parts[1] + ".!!!",
		ciEvidenceTokenPrefix + "." + parts[1] + "." + parts[2] + ".extra",
		"",
		strings.Repeat("A", ciAdmissionCapabilityMaxBytes+1),
	} {
		if _, err := parseCIEvidenceCapability(key, bad, now); err == nil {
			t.Fatalf("malformed token accepted: %.40q", bad)
		}
	}
}

func TestCIEvidenceTokenDeadlineBounds(t *testing.T) {
	key := bytes.Repeat([]byte("e"), 32)
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	claims := ciEvidenceTestClaims(now)

	// Valid through deadline+grace-1ms; expired exactly at deadline+grace.
	deadline := time.UnixMilli(claims.DeadlineUnixMs)
	graceEnd := deadline.Add(ciEvidenceUploadGrace)
	tok, err := mintCIEvidenceCapability(key, claims, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCIEvidenceCapability(key, tok, graceEnd.Add(-time.Millisecond)); err != nil {
		t.Fatalf("token inside grace rejected: %v", err)
	}
	if _, err := parseCIEvidenceCapability(key, tok, graceEnd); err == nil {
		t.Fatal("token accepted at exact grace boundary")
	}
	if _, err := parseCIEvidenceCapability(key, tok, graceEnd.Add(time.Millisecond)); err == nil {
		t.Fatal("token accepted past grace")
	}

	// A deadline beyond now+lifecycle hard window cannot be minted.
	far := claims
	far.DeadlineUnixMs = now.Add(ciLifecycleHardWindow).UnixMilli() + 1
	if _, err := mintCIEvidenceCapability(key, far, now); err == nil {
		t.Fatal("over-horizon deadline minted")
	}
	// Hand-signed token bypassing mint still fails the same bound on parse.
	handFar := fmt.Sprintf(`{"version":1,"workspace":%q,"runner":%q,"admissionId":%q,"githubInstallationId":%d,"githubRepositoryId":%d,"githubRunnerId":%d,"githubJobId":%d,"deadlineUnixMs":%d}`,
		claims.Workspace, claims.Runner, claims.AdmissionID, claims.GitHubInstallationID,
		claims.GitHubRepositoryID, claims.GitHubRunnerID, claims.GitHubJobID, far.DeadlineUnixMs)
	if _, err := parseCIEvidenceCapability(key, ciEvidenceSignRaw(key, handFar), now); err == nil {
		t.Fatal("over-horizon hand-signed token accepted")
	}
	// Exactly at the hard-window bound is allowed.
	atBound := claims
	atBound.DeadlineUnixMs = now.Add(ciLifecycleHardWindow).UnixMilli()
	boundTok, err := mintCIEvidenceCapability(key, atBound, now)
	if err != nil {
		t.Fatalf("hard-window bound rejected: %v", err)
	}
	if _, err := parseCIEvidenceCapability(key, boundTok, now); err != nil {
		t.Fatalf("hard-window bound parse: %v", err)
	}
}

func TestCIEvidenceTokenStrictClaims(t *testing.T) {
	key := bytes.Repeat([]byte("e"), 32)
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	claims := ciEvidenceTestClaims(now)
	base := func() string {
		return fmt.Sprintf(`{"version":1,"workspace":%q,"runner":%q,"admissionId":%q,"githubInstallationId":%d,"githubRepositoryId":%d,"githubRunnerId":%d,"githubJobId":%d,"deadlineUnixMs":%d}`,
			claims.Workspace, claims.Runner, claims.AdmissionID, claims.GitHubInstallationID,
			claims.GitHubRepositoryID, claims.GitHubRunnerID, claims.GitHubJobID, claims.DeadlineUnixMs)
	}
	badJSON := []string{
		// Wrong or missing version.
		strings.Replace(base(), `"version":1`, `"version":2`, 1),
		strings.Replace(base(), `"version":1`, `"Version":1`, 1),
		strings.Replace(base(), `"version":1,`, ``, 1),
		// Unknown, duplicate, case-alias, and null fields.
		strings.Replace(base(), `}`, `,"extra":1}`, 1),
		strings.Replace(base(), `"workspace":`, `"Workspace":`, 1),
		strings.Replace(base(), `"githubJobId":`, `"githubjobid":`, 1),
		strings.Replace(base(), `}`, `,"workspace":"ws_evidence"}`, 1),
		strings.Replace(base(), `"admissionId":"`+claims.AdmissionID+`"`, `"admissionId":null`, 1),
		strings.Replace(base(), `"deadlineUnixMs":`+fmt.Sprint(claims.DeadlineUnixMs), `"deadlineUnixMs":null`, 1),
		// Missing mandatory keys.
		strings.Replace(base(), `"runner":"runner_evidence",`, ``, 1),
		strings.Replace(base(), `,"deadlineUnixMs":`+fmt.Sprint(claims.DeadlineUnixMs), ``, 1),
		// Mistyped values.
		strings.Replace(base(), `"githubJobId":9001`, `"githubJobId":"9001"`, 1),
		strings.Replace(base(), `"githubJobId":9001`, `"githubJobId":0`, 1),
		strings.Replace(base(), `"githubJobId":9001`, `"githubJobId":-1`, 1),
		strings.Replace(base(), `"githubRepositoryId":789`, `"githubRepositoryId":0`, 1),
		strings.Replace(base(), `"deadlineUnixMs":`+fmt.Sprint(claims.DeadlineUnixMs), `"deadlineUnixMs":0`, 1),
		strings.Replace(base(), `"workspace":"ws_evidence"`, `"workspace":"bad workspace!"`, 1),
		strings.Replace(base(), `"admissionId":"`+claims.AdmissionID+`"`, `"admissionId":"short"`, 1),
		// Trailing data and non-object roots.
		base() + ` {}`,
		`[1,2,3]`,
		`null`,
		`{"version":1}`,
	}
	for _, raw := range badJSON {
		if _, err := parseCIEvidenceCapability(key, ciEvidenceSignRaw(key, raw), now); err == nil {
			t.Fatalf("invalid claims accepted: %.100s", raw)
		}
	}
	// Mint-side validation rejects the same invalid claims.
	for _, mutate := range []func(*ciEvidenceCapability){
		func(c *ciEvidenceCapability) { c.Version = 2 },
		func(c *ciEvidenceCapability) { c.Workspace = "" },
		func(c *ciEvidenceCapability) { c.Runner = "bad runner!" },
		func(c *ciEvidenceCapability) { c.AdmissionID = "short" },
		func(c *ciEvidenceCapability) { c.GitHubInstallationID = 0 },
		func(c *ciEvidenceCapability) { c.GitHubRepositoryID = -1 },
		func(c *ciEvidenceCapability) { c.GitHubRunnerID = 0 },
		func(c *ciEvidenceCapability) { c.GitHubJobID = 0 },
		func(c *ciEvidenceCapability) { c.DeadlineUnixMs = 0 },
	} {
		bad := claims
		mutate(&bad)
		if _, err := mintCIEvidenceCapability(key, bad, now); err == nil {
			t.Fatalf("invalid claims minted: %+v", bad)
		}
	}
}
