package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	ciassets "github.com/canter0/canter/scripts"
)

const ciGuestUserDataMaxBytes = 49149

type ciGuestBundle struct {
	Compressed []byte
	SHA256     string
	Sources    map[string][]byte
}

func newCIGuestBundle() (ciGuestBundle, error) {
	files, err := ciassets.Sources()
	if err != nil || len(files) != 7 {
		return ciGuestBundle{}, ErrConflict
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var out bytes.Buffer
	gz, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return ciGuestBundle{}, ErrConflict
	}
	gz.Header.OS = 255
	gz.Header.ModTime = time.Time{}
	tw := tar.NewWriter(gz)
	total := 0
	for _, name := range names {
		raw := files[name]
		total += len(raw)
		if len(raw) == 0 || len(raw) > 256*1024 || total > 1024*1024 {
			return ciGuestBundle{}, ErrConflict
		}
		hdr := &tar.Header{
			Name: name, Mode: 0o555, Size: int64(len(raw)), Typeflag: tar.TypeReg,
			Uid: 0, Gid: 0, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return ciGuestBundle{}, ErrConflict
		}
		if _, err := tw.Write(raw); err != nil {
			return ciGuestBundle{}, ErrConflict
		}
	}
	if tw.Close() != nil || gz.Close() != nil {
		return ciGuestBundle{}, ErrConflict
	}
	blob := out.Bytes()
	sum := sha256.Sum256(blob)
	return ciGuestBundle{
		Compressed: append([]byte(nil), blob...), SHA256: hex.EncodeToString(sum[:]), Sources: files,
	}, nil
}

type ciMachineRuntimeConfig struct {
	Version             int      `json:"version"`
	BundleBase64        string   `json:"bundleBase64"`
	BundleSHA256        string   `json:"bundleSha256"`
	RunnerURL           string   `json:"runnerUrl"`
	RunnerSHA256        string   `json:"runnerSha256"`
	GuestArguments      []string `json:"guestArguments"`
	BootstrapToken      string   `json:"bootstrapToken"`
	SetupDeadlineUnixMs int64    `json:"setupDeadlineUnixMs"`
	HardDeadlineUnixMs  int64    `json:"hardDeadlineUnixMs"`
}

type ciGuestCloudUser struct {
	Name       string   `json:"name"`
	Shell      string   `json:"shell"`
	LockPasswd bool     `json:"lock_passwd"`
	Groups     []string `json:"groups"`
	Sudo       bool     `json:"sudo"`
	HomeDir    string   `json:"homedir"`
}

type ciGuestCloudFile struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Owner       string `json:"owner"`
	Permissions string `json:"permissions"`
}

type ciGuestCloudInit struct {
	SSHPWAUTH     bool               `json:"ssh_pwauth"`
	DisableRoot   bool               `json:"disable_root"`
	PackageUpdate bool               `json:"package_update"`
	Packages      []string           `json:"packages"`
	Users         []ciGuestCloudUser `json:"users"`
	WriteFiles    []ciGuestCloudFile `json:"write_files"`
	RunCmd        [][]string         `json:"runcmd"`
}

