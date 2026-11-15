package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/canter0/canter/pricing"
)

type ciManagedRunnerMachine struct {
	SizeID    string `json:"sizeId"`
	FlavorID  string `json:"flavorId"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memoryMiB"`
	DiskGiB   int    `json:"diskGiB"`
}

// This manifest binds the infrastructure authorization to the exact machine
// catalog, boot transport, official runner archive, and embedded guest tools.
// It is not an invoice, funding authorization, or customer billing policy.
type ciManagedRunnerProfile struct {
	Version           int                      `json:"version"`
	Region            string                   `json:"region"`
	ImageID           string                   `json:"imageId"`
	NetworkID         string                   `json:"networkId"`
	BootstrapURL      string                   `json:"bootstrapUrl"`
	RunnerVersion     string                   `json:"runnerVersion"`
	RunnerSHA256      string                   `json:"runnerSha256"`
	GuestBundleSHA256 string                   `json:"guestBundleSha256"`
	Machines          []ciManagedRunnerMachine `json:"machines"`
}

var ciManagedRunnerVersion = regexp.MustCompile(`^[1-9][0-9]{0,3}\.(0|[1-9][0-9]{0,3})\.(0|[1-9][0-9]{0,3})$`)

func (p ciManagedRunnerProfile) normalized() ciManagedRunnerProfile {
	p.Machines = append([]ciManagedRunnerMachine(nil), p.Machines...)
	sort.Slice(p.Machines, func(i, j int) bool { return p.Machines[i].SizeID < p.Machines[j].SizeID })
	return p
}

func (p ciManagedRunnerProfile) valid() bool {
	if p.Version != 1 || !ciProvisioningID.MatchString(p.Region) ||
		!ciProvisioningID.MatchString(p.ImageID) || !ciProvisioningID.MatchString(p.NetworkID) ||
		!ciManagedRunnerVersion.MatchString(p.RunnerVersion) ||
		!ciProvisioningDigest.MatchString(p.RunnerSHA256) ||
		!ciProvisioningDigest.MatchString(p.GuestBundleSHA256) ||
		len(p.Machines) < 1 || len(p.Machines) > 2 {
		return false
	}
	if strings.ContainsAny(p.BootstrapURL, "?#") {
		return false
	}
	u, err := url.Parse(p.BootstrapURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawPath != "" || u.Path != "/v1/ci/runner-bootstrap" {
		return false
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	sizes, flavors := map[string]bool{}, map[string]bool{}
	for _, m := range p.Machines {
		size, ok := pricing.FindCI(m.SizeID)
		if !ok || size.ID != m.SizeID || sizes[m.SizeID] || flavors[m.FlavorID] ||
			!ciProvisioningID.MatchString(m.FlavorID) ||
			(m.VCPUs != 2 && m.VCPUs != 4) || m.VCPUs != size.VCPUs ||
			m.MemoryMiB != size.MemoryMiB || m.MemoryMiB != m.VCPUs*4096 ||
			m.DiskGiB < 1 || m.DiskGiB > 160 {
			return false
		}
		sizes[m.SizeID], flavors[m.FlavorID] = true, true
	}
	return true
}

func (p ciManagedRunnerProfile) digest() (string, error) {
	if !p.valid() {
		return "", ErrConflict
	}
	raw, err := json.Marshal(p.normalized())
	if err != nil {
		return "", ErrConflict
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (p ciManagedRunnerProfile) machineFor(spec ciEnforcerSpec) (ciManagedRunnerMachine, error) {
	if !p.valid() || !spec.valid() || spec.Plan.Region != p.Region ||
		spec.Plan.ImageID != p.ImageID || spec.Plan.NetworkID != p.NetworkID {
		return ciManagedRunnerMachine{}, ErrConflict
	}
	for _, m := range p.Machines {
		if m.SizeID == spec.SizeID && m.FlavorID == spec.Plan.FlavorID {
			return m, nil
		}
	}
	return ciManagedRunnerMachine{}, ErrConflict
}

func (p ciManagedRunnerProfile) planFor(sizeID, operationID, leaseID string, now time.Time) (ciRunnerLifecyclePlan, error) {
	if !p.valid() || !ciProvisioningID.MatchString(operationID) ||
		!ciLifecycleLeaseID.MatchString(leaseID) {
		return ciRunnerLifecyclePlan{}, ErrConflict
	}
	now = now.UTC().Truncate(time.Microsecond)
	for _, m := range p.Machines {
		if m.SizeID != sizeID {
			continue
		}
		plan := ciRunnerLifecyclePlan{
			OperationID: operationID, ServerName: ciEnforcerRunnerName(operationID),
			Region: p.Region, ImageID: p.ImageID, NetworkID: p.NetworkID, FlavorID: m.FlavorID,
			EnforcerLease: leaseID, SetupDeadlineAt: now.Add(ciLifecycleSetupWindow),
			HardDeadlineAt: now.Add(ciLifecycleHardWindow),
		}
		if !plan.newPlanValid(now) {
			return ciRunnerLifecyclePlan{}, ErrConflict
		}
		return plan, nil
	}
	return ciRunnerLifecyclePlan{}, ErrConflict
}

func (p ciManagedRunnerProfile) runnerArchiveURL() (string, error) {
	if !p.valid() {
		return "", ErrConflict
	}
	return "https://github.com/actions/runner/releases/download/v" + p.RunnerVersion +
		"/actions-runner-linux-x64-" + p.RunnerVersion + ".tar.gz", nil
}
