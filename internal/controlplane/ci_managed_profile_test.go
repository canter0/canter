package controlplane

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func ciManagedProfileFixture() ciManagedRunnerProfile {
	return ciManagedRunnerProfile{
		Version: 1, Region: "r1", ImageID: "i1", NetworkID: "n1",
		BootstrapURL:  "https://ci.example.test/v1/ci/runner-bootstrap",
		RunnerVersion: "2.330.0", RunnerSHA256: strings.Repeat("a", 64),
		GuestBundleSHA256: strings.Repeat("b", 64),
		Machines: []ciManagedRunnerMachine{
			{SizeID: "linux-x64-2", FlavorID: "f2", VCPUs: 2, MemoryMiB: 8192, DiskGiB: 80},
			{SizeID: "linux-x64-4", FlavorID: "f4", VCPUs: 4, MemoryMiB: 16384, DiskGiB: 160},
		},
	}
}

func TestCIManagedRunnerProfile(t *testing.T) {
	t.Run("canonical authorization identity", func(t *testing.T) {
		p := ciManagedProfileFixture()
		// Independently computed from the frozen canonical JSON fixture.
		const want = "c6051851dec2bb8ee2fb1f8076e017d66107c70f5ea957caae238c9813a4830f"
		got, err := p.digest()
		if err != nil || got != want {
			t.Fatalf("profile digest: %s error=%v", got, err)
		}
		p.Machines[0], p.Machines[1] = p.Machines[1], p.Machines[0]
		reordered, err := p.digest()
		if err != nil || reordered != want {
			t.Fatalf("machine order changed authorization: %s error=%v", reordered, err)
		}
		if p.Machines[0].SizeID != "linux-x64-4" {
			t.Fatal("digest mutated caller-owned machine slice")
		}
	})

	t.Run("all allocation and bootstrap fields bind authorization", func(t *testing.T) {
		baseline, _ := ciManagedProfileFixture().digest()
		changes := map[string]func(*ciManagedRunnerProfile){
			"region":       func(p *ciManagedRunnerProfile) { p.Region = "r2" },
			"image":        func(p *ciManagedRunnerProfile) { p.ImageID = "i2" },
			"network":      func(p *ciManagedRunnerProfile) { p.NetworkID = "n2" },
			"endpoint":     func(p *ciManagedRunnerProfile) { p.BootstrapURL = "https://other.example.test/v1/ci/runner-bootstrap" },
			"runner":       func(p *ciManagedRunnerProfile) { p.RunnerVersion = "2.330.1" },
			"runner hash":  func(p *ciManagedRunnerProfile) { p.RunnerSHA256 = strings.Repeat("c", 64) },
			"guest bundle": func(p *ciManagedRunnerProfile) { p.GuestBundleSHA256 = strings.Repeat("d", 64) },
			"flavor":       func(p *ciManagedRunnerProfile) { p.Machines[0].FlavorID = "f3" },
			"disk":         func(p *ciManagedRunnerProfile) { p.Machines[0].DiskGiB = 81 },
			"catalog":      func(p *ciManagedRunnerProfile) { p.Machines = p.Machines[:1] },
		}
		for name, change := range changes {
			p := ciManagedProfileFixture()
			change(&p)
			got, err := p.digest()
			if err != nil || got == baseline {
				t.Fatalf("%s did not produce a distinct valid authorization: %s error=%v", name, got, err)
			}
		}
	})

	t.Run("invalid configuration fails before allocation", func(t *testing.T) {
		invalid := map[string]func(*ciManagedRunnerProfile){
			"schema":          func(p *ciManagedRunnerProfile) { p.Version = 0 },
			"region":          func(p *ciManagedRunnerProfile) { p.Region = "../r1" },
			"image":           func(p *ciManagedRunnerProfile) { p.ImageID = "" },
			"network":         func(p *ciManagedRunnerProfile) { p.NetworkID = "n/1" },
			"unhashed runner": func(p *ciManagedRunnerProfile) { p.RunnerSHA256 = "" },
			"unhashed guest":  func(p *ciManagedRunnerProfile) { p.GuestBundleSHA256 = "" },
			"runner version":  func(p *ciManagedRunnerProfile) { p.RunnerVersion = "2.330.0/other" },
			"zero version":    func(p *ciManagedRunnerProfile) { p.RunnerVersion = "0.0.0" },
			"empty catalog":   func(p *ciManagedRunnerProfile) { p.Machines = nil },
			"duplicate size":  func(p *ciManagedRunnerProfile) { p.Machines[1] = p.Machines[0] },
			"duplicate flavor": func(p *ciManagedRunnerProfile) {
				p.Machines[1].FlavorID = p.Machines[0].FlavorID
			},
			"label alias": func(p *ciManagedRunnerProfile) { p.Machines[0].SizeID = "canter-2vcpu-ubuntu-2404" },
			"larger cpu":  func(p *ciManagedRunnerProfile) { p.Machines[1].VCPUs = 8 },
			"larger ram":  func(p *ciManagedRunnerProfile) { p.Machines[1].MemoryMiB = 32768 },
			"wrong ram":   func(p *ciManagedRunnerProfile) { p.Machines[0].MemoryMiB = 4096 },
			"zero disk":   func(p *ciManagedRunnerProfile) { p.Machines[0].DiskGiB = 0 },
			"larger disk": func(p *ciManagedRunnerProfile) { p.Machines[1].DiskGiB = 161 },
		}
		for name, change := range invalid {
			p := ciManagedProfileFixture()
			change(&p)
			if _, err := p.digest(); !errors.Is(err, ErrConflict) {
				t.Fatalf("%s accepted: %v", name, err)
			}
		}
		for _, endpoint := range []string{
			"http://ci.example.test/v1/ci/runner-bootstrap",
			"https://user:pass@ci.example.test/v1/ci/runner-bootstrap",
			"https://ci.example.test/v1/ci/runner-bootstrap?x=1",
			"https://ci.example.test/v1/ci/runner-bootstrap?",
			"https://ci.example.test/v1/ci/runner-bootstrap#fragment",
			"https://ci.example.test/v1/ci/runner-bootstrap#",
			"https://ci.example.test/v1/ci/runner-admissions",
			"https://ci.example.test/v1/ci/%72unner-bootstrap",
			"https://ci.example.test:0/v1/ci/runner-bootstrap",
			"https://ci.example.test:65536/v1/ci/runner-bootstrap",
		} {
			p := ciManagedProfileFixture()
			p.BootstrapURL = endpoint
			if _, err := p.digest(); !errors.Is(err, ErrConflict) {
				t.Fatalf("unsafe endpoint accepted: %v", err)
			}
		}
	})

	t.Run("exact plan and machine scope", func(t *testing.T) {
		p := ciManagedProfileFixture()
		now := time.Date(2026, time.November, 11, 12, 0, 0, 123456789, time.UTC)
		normalized := now.Truncate(time.Microsecond)
		plan, err := p.planFor("linux-x64-2", "op_profile", strings.Repeat("e", 64), now)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Region != "r1" || plan.ImageID != "i1" || plan.NetworkID != "n1" || plan.FlavorID != "f2" ||
			plan.ServerName != ciEnforcerRunnerName("op_profile") ||
			!plan.SetupDeadlineAt.Equal(normalized.Add(900*time.Second)) ||
			!plan.HardDeadlineAt.Equal(normalized.Add(22500*time.Second)) {
			t.Fatalf("unexpected plan: %+v", plan)
		}
		spec := ciEnforcerSpec{
			Plan: plan, GitHubInstallationID: 123, GitHubRepositoryID: 789, SizeID: "linux-x64-2",
		}
		m, err := p.machineFor(spec)
		if err != nil || m.VCPUs != 2 || m.MemoryMiB != 8192 || m.DiskGiB != 80 {
			t.Fatalf("machine: %+v error=%v", m, err)
		}
		for name, change := range map[string]func(*ciEnforcerSpec){
			"region":       func(s *ciEnforcerSpec) { s.Plan.Region = "r2" },
			"image":        func(s *ciEnforcerSpec) { s.Plan.ImageID = "i2" },
			"network":      func(s *ciEnforcerSpec) { s.Plan.NetworkID = "n2" },
			"other flavor": func(s *ciEnforcerSpec) { s.Plan.FlavorID = "f4" },
			"other size":   func(s *ciEnforcerSpec) { s.SizeID = "linux-x64-4" },
			"scope":        func(s *ciEnforcerSpec) { s.GitHubRepositoryID = 0 },
		} {
			drift := spec
			change(&drift)
			if _, err := p.machineFor(drift); !errors.Is(err, ErrConflict) {
				t.Fatalf("%s accepted: %v", name, err)
			}
		}
		for _, size := range []string{"linux-x64-8", "canter-2vcpu-ubuntu-2404"} {
			if _, err := p.planFor(size, "op_profile", strings.Repeat("e", 64), now); !errors.Is(err, ErrConflict) {
				t.Fatalf("unknown size accepted: %v", err)
			}
		}
		url, err := p.runnerArchiveURL()
		if err != nil || url != "https://github.com/actions/runner/releases/download/v2.330.0/actions-runner-linux-x64-2.330.0.tar.gz" {
			t.Fatalf("archive URL changed: %s error=%v", url, err)
		}
	})
}
