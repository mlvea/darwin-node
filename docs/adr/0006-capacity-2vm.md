# ADR 0006 — The 2-VM limit, capacity, and scheduling

- Status: Accepted
- Date: 2026-08-23
- Phase: 0 / 1

## Context

Apple's macOS EULA (section 2B(iii) family) allows **two additional** macOS
instances in VMs per Apple-branded host, for development, testing, macOS
Server, or personal use. Virtualization.framework enforces this in XNU
(`hv_apple_isa_vm_quota`, default 2). A third concurrent macOS VM fails with
`VZErrorVirtualMachineLimitExceeded` (VZErrorDomain code 6).

This is not a hardware limit. An M2 Ultra still gets two guests. Linux guests
are *not* under this quota; we do not run Linux VMs.

Agoda sets `capacity.pods = 2` (good) but then **queues** CreatePod on a
buffered channel of size 2 (bad). The pod is already bound. It sits in
Starting/unknown until a slot frees, with no condition explaining why. The
scheduler believes the node still has CPU/memory (allocatable == host totals).

## Decision

### Hard cap

```
MaxConcurrentMacOSVMs = 2
```

Not configurable above 2. A `--max-vms` flag may only *lower* it (e.g. 1 on a
16 GB MacBook that must keep UI interactive). Attempting `> 2` is a startup
error.

### Advertised resources

| Resource | Capacity | Allocatable |
|---|---|---|
| `pods` | `maxVMs` (≤2) | same |
| `darwin.node/vm` | `maxVMs` | same (extended resource) |
| `darwin.node/metal` | `maxVMs` | same (shared GPU slots, see ADR 0007) |
| `cpu` | host logical CPUs | host − `ReservedCPU` (default 2) |
| `memory` | host total | host − `ReservedMemory` (default 4Gi) |
| `ephemeral-storage` | host `/` | host − `ReservedEphemeral` (default 20Gi) |

Pods **must** request `darwin.node/vm: 1` *or* we inject it as a default
request on container[0] via admission-less mutation in the provider (so
stock kube-scheduler accounts for the slot). We also set `pods=2`, which
alone is enough for the default scheduler if no other pods land here.

Taint (default on): `darwin.node/macos=true:NoSchedule`. Workloads opt in
with a matching toleration (and typically `nodeSelector kubernetes.io/os=darwin`).

### Fail closed

`CreatePod`:

1. Validate spec (container[0] is the VM, volumes known, resources parse).
2. `capacity.TryAcquire(podUID)` — if both slots are taken, return a typed
   `ErrVMCapacityExhausted`. The provider records a Warning event and sets
   the pod to Failed with reason `VMCapacityExhausted`. We do **not** queue.
3. On DeletePod / runtime failure after acquire, `Release(podUID)`.
4. Crash recovery: on start, rebuild the slot table from local pod state
   (and running VZ VMs if we can see them). Leaked slots are worse than a
   brief under-count.

A third VM is never started. We never catch `VirtualMachineLimitExceeded` as
a retryable error; it is a bug if it happens.

### Node conditions

| Type | True when |
|---|---|
| `Ready` | provider healthy, VZ available, disk/memory not critical |
| `VMCapacity` (custom) | `used < max` → False (no pressure); `used == max` → True, reason `SlotsFull` |
| `MemoryPressure` / `DiskPressure` | real host checks, not hardcoded |
| `GuestRuntime` | Virtualization.framework probe succeeded |

When `VMCapacity=True` (full), we additionally set
`darwin.node/vm-full=true:NoSchedule` dynamically so the scheduler stops
sending work *before* CreatePod rejects. Remove the taint on release.

### Fairness

Two pods. No weighted fair-share beyond Kubernetes requests. CPU/memory
requests size the VM (`SetCPUCount` / `SetMemorySize`). Limits: if set, they
**cap** the VM size; if limit < request, reject. VZ has no hard cap after
start — the VM gets what it was created with. We do not overcommit vCPUs
above host allocatable. Two 10-CPU requests on a 12-CPU Mac: the second
fails resource admission even if a VM slot is free.

### Multi-node

Each physical Mac runs one `darwin-node`. Capacity is per process / per
host. There is no cluster-level 2-VM quota beyond the sum of nodes. Helpers:
label `darwin.node/host-id=<hardware UUID>` so operators can spread.

## Consequences

- Honest scheduling: a 20-Mac fleet = 40 concurrent macOS pods, full stop.
- Overscheduled pods fail fast and observably.
- Host UI/CI remains responsive because of reserved CPU/memory.
- DaemonSets must tolerate the taint *and* request no `darwin.node/vm` — we
  reject any DaemonSet-like extra VM. Sidecar-only DaemonSets are not
  supported (container[0] must be a VM). Use a Linux node for node-exporter;
  scrape darwin-node's metrics endpoint instead.
