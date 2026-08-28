# Black-box Firecracker MySQL pair

This example uses Canter's higher-level `sdk.System` builder to describe one logical Oracle MySQL service with two instances. It deterministically lowers that contract into the one-host `Sandbox` accepted by the current deployment client. The host bootstrap builds and starts two real Firecracker microVMs. It does not use containers, QEMU, MariaDB, stand-in processes, or synthetic readiness receipts.

The fixed capacity budget is:

| Resource | Contract |
| --- | ---: |
| Hosts | exactly 1 c1 |
| Nominal host memory | 1024 MiB |
| Host reserve | 384 MiB |
| Firecracker guests | exactly 2 |
| Each guest | 250 MiB, 1 vCPU |
| Unallocated contract memory | 140 MiB |

Each guest has its own writable ext4 root disk, MAC address, TAP device, and unbridged `/30` network (`10.200.1.0/30` and `10.200.2.0/30`). Oracle MySQL is installed from a timestamped, signed Ubuntu Noble snapshot and configured for a 250 MiB VM. The bootstrap succeeds only after the host's real MySQL client obtains `1` from `SELECT 1` over TCP from both guests.

> Live result on 2026-08-28: the selected `c1` substrate did not expose `/dev/kvm`. The host rejected the deployment before downloading artifacts, Canter recorded a failed proof, and the host was destroyed. The generated code passed structural acceptance, but the two live MySQL guests remain unverified on this compute class. See `doc/(initial steps, august 2026)/firecracker-acceptance.md`.

## Artifacts and trust boundary

The renderer pins Firecracker `v1.16.1` and the Ubuntu Noble `release-20260105` root tree by HTTPS URL and SHA-256. Guest packages, including `mysql-server`, `linux-image-virtual`, and its initramfs, come from `snapshot.ubuntu.com/ubuntu/20260105T000000Z`; apt verifies signed Release metadata and package hashes. Host packages come from the signed repositories configured in the selected Ubuntu image.

Before downloads, the bootstrap requires x86-64, Ubuntu 24.04, the nominal 1024 MiB c1 memory envelope, and both read and write access to `/dev/kvm`. It then requires `kvm-ok`, and actual Firecracker startup is the final KVM ioctl test. Missing nested virtualization therefore exits nonzero before Canter can write a successful boot proof.

There is one platform limitation: the current Canter `Sandbox` contract and image resolver accept an image alias, not an immutable image ID. The generated spec therefore requests `ubuntu-24.04`, and bootstrap rejects any host that is not Ubuntu 24.04, but provider-side host-image selection is not content-addressed. All artifacts downloaded by bootstrap are pinned or resolved through the timestamped signed snapshot.

## Build, compile, and render

Run every command from the repository root:

```sh
go build -o ./examples/blackbox-firecracker-mysql/blackbox-firecracker-mysql ./examples/blackbox-firecracker-mysql

./examples/blackbox-firecracker-mysql/blackbox-firecracker-mysql contract > ./examples/blackbox-firecracker-mysql/system.yaml

go run ./cmd/canter compile \
  --file ./examples/blackbox-firecracker-mysql/system.yaml \
  > ./examples/blackbox-firecracker-mysql/execution-graph.json

./examples/blackbox-firecracker-mysql/blackbox-firecracker-mysql render \
  --output ./examples/blackbox-firecracker-mysql/canter.yaml
```

`system.go` is the SDK-facing source of truth. `system.yaml` and `canter.yaml` are checked-in deterministic renderings. The application also has a `compile` subcommand that invokes `sdk.CompileSystem` directly:

```sh
./examples/blackbox-firecracker-mysql/blackbox-firecracker-mysql compile
```

## Local structural tests

These checks do not require Linux, KVM, cloud credentials, or a deployment:

```sh
gofmt -w ./examples/blackbox-firecracker-mysql/*.go
go test ./examples/blackbox-firecracker-mysql
go vet ./examples/blackbox-firecracker-mysql
```

They validate contract/golden-file drift, the compiled graph and capacity, the KVM invariant and pre-download gate, exact Firecracker memory/vCPU settings, distinct disks/TAP networks, pinned digest shape, POSIX shell syntax, and two explicit host-side SQL checks.

## Live acceptance

Live acceptance requires Canter credentials plus a c1 provider flavor that is nominally 1024 MiB, runs x86-64 Ubuntu 24.04, and exposes usable nested KVM as a readable and writable `/dev/kvm`. Do not treat the local tests as proof that a provider supports nested virtualization.

Probe and create an M1 checkpoint before deployment:

```sh
go run ./cmd/canter probe

go run ./cmd/canter checkpoint \
  --file ./examples/blackbox-firecracker-mysql/canter.yaml \
  --message 'compiled exact one-host/two-Firecracker MySQL contract'
```

Review the provider plan, then deploy. Rootfs construction installs MySQL and a kernel, so use a longer timeout than the CLI default:

```sh
go run ./cmd/canter plan \
  --file ./examples/blackbox-firecracker-mysql/canter.yaml

go run ./cmd/canter apply \
  --file ./examples/blackbox-firecracker-mysql/canter.yaml \
  --timeout 30m
```

`canter apply` returns success only after bootstrap exits zero. Bootstrap cannot exit zero until both live guests returned `1` for `SELECT 1`; only then does the SDK accept the boot proof and write the apply receipt under `systems/blackbox-firecracker-mysql/receipts/`. On the host, the detailed causal acceptance record is `/var/lib/canter-firecracker-mysql/acceptance.json`. It is created only from the two captured SQL results and is not a substitute for the SDK's M1 proof/receipt.

Check lifecycle state:

```sh
go run ./cmd/canter status \
  --file ./examples/blackbox-firecracker-mysql/canter.yaml
```

Destroy the single host and persist the destroyed M1 state/receipt:

```sh
go run ./cmd/canter destroy \
  --file ./examples/blackbox-firecracker-mysql/canter.yaml \
  --yes
```

If nested KVM is unavailable, apply fails with an explicit `nested KVM unavailable` bootstrap error and cannot claim readiness. The host may remain recorded in Canter's `creating` state after a failed bootstrap; use the same `destroy --yes` command to clean it up.
