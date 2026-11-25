# Architecture

darwin-node turns an Apple Silicon Mac into a Kubernetes node that runs
**native macOS virtual machines as pods**, with optional Linux sidecar
containers on the host.

It is not a kubelet fork and not an Orka-style control plane. It is a
Virtual Kubelet provider with a real VM runtime and a Guest Agent.
Lineage and third-party licenses: [credits.md](credits.md).

```
                    Kubernetes API server
                            │
                            ▼
                 ┌─────────────────────┐
                 │    darwin-node      │  Virtual Kubelet
                 │  ┌───────────────┐  │
                 │  │   Provider    │  │  Pod API, exec, logs, stats
                 │  └──────┬────────┘  │
                 │         ▼           │
                 │  ┌───────────────┐  │
                 │  │  Pod Engine   │  │  lifecycle, probes, hooks
                 │  └──┬───┬───┬────┘  │
                 │     │   │   │       │
                 │     ▼   ▼   ▼       │
                 │   Runtime Sidecar   │
                 │   Image   Volume    │
                 │   Net     Capacity  │
                 └─────┬───────────────┘
                       │ vsock (only agent transport)
                       │ SSH  (last resort, no agent)
                       ▼
                 ┌─────────────────────┐
                 │   macOS VM (x <= 2)  │
                 │  darwin-guest-agent │  launchd
                 │  Metal (shared GPU) │
                 └─────────────────────┘
```

## Hybrid pods

```
spec.containers[0]     ->  macOS VM via Virtualization.framework
spec.containers[1..]   ->  host Docker (or future containerd) sidecars
spec.initContainers    ->  host-side only (sequential); no extra VM boots
```

This is the unique capability. A typical CI pod is a macOS VM plus a logging
or artifact sidecar.

## 2-VM hard cap

Apple's EULA and XNU (`hv_apple_isa_vm_quota`) allow two concurrent macOS
VMs per host. darwin-node:

- Advertises `pods=2`, `darwin.node/vm=2`, and `darwin.node/vm-slot=2`
  (capacity and allocatable follow `--max-vms`)
- Does not inject a resource request. Pods request `darwin.node/vm-slot: 1`.
  The optional Helm MutatingAdmissionPolicy adds that request on
  container 0 when `runtimeClassName` is `darwin-vm`. The RuntimeClass
  carries the static tolerations and nodeSelector. See
  [scheduling](scheduling.md).
- **Rejects** a pod when every slot is held by a running VM and nothing
  warm can be reclaimed (`VMCapacityExhausted`). It does not queue.
- Does not taint `darwin.node/vm-full`. A stale taint is removed on the
  next node status publish. `VMCapacity` stays informational.

See [ADR 0006](adr/0006-capacity-2vm.md).

## Warm VM pool

`--warm-slots=N` keeps up to N pre-booted, agent-ready guests in slots
that pods do not currently need (default image source: the most recently
booted image, or `--warm-image`). A pod whose image matches and whose
primary container mounts nothing adopts a warm VM and skips the cold
boot entirely. Invariants:

- Warm VMs acquire real slots from the same fail-closed table.
- Adoption requires the same image ref and the same vCPU and memory the
  pod requested. The default request is 2 vCPU and 4 GiB, which is the
  size the pool boots.
- A real pod cancels an in-flight warm boot and evicts a pooled guest it
  cannot adopt. A crashed warm VM is removed. A failed warm boot waits
  1s, doubling to 30s.
- Rejection still happens once no warm entry and no in-flight warm boot
  remain and every slot is held.

## Cache volumes

Pod annotations `cache.darwin.node/<name>: <absolute-guest-path>`
declare persistent caches. The host restores each one into the pod dir
with APFS `clonefile` (CoW), shares it read-write over virtio-fs at the
annotated path (link placement, guest writes land on host disk), and
clones the final state back into `<cache-dir>/cache-store/<ns>/<name>/`
on graceful delete. Failed starts never snapshot. No new guest protocol;
no PVC/CSI.

