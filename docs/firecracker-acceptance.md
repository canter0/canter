# Firecracker/MySQL acceptance — 2026-08-28

The black-box application generated a `System` contract for one `c1` host with a nominal 1024 MiB memory envelope, a 384 MiB host reserve, and two isolated MySQL services. The deterministic compiler expanded it into one compute host, one Firecracker runtime, two 250 MiB/1-vCPU microVMs, two MySQL service nodes, one `m1` namespace, and explicit KVM/readiness invariants.

Local structural acceptance passed: the Go tests and vet checks, POSIX shell syntax, SDK/CLI graph equivalence, checked-in rendering, two distinct TAP networks, two distinct guest disks, two 250 MiB Firecracker configurations, pinned artifact digest shape, and two explicit TCP `SELECT 1` checks.

The live deployment created exactly one `c1` host. It became ACTIVE, then its bootstrap rejected the host before downloading any artifacts:

```text
FATAL: nested KVM unavailable: Firecracker requires read/write /dev/kvm
```

Canter received a signed failed proof with exit code 1 and did not emit a ready/apply receipt. Console output independently showed the failure at 27.57 seconds after boot. The exact compute resource was destroyed, its final state was persisted as `DELETED`, and a subsequent live inventory probe returned zero resources.

This is an unsatisfied substrate capability, not a successful Firecracker/MySQL run. Both MySQL guests remain structurally implemented but unverified on this compute class. A passing live test requires a 1 GiB substrate that exposes read/write nested KVM; substituting containers, QEMU, MariaDB, or process-level stand-ins would violate the contract.
