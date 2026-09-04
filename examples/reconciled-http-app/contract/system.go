package contract

import "github.com/canter0/canter/sdk"

func Build() (sdk.System, error) {
	return sdk.NewSystem("reconciled-http", "Run a versioned HTTP application, keep it healthy, update it without replacing its host, and support rollback.").
		OnHost("c1", 1, 1024, 512).
		WithM1("systems/reconciled-http").
		Provide(sdk.SystemService{
			Name: "web", Kind: "service", Engine: "http", Isolation: "process", Instances: 1,
			Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 128},
			Readiness: sdk.Readiness{Protocol: "http", Port: 8080}, Networking: "public",
		}).Build()
}