## Interactive exec, follow logs, console

- Exec: `ExecReq.Stdin/TTY` + client->agent stream frames
  (`ExecStdin`, `TtyResize`) ride the same envelope ID as the request.
  The agent routes them through a per-connection upstream router; TTY
  execs run under a real PTY (`creack/pty`) so control bytes reach the
  guest line discipline.
- Logs: `LogsReq.Follow` subscribes to the guest `LogBuffer`; appends
  stream until the client disconnects. Slow followers drop rather than
  block the buffer.
- Console: with `--serial-console`, vz attaches a serial port backed by
  pipe pairs (`FileHandleSerialPortAttachment`). The engine bridges it
  to `darwin-console-<hash>.sock` in TempDir; `darwin-node console`
  dials and enters raw mode. Independent of agent and SSH by design.

## Delta images

A delta dir carries `delta.json` plus `<name>.patch` files: flat
[offset][len][data] records at 4 MiB granularity. `ApplyDelta`
digest-verifies the base, clonefile-clones it, patches in place,
truncates to target size, and verifies the result SHA-256, so patch
correctness is checked end to end on every apply. Output is an ordinary
image dir consumable by the engine's normal resolve path.

The pull path understands delta artifacts natively (see
[delta-images.md](delta-images.md)): the base resolves through the same
single-flight cache, and a failed apply never leaves a partial entry.

## Graceful shutdown

SIGTERM/SIGINT start a drain bounded by `--shutdown-grace-period`
(default 60s): new creates are rejected, every pod is deleted through
the normal path (preStop, guest shutdown RPC, cache snapshots), each
with its `terminationGracePeriodSeconds`. The warm replenisher is paused
for the duration; freed slots serve departing pods. Pods still running
when the budget expires are force-stopped by `Engine.Close`, which
releases their slots and snapshots caches. A Create, Start, Stop, or
RemovePod that has not returned after 30 seconds does not hold that
close: the error is logged, and the slot stays held until the call
returns and the normal path releases it. A second signal cancels the
rest of that drain and still runs `Close` before exiting 130. A crash
still skips the stop; the next process reclaims leftover pod directories.

## Package map

| Package | Responsibility |
|---|---|
| `pkg/provider` | Virtual Kubelet `PodLifecycleHandler` |
| `pkg/engine` | Pod state machine, restart policy, hooks, probes |
| `pkg/runtime` | VM interface |
| `pkg/runtime/vz` | Virtualization.framework (`darwin/arm64`) |
| `pkg/runtime/fake` | In-process VM for tests |
| `pkg/guest` | Agent protocol, client, server, transports |
| `pkg/capacity` | Slot table, fail-closed acquire/release |
| `pkg/image` | OCI pull/cache/verify, clonefile overlay |
| `pkg/volume` | Materialize emptyDir/hostPath/ConfigMap/Secret/projected |
| `pkg/net` | NAT/bridged, IP, MAC |
| `pkg/hostport` | Userspace hostPort proxy |
| `pkg/sidecar` | Host containers |
| `pkg/node` | Capacity, allocatable, conditions, labels |
| `pkg/event` | Kubernetes events |
| `pkg/observability` | slog, Prometheus, OpenTelemetry |
| `pkg/config` | Flags, env, file |

## Extension points (CRDs later)

`pkg/runtime.Runtime` and `pkg/image.Store` have no Pod types on their
public surfaces. A future `VirtualMachine` CRD controller can call them
directly. The provider stays the Kubernetes *Pod* API.

## Trust boundaries

- Host agent is a privileged, entitled process (Virtualization, optional vmnet).
- Guest agent runs as root *inside* the VM, not on the host.
- vsock is host-process-only. The guest agent has no TCP transport.
  Guest-reported metrics and status text are untrusted.
- Secrets exist on the host disk under a 0700 pod directory for the pod lifetime.
- Sidecars run on a per-pod Docker bridge with `no-new-privileges`. Probes dial that container's address, and the response body is not published on the pod.