func buildCIGuestUserData(profile ciManagedRunnerProfile, scope ciBootstrapScope, token string, now time.Time) (string, error) {
	if _, err := profile.machineFor(scope.Spec); err != nil {
		return "", err
	}
	plan, grant, snap := scope.Spec.Plan, scope.Grant, scope.Snapshot
	if !ciRunnerLifecyclePlanEqual(plan, snap.Plan) || !plan.newPlanValid(now) ||
		!ciBootstrapGrantMatches(scope) ||
		grant.MaximumSeconds < 1 || grant.MaximumSeconds > ciMaxRuntimeSeconds ||
		grant.ExpiresAt.Unix() <= now.Unix() || grant.ExpiresAt.Unix() > now.Unix()+900 ||
		grant.ExpiresAt.Unix() > plan.SetupDeadlineAt.Unix() ||
		!plan.SetupDeadlineAt.After(now) || !plan.HardDeadlineAt.After(now) ||
		grant.AttemptedAt != nil || grant.DeliveredAt != nil || snap.CreateAttemptedAt != nil ||
		snap.StopRequestedAt != nil || snap.CleanupVerifiedAt != nil ||
		snap.ExecutionStartedAt != nil || snap.ExecutionStoppedAt != nil ||
		(snap.Phase != "queued" && snap.Phase != "provisioning") ||
		len(token) == 0 || len(token) > ciBootstrapCapabilityMaxBytes {
		return "", ErrConflict
	}
	for _, ch := range token {
		if ch < 33 || ch > 126 {
			return "", ErrConflict
		}
	}
	bundle, err := newCIGuestBundle()
	if err != nil || bundle.SHA256 != profile.GuestBundleSHA256 {
		return "", ErrConflict
	}
	runnerURL, err := profile.runnerArchiveURL()
	if err != nil {
		return "", ErrConflict
	}
	args := []string{
		"--endpoint", profile.BootstrapURL,
		"--runner-dir", "/home/canter-ci/actions-runner",
		"--admission-dir", "/home/canter-ci/admission",
		"--operation-id", plan.OperationID,
		"--admission-id", grant.BootstrapID,
		"--name", plan.ServerName,
		"--github-repository-id", strconv.FormatInt(scope.Spec.GitHubRepositoryID, 10),
		"--maximum-seconds", strconv.FormatInt(grant.MaximumSeconds, 10),
		"--setup-deadline-unix-ms", strconv.FormatInt(plan.SetupDeadlineAt.UnixMilli(), 10),
		"--hard-deadline-unix-ms", strconv.FormatInt(plan.HardDeadlineAt.UnixMilli(), 10),
		"--bootstrap-expires-at", strconv.FormatInt(grant.ExpiresAt.Unix(), 10),
		"--wait-seconds", "120",
	}
	data, err := json.Marshal(ciMachineRuntimeConfig{
		Version: 1, BundleBase64: base64.StdEncoding.EncodeToString(bundle.Compressed),
		BundleSHA256: bundle.SHA256, RunnerURL: runnerURL, RunnerSHA256: profile.RunnerSHA256,
		GuestArguments: args, BootstrapToken: token,
		SetupDeadlineUnixMs: plan.SetupDeadlineAt.UnixMilli(), HardDeadlineUnixMs: plan.HardDeadlineAt.UnixMilli(),
	})
	if err != nil {
		return "", ErrConflict
	}
	literal, err := json.Marshal(string(data))
	if err != nil {
		return "", ErrConflict
	}
	template := string(bundle.Sources["canter-ci-machine-init.py"])
	const marker = `"__CANTER_CI_DATA_JSON__"`
	if strings.Count(template, marker) != 1 {
		return "", ErrConflict
	}
	machineSource := strings.Replace(template, marker, string(literal), 1)
	body, err := json.Marshal(ciGuestCloudInit{
		DisableRoot: true, PackageUpdate: true,
		Packages: []string{"ca-certificates", "curl", "git", "python3", "tar", "gzip"},
		Users: []ciGuestCloudUser{{
			Name: "canter-ci", Shell: "/bin/bash", LockPasswd: true,
			Groups: []string{}, Sudo: false, HomeDir: "/home/canter-ci",
		}},
		WriteFiles: []ciGuestCloudFile{{
			Path: "/opt/canter-ci-machine.py", Content: machineSource,
			Owner: "root:root", Permissions: "0600",
		}},
		RunCmd: [][]string{{"/usr/bin/python3", "-I", "/opt/canter-ci-machine.py", "install"}},
	})
	if err != nil {
		return "", ErrConflict
	}
	// Opaque gzip payload: the provider base64-encodes these bytes on the
	// wire and cloud-init transparently decompresses them at boot.
	plain := []byte("#cloud-config\n" + string(body))
	var packed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&packed, gzip.BestCompression)
	if err != nil {
		return "", ErrConflict
	}
	writer.Header.OS = 255
	writer.Header.ModTime = time.Time{}
	if _, err = writer.Write(plain); err != nil {
		writer.Close()
		return "", ErrConflict
	}
	if writer.Close() != nil {
		return "", ErrConflict
	}
	if packed.Len() > ciGuestUserDataMaxBytes {
		return "", ErrConflict
	}
	return packed.String(), nil
}
