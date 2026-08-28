package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestContractIsExactAndCheckedIn(t *testing.T) {
	want, err := BuildSystem()
	if err != nil {
		t.Fatal(err)
	}
	got, err := sdk.LoadSystem("system.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("system.yaml drifted from BuildSystem()\ngot:  %#v\nwant: %#v", got, want)
	}

	host := got.Spec.Constraints.Host
	if host.Class != "c1" || host.Count != 1 || host.MemoryMiB != 1024 || host.SystemReserve != 384 {
		t.Fatalf("unexpected host contract: %+v", host)
	}
	if len(got.Spec.Services) != 1 {
		t.Fatalf("got %d services, want one logical service", len(got.Spec.Services))
	}
	service := got.Spec.Services[0]
	if service.Engine != "mysql" || service.Isolation != "firecracker" || service.Instances != 2 || service.Resources.MemoryMiB != 250 || service.Resources.VCPU != 1 {
		t.Fatalf("unexpected MySQL contract: %+v", service)
	}
}

func TestCompiledGraphPreservesPlacementCapacityAndKVM(t *testing.T) {
	system, err := BuildSystem()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := sdk.CompileSystem(system)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Capacity.HostMemoryMiB != 1024 || graph.Capacity.SystemReserveMiB != 384 || graph.Capacity.GuestMemoryMiB != 500 || graph.Capacity.UnallocatedMemory != 140 {
		t.Fatalf("unexpected capacity: %+v", graph.Capacity)
	}

	guests := map[string]bool{}
	databases := map[string]bool{}
	for _, node := range graph.Nodes {
		switch node.Kind {
		case "runtime.microvm":
			if node.Placement != "compute/host-1" || node.Resources.MemoryMiB != 250 || node.Resources.VCPU != 1 || node.Properties["isolation"] != "firecracker" {
				t.Fatalf("bad microVM node: %+v", node)
			}
			guests[node.ID] = true
		case "database.mysql":
			if node.Properties["readiness"] != "mysql:3306" || node.Properties["networking"] != "isolated-tap" {
				t.Fatalf("bad database node: %+v", node)
			}
			databases[node.ID] = true
		}
	}
	if len(guests) != 2 || !guests["microvm/mysql-1"] || !guests["microvm/mysql-2"] {
		t.Fatalf("unexpected guests: %v", guests)
	}
	if len(databases) != 2 {
		t.Fatalf("got %d database instances, want 2", len(databases))
	}
	if len(graph.Invariants) == 0 || graph.Invariants[0] != (sdk.Invariant{Kind: "host.capability", Subject: "compute/host-1", Value: "kvm"}) {
		t.Fatalf("missing KVM invariant: %+v", graph.Invariants)
	}
}

func TestRendererMatchesCheckedInSandbox(t *testing.T) {
	want, err := RenderSandbox()
	if err != nil {
		t.Fatal(err)
	}
	got, err := sdk.LoadSpec("canter.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("canter.yaml drifted; rerun the renderer")
	}
	if got.Spec.Compute.Class != "c1" || got.Spec.Compute.Replicas != 1 || got.Spec.Policy.MaxReplicas != 1 {
		t.Fatalf("rendered Sandbox can allocate more than the one contracted c1 host: %+v", got.Spec)
	}
}

func TestBootstrapHasExactFirecrackerGuestsAndNetworks(t *testing.T) {
	bootstrap := mustBootstrap(t)
	for _, fragment := range []string{
		"HOST_MEMORY_MIB=1024",
		"HOST_RESERVE_MIB=384",
		"GUEST_COUNT=2",
		"GUEST_MEMORY_MIB=250",
		"GUEST_VCPU=1",
		`"vcpu_count": ${GUEST_VCPU}`,
		`"mem_size_mib": ${GUEST_MEMORY_MIB}`,
		"write_config 1 tap-mysql-1 10.200.1.2 06:00:ac:10:00:01",
		"write_config 2 tap-mysql-2 10.200.2.2 06:00:ac:10:00:02",
		`mysql-1.ext4`,
		`mysql-2.ext4`,
	} {
		if !strings.Contains(bootstrap, fragment) {
			t.Errorf("bootstrap is missing %q", fragment)
		}
	}
	lower := strings.ToLower(bootstrap)
	if strings.Contains(lower, "apt-get install mariadb") || strings.Contains(lower, "mariadbd") || strings.Contains(lower, "qemu") || strings.Contains(lower, "docker") {
		t.Fatal("bootstrap contains a forbidden substitute runtime or database")
	}
}

func TestBootstrapFailsClosedOnKVMBeforeArtifacts(t *testing.T) {
	bootstrap := mustBootstrap(t)
	gate := `if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then`
	gateAt := strings.Index(bootstrap, gate)
	downloadAt := strings.Index(bootstrap, "curl --fail --location")
	if gateAt < 0 || downloadAt < 0 || gateAt > downloadAt {
		t.Fatalf("read/write KVM gate must precede artifact downloads (gate=%d download=%d)", gateAt, downloadAt)
	}
	for _, fragment := range []string{"nested KVM unavailable", "kvm-ok", "SHA-256 mismatch", "expected exactly two live Firecracker processes"} {
		if !strings.Contains(bootstrap, fragment) {
			t.Errorf("bootstrap is missing fail-closed check %q", fragment)
		}
	}
}

func TestBootstrapExecutesTwoRealSQLReadinessChecks(t *testing.T) {
	bootstrap := mustBootstrap(t)
	if !strings.Contains(bootstrap, "mysql --protocol=TCP") || !strings.Contains(bootstrap, "--execute='SELECT 1'") {
		t.Fatal("readiness is not implemented with a real host-side MySQL SELECT 1")
	}
	calls := regexp.MustCompile(`(?m)^mysql_[12]_result=\$\(sql_ready mysql-[12] 10\.200\.[12]\.2\)$`).FindAllString(bootstrap, -1)
	if len(calls) != 2 {
		t.Fatalf("got %d explicit SQL readiness checks, want 2: %q", len(calls), calls)
	}
	if calls[0] == calls[1] {
		t.Fatal("SQL readiness checks do not target distinct guests")
	}
}

func TestPinnedArtifactDigestsAreSHA256(t *testing.T) {
	for name, digest := range map[string]string{
		"firecracker": firecrackerSHA256,
		"rootfs":      rootfsSHA256,
	} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != 32 {
			t.Errorf("%s digest is not SHA-256: %q", name, digest)
		}
	}
}

func TestBootstrapIsPOSIXShellSyntax(t *testing.T) {
	bootstrap := mustBootstrap(t)
	cmd := exec.Command("/bin/sh", "-n")
	cmd.Stdin = strings.NewReader(bootstrap)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n failed: %v\n%s", err, output)
	}
}

func TestCommandsAreDeterministic(t *testing.T) {
	var first, second bytes.Buffer
	old := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	if err := run([]string{"compile"}); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	_, _ = first.ReadFrom(read)
	os.Stdout = old

	read, write, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	if err := run([]string{"compile"}); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	_, _ = second.ReadFrom(read)
	os.Stdout = old
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("compile output is not deterministic")
	}
}

func mustBootstrap(t *testing.T) string {
	t.Helper()
	spec, err := RenderSandbox()
	if err != nil {
		t.Fatal(err)
	}
	return spec.Spec.Compute.Bootstrap
}
