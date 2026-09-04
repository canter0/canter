package main

import "github.com/canter0/canter/sdk"

const (
	systemName        = "blackbox-firecracker-mysql"
	hostClass         = "c1"
	hostCount         = 1
	hostMemoryMiB     = 1024
	hostReserveMiB    = 384
	guestCount        = 2
	guestMemoryMiB    = 250
	guestVCPU         = 1
	m1Prefix          = "systems/blackbox-firecracker-mysql"
	hostImage         = "ubuntu-24.04"
	readinessPort     = 3306
	readinessProtocol = "mysql"
)

// BuildSystem is the SDK-facing source of truth for this example.  The checked-in
// system.yaml is a human-reviewable serialization of this value.
func BuildSystem() (sdk.System, error) {
	return sdk.NewSystem(
		systemName,
		"Provide a logical two-instance Oracle MySQL service in two Firecracker microVMs on exactly one c1 host.",
	).
		OnHost(hostClass, hostCount, hostMemoryMiB, hostReserveMiB).
		WithM1(m1Prefix).
		Provide(sdk.SystemService{
			Name:       "mysql",
			Kind:       "database",
			Engine:     "mysql",
			Isolation:  "firecracker",
			Instances:  guestCount,
			Resources:  sdk.ServiceResources{VCPU: guestVCPU, MemoryMiB: guestMemoryMiB},
			Readiness:  sdk.Readiness{Protocol: readinessProtocol, Port: readinessPort},
			Networking: "isolated-tap",
		}).
		Build()
}
