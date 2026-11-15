package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ciUserDataFixture(t *testing.T) (ciManagedRunnerProfile, ciBootstrapScope, time.Time) {
	t.Helper()
	profile := ciManagedProfileFixture()
	bundle, err := newCIGuestBundle()
	if err != nil {
		t.Fatal("guest bundle unavailable")
	}
	profile.GuestBundleSHA256 = bundle.SHA256
	now := time.Date(2026, time.November, 11, 12, 0, 0, 123456000, time.UTC)
	plan, err := profile.planFor("linux-x64-2", "op_userdata", strings.Repeat("e", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	scope := ciBootstrapScope{
		Spec: ciEnforcerSpec{
			Plan: plan, GitHubInstallationID: 123, GitHubRepositoryID: 789, SizeID: "linux-x64-2",
		},
		Grant: ciBootstrapGrant{
			BootstrapID: strings.Repeat("c", 64), MaximumSeconds: 3600,
			ExpiresAt: time.Unix(plan.SetupDeadlineAt.Unix(), 0),
		},
		CreatedAt: now,
		Snapshot:  ciRunnerLifecycleSnapshot{Plan: plan, Phase: "queued"},
	}
	return profile, scope, now
}

func ciUserDataPayload(t *testing.T, source string) ciMachineRuntimeConfig {
	t.Helper()
	// Parse, never execute, the generated privileged boot program. Both
	// source and decoded capability stay in memory, not argv or artifacts.
	const extract = `import ast, json, sys
tree = ast.parse(sys.stdin.read(), filename="<ci-userdata>")
values = [n.value for n in tree.body if isinstance(n, ast.Assign)
          and any(isinstance(t, ast.Name) and t.id == "DATA_JSON" for t in n.targets)]
if len(values) != 1:
    raise ValueError("missing machine configuration")
value = ast.literal_eval(values[0])
if not isinstance(value, str):
    raise ValueError("invalid machine configuration")
data = json.loads(value)
sys.stdout.write(json.dumps(data, separators=(",", ":")))
`
	cmd := exec.Command("python3", "-I", "-c", extract)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	cmd.Stdin = strings.NewReader(source)
	cmd.Stderr = io.Discard
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("generated machine program or configuration is invalid")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 9 {
		t.Fatal("machine configuration schema drift")
	}
	var out ciMachineRuntimeConfig
	if json.Unmarshal(raw, &out) != nil {
		t.Fatal("machine configuration does not decode")
	}
	return out
}

func TestCIGuestUserDataSignedCapabilityFits(t *testing.T) {
	profile, scope, now := ciUserDataFixture(t)
	entropy := func(seed string) string {
		var value string
		for i := 0; len(value) < 128; i++ {
			sum := sha256.Sum256([]byte(seed + strconv.Itoa(i)))
			value += base64.RawURLEncoding.EncodeToString(sum[:])
		}
		return value[:128]
	}
	plan, err := profile.planFor("linux-x64-2", entropy("operation"),
		strings.Repeat("e", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	scope.Spec.Plan, scope.Snapshot.Plan = plan, plan
	scope.Spec.GitHubRepositoryID = 9223372036854775807
	scope.Grant.MaximumSeconds = ciMaxRuntimeSeconds
	scope.Snapshot.PaidOverrunEnabled = true
	claims := scope.claims(entropy("workspace"), entropy("runner"))
	key := sha256.Sum256([]byte("userdata-test-key-not-production"))
	token, err := mintCIBootstrapCapability(key[:], claims, now, plan.SetupDeadlineAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCIBootstrapCapability(key[:], token, now); err != nil {
		t.Fatal(err)
	}
	// Stress real signed claims at the safe-ASCII ID byte limits rather than
	// treating an arbitrary 4096-byte string as a mintable capability.
	payload, err := buildCIGuestUserData(profile, scope, token, now)
	if err != nil {
		t.Fatal("signed capability fixture does not fit provider userdata:", err)
	}
	if len(payload) > ciGuestUserDataMaxBytes {
		t.Fatal("userdata exceeds provider limit")
	}
}

func TestCIGuestUserData(t *testing.T) {
	t.Run("sealed deterministic artifact matches all source bytes", func(t *testing.T) {
		first, err := newCIGuestBundle()
		if err != nil {
			t.Fatal(err)
		}
		second, err := newCIGuestBundle()
		if err != nil || !bytes.Equal(first.Compressed, second.Compressed) || first.SHA256 != second.SHA256 {
			t.Fatal("guest artifact is nondeterministic")
		}
		sum := sha256.Sum256(first.Compressed)
		if first.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatal("artifact digest does not cover delivered compressed bytes")
		}
		want := map[string]bool{
			"canter-ci-runner.py": true, "canter-ci-jit-runner.py": true,
			"canter-ci-admission-client.py": true, "canter-ci-guest-bootstrap.py": true,
			"canter-ci-machine-init.py": true, "canter-ci-cache.py": true,
			"canter-ci-evidence.py": true,
		}
		gz, err := gzip.NewReader(bytes.NewReader(first.Compressed))
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		archive := tar.NewReader(gz)
		seen := map[string]bool{}
		for {
			hdr, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if !want[hdr.Name] || seen[hdr.Name] || hdr.Typeflag != tar.TypeReg ||
				hdr.Mode != 0o555 || hdr.Uid != 0 || hdr.Gid != 0 ||
				!hdr.ModTime.Equal(time.Unix(0, 0)) {
				t.Fatal("guest archive membership, type or permissions drift")
			}
			seen[hdr.Name] = true
			delivered, err := io.ReadAll(archive)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join("..", "..", "scripts", hdr.Name))
			if err != nil || !bytes.Equal(delivered, original) || !bytes.Equal(delivered, first.Sources[hdr.Name]) {
				t.Fatal("delivered guest code differs from reviewed repository source")
			}
		}
		if len(seen) != len(want) {
			t.Fatal("guest bundle omits required code")
		}
		first.Compressed[0] ^= 1
		first.Sources["canter-ci-runner.py"][0] ^= 1
		third, err := newCIGuestBundle()
		if err != nil || !bytes.Equal(third.Compressed, second.Compressed) || third.SHA256 != second.SHA256 {
			t.Fatal("caller mutation changed sealed source bytes")
		}
	})

	t.Run("private cloud init and exact runtime scope", func(t *testing.T) {
		profile, scope, now := ciUserDataFixture(t)
		const token = "v1.rootfixture.scoped-capability"
		userData, err := buildCIGuestUserData(profile, scope, token, now)
		if err != nil {
			t.Fatal("valid guest data did not materialize")
		}
		if len(userData) > 49149 || base64.StdEncoding.EncodedLen(len(userData)) > 65536 {
			t.Fatal("cloud-init encoding or Nova size limit changed")
		}
		wire, err := base64.StdEncoding.DecodeString(
			base64.StdEncoding.EncodeToString([]byte(userData)))
		if err != nil || !bytes.Equal(wire, []byte(userData)) {
			t.Fatal("provider base64 wire mapping does not preserve userdata bytes")
		}
		outerGz, err := gzip.NewReader(bytes.NewReader([]byte(userData)))
		if err != nil {
			t.Fatal("userdata is not the opaque gzip payload")
		}
		cloudText, err := io.ReadAll(io.LimitReader(outerGz, 2<<20))
		outerGz.Close()
		if err != nil || len(cloudText) == 0 || len(cloudText) >= 2<<20 {
			t.Fatal("decoded cloud-init exceeds bounded size")
		}
		pyDecode := exec.Command("python3", "-I", "-c",
			"import gzip,sys\nsys.stdout.buffer.write(gzip.decompress(sys.stdin.buffer.read()))")
		pyDecode.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
		pyDecode.Stdin = strings.NewReader(userData)
		pyDecode.Stderr = io.Discard
		pyText, err := pyDecode.Output()
		if err != nil || !bytes.Equal(pyText, cloudText) {
			t.Fatal("independent gzip decode differs")
		}
		const prefix = "#cloud-config\n"
		if !bytes.HasPrefix(cloudText, []byte(prefix)) {
			t.Fatal("decoded userdata lost the cloud-config header")
		}
		decoded := string(cloudText)
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(strings.TrimPrefix(decoded, prefix)), &fields) != nil || len(fields) != 7 {
			t.Fatal("cloud-init schema drift")
		}
		var cloud ciGuestCloudInit
		if json.Unmarshal([]byte(strings.TrimPrefix(decoded, prefix)), &cloud) != nil {
			t.Fatal("cloud-init does not decode")
		}
		if cloud.SSHPWAUTH || !cloud.DisableRoot || !cloud.PackageUpdate ||
			!reflect.DeepEqual(cloud.Packages, []string{"ca-certificates", "curl", "git", "python3", "tar", "gzip"}) {
			t.Fatal("unapproved machine login or package configuration")
		}
		if len(cloud.Users) != 1 {
			t.Fatal("unexpected guest accounts")
		}
		user := cloud.Users[0]
		if user.Name != "canter-ci" || user.Shell != "/bin/bash" || !user.LockPasswd ||
			user.Groups == nil || len(user.Groups) != 0 || user.Sudo || user.HomeDir != "/home/canter-ci" {
			t.Fatal("guest account has unapproved privileges")
		}
		if len(cloud.WriteFiles) != 1 || cloud.WriteFiles[0].Path != "/opt/canter-ci-machine.py" ||
			cloud.WriteFiles[0].Owner != "root:root" || cloud.WriteFiles[0].Permissions != "0600" ||
			!reflect.DeepEqual(cloud.RunCmd, [][]string{{"/usr/bin/python3", "-I", "/opt/canter-ci-machine.py", "install"}}) {
			t.Fatal("machine launch exposes configuration or changes execution")
		}
		payload := ciUserDataPayload(t, cloud.WriteFiles[0].Content)
		if payload.Version != 1 || payload.BootstrapToken != token ||
			payload.BundleSHA256 != profile.GuestBundleSHA256 || payload.RunnerSHA256 != profile.RunnerSHA256 ||
			payload.RunnerURL != "https://github.com/actions/runner/releases/download/v2.330.0/actions-runner-linux-x64-2.330.0.tar.gz" ||
			payload.SetupDeadlineUnixMs != scope.Spec.Plan.SetupDeadlineAt.UnixMilli() ||
			payload.HardDeadlineUnixMs != scope.Spec.Plan.HardDeadlineAt.UnixMilli() {
			t.Fatal("runtime artifact or capability scope drift")
		}
		wantArgs := []string{
			"--endpoint", "https://ci.example.test/v1/ci/runner-bootstrap",
			"--runner-dir", "/home/canter-ci/actions-runner",
			"--admission-dir", "/home/canter-ci/admission",
			"--operation-id", "op_userdata",
			"--admission-id", strings.Repeat("c", 64),
			"--name", ciEnforcerRunnerName("op_userdata"),
			"--github-repository-id", "789", "--maximum-seconds", "3600",
			"--setup-deadline-unix-ms", strconv.FormatInt(scope.Spec.Plan.SetupDeadlineAt.UnixMilli(), 10),
			"--hard-deadline-unix-ms", strconv.FormatInt(scope.Spec.Plan.HardDeadlineAt.UnixMilli(), 10),
			"--bootstrap-expires-at", strconv.FormatInt(scope.Grant.ExpiresAt.Unix(), 10),
			"--wait-seconds", "120",
		}
		if !reflect.DeepEqual(payload.GuestArguments, wantArgs) {
			t.Fatal("guest argv differs from pinned scope or carries a credential")
		}
		blob, err := base64.StdEncoding.DecodeString(payload.BundleBase64)
		if err != nil {
			t.Fatal("runtime bundle does not decode")
		}
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) != payload.BundleSHA256 {
			t.Fatal("runtime bundle is not the authorized artifact")
		}
		t.Logf("Guest bundle SHA256=%s compressed_bytes=%d userdata_bytes=%d decoded_cloud_bytes=%d",
			payload.BundleSHA256, len(blob), len(userData), len(cloudText))
		again, err := buildCIGuestUserData(profile, scope, token, now)
		if err != nil || again != userData {
			t.Fatal("cloud-init output is nondeterministic")
		}
	})

	t.Run("stale or unapproved scope never emits a capability", func(t *testing.T) {
		cases := map[string]func(*ciManagedRunnerProfile, *ciBootstrapScope, *string, time.Time){
			"wrong bundle": func(p *ciManagedRunnerProfile, _ *ciBootstrapScope, _ *string, _ time.Time) {
				p.GuestBundleSHA256 = strings.Repeat("f", 64)
			},
			"free runtime drift": func(_ *ciManagedRunnerProfile, s *ciBootstrapScope, _ *string, _ time.Time) {
				s.Grant.MaximumSeconds = 21600
			},
			"paid snapshot drift": func(_ *ciManagedRunnerProfile, s *ciBootstrapScope, _ *string, _ time.Time) {
				s.Snapshot.PaidOverrunEnabled = true
			},
			"expiry drift": func(_ *ciManagedRunnerProfile, s *ciBootstrapScope, _ *string, _ time.Time) {
				s.Grant.ExpiresAt = s.Grant.ExpiresAt.Add(time.Second)
			},
			"used": func(_ *ciManagedRunnerProfile, s *ciBootstrapScope, _ *string, now time.Time) {
				s.Grant.AttemptedAt = &now
			},
			"fenced": func(_ *ciManagedRunnerProfile, s *ciBootstrapScope, _ *string, now time.Time) {
				s.Snapshot.CreateAttemptedAt = &now
			},
			"stopped": func(_ *ciManagedRunnerProfile, s *ciBootstrapScope, _ *string, now time.Time) {
				s.Snapshot.StopRequestedAt = &now
			},
			"executing": func(_ *ciManagedRunnerProfile, s *ciBootstrapScope, _ *string, now time.Time) {
				s.Snapshot.ExecutionStartedAt = &now
			},
			"mismatched plan": func(_ *ciManagedRunnerProfile, s *ciBootstrapScope, _ *string, _ time.Time) {
				s.Snapshot.Plan.FlavorID = "other"
			},
			"empty token": func(_ *ciManagedRunnerProfile, _ *ciBootstrapScope, token *string, _ time.Time) {
				*token = ""
			},
			"whitespace token": func(_ *ciManagedRunnerProfile, _ *ciBootstrapScope, token *string, _ time.Time) {
				*token = "secret\nvalue"
			},
		}
		for name, mutate := range cases {
			profile, scope, now := ciUserDataFixture(t)
			token := "v1.fixture.scoped-capability"
			mutate(&profile, &scope, &token, now)
			body, err := buildCIGuestUserData(profile, scope, token, now)
			if !errors.Is(err, ErrConflict) || body != "" {
				t.Fatalf("%s emitted data for an unapproved scope", name)
			}
		}
		profile, scope, now := ciUserDataFixture(t)
		if body, err := buildCIGuestUserData(profile, scope, "v1.fixture.scoped-capability", now.Add(901*time.Second)); !errors.Is(err, ErrConflict) || body != "" {
			t.Fatal("expired setup emitted data")
		}
	})
}
