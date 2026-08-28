package main

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"

	"github.com/canter0/canter/sdk"
	"gopkg.in/yaml.v3"
)

const (
	firecrackerVersion = "1.16.1"
	firecrackerURL     = "https://github.com/firecracker-microvm/firecracker/releases/download/v1.16.1/firecracker-v1.16.1-x86_64.tgz"
	firecrackerSHA256  = "382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6"

	ubuntuSnapshot = "20260105T000000Z"
	rootfsURL      = "https://cloud-images.ubuntu.com/minimal/releases/noble/release-20260105/ubuntu-24.04-minimal-cloudimg-amd64-root.tar.xz"
	rootfsSHA256   = "7f953b2129fb2752c79b575fe9a9180170bbfe6ee12186a9f732d2208b5df849"
)

//go:embed bootstrap.sh
var bootstrapTemplate string

// RenderSandbox deterministically lowers the logical System into the single
// deployable host Sandbox accepted by `canter apply`.
func RenderSandbox() (sdk.Spec, error) {
	system, err := BuildSystem()
	if err != nil {
		return sdk.Spec{}, err
	}
	graph, err := sdk.CompileSystem(system)
	if err != nil {
		return sdk.Spec{}, err
	}
	if err := validateLowering(system, graph); err != nil {
		return sdk.Spec{}, err
	}

	bootstrap := renderBootstrap()
	if strings.Contains(bootstrap, "@@") {
		return sdk.Spec{}, fmt.Errorf("bootstrap contains an unresolved renderer token")
	}
	spec := sdk.Spec{
		APIVersion: sdk.APIVersion,
		Kind:       "Sandbox",
		Metadata:   sdk.Metadata{Name: systemName},
		Spec: sdk.Desired{
			Intent: "Provision one verified KVM host and boot two real Oracle MySQL Firecracker guests; emit M1 proof only after two SQL SELECT 1 checks pass.",
			Compute: sdk.ComputeSpec{
				Class:     hostClass,
				Image:     hostImage,
				Replicas:  hostCount,
				Bootstrap: bootstrap,
			},
			M1:     sdk.M1Spec{Prefix: m1Prefix},
			Policy: sdk.Policy{MaxReplicas: hostCount},
		},
	}
	if err := spec.Validate(); err != nil {
		return sdk.Spec{}, fmt.Errorf("rendered sandbox: %w", err)
	}
	return spec, nil
}

func validateLowering(system sdk.System, graph sdk.ExecutionGraph) error {
	host := system.Spec.Constraints.Host
	if host.Class != hostClass || host.Count != hostCount || host.MemoryMiB != hostMemoryMiB || host.SystemReserve != hostReserveMiB {
		return fmt.Errorf("renderer only supports the exact c1/one-host/1024-MiB/384-MiB-reserve contract")
	}
	if len(system.Spec.Services) != 1 {
		return fmt.Errorf("renderer requires exactly one logical service")
	}
	service := system.Spec.Services[0]
	if service.Engine != "mysql" || service.Isolation != "firecracker" || service.Instances != guestCount || service.Resources.MemoryMiB != guestMemoryMiB || service.Resources.VCPU != guestVCPU {
		return fmt.Errorf("renderer requires exactly two 250-MiB/1-vCPU Firecracker MySQL guests")
	}
	if graph.Capacity.HostMemoryMiB != hostMemoryMiB || graph.Capacity.SystemReserveMiB != hostReserveMiB || graph.Capacity.GuestMemoryMiB != guestCount*guestMemoryMiB || graph.Capacity.UnallocatedMemory != 140 {
		return fmt.Errorf("compiled capacity does not preserve the renderer's fixed budget")
	}
	return nil
}

func renderBootstrap() string {
	replacer := strings.NewReplacer(
		"@@HOST_MEMORY_MIB@@", strconv.Itoa(hostMemoryMiB),
		"@@HOST_RESERVE_MIB@@", strconv.Itoa(hostReserveMiB),
		"@@GUEST_COUNT@@", strconv.Itoa(guestCount),
		"@@GUEST_MEMORY_MIB@@", strconv.Itoa(guestMemoryMiB),
		"@@GUEST_VCPU@@", strconv.Itoa(guestVCPU),
		"@@FIRECRACKER_VERSION@@", firecrackerVersion,
		"@@FIRECRACKER_URL@@", firecrackerURL,
		"@@FIRECRACKER_SHA256@@", firecrackerSHA256,
		"@@UBUNTU_SNAPSHOT@@", ubuntuSnapshot,
		"@@ROOTFS_URL@@", rootfsURL,
		"@@ROOTFS_SHA256@@", rootfsSHA256,
	)
	return strings.TrimSpace(replacer.Replace(bootstrapTemplate)) + "\n"
}

func marshalYAML(value any) ([]byte, error) {
	b, err := yaml.Marshal(value)
	if err != nil {
		return nil, err
	}
	return b, nil
}
