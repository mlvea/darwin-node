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

With `--vm-supervisor` left at its default `off`, the runtime box above
creates the `VZVirtualMachine` inside the agent process. `--vm-supervisor=on`
replaces that box with a Unix client. Each VM then lives in the same
signed binary started as `--role=supervisor --slot=0` or `--slot=1`.
The agent still watches Kubernetes and serves `:10250`. The supervisor
does not pull images, watch the API server, or listen on that port.
See [Per-VM supervisor](#per-vm-supervisor).

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
  carries the static tolerations, including
  `darwin.node/os=macos:NoSchedule`, and `kubernetes.io/os=darwin`.
  See [scheduling](scheduling.md).
- Publishes `darwin.node/os=macos:NoSchedule` on every node, including
  when `--disable-taint` drops `virtual-kubelet.io/provider` and
  `darwin.node/macos`. The node label stays `kubernetes.io/os=darwin`.
  A pod tolerates it with key `darwin.node/os`, operator `Equal`,
  value `macos`, effect `NoSchedule`. Which images and CPU/memory sums
  `CreatePod` accepts are in [admission](admission.md).
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

## VM identity

A cold boot leaves the image config's `machineIdData` unused. That field
is a template baked at image build time. `preparePlatform` mints one
`vz.NewMacMachineIdentifier` and `vmidentity.ColdBoot` keeps the image
value only so a minter that returns those same bytes fails the boot. The
platform is configured with the minted object. The image bytes are not
decoded into it.

`ColdBoot` clonefiles the golden aux to
`<pod dir>/ephemeral/slot-<0|1>/<pod-uid>/aux.img`. `Machine.Stop` removes
that file. The golden image stays. A missing path, an empty file, or a
symlink is refused before the copy, so a failed boot does not leave an
aux behind. There is no flag. `VMSpec` has no slot index. The vz runtime
asks for slot 0, and the pod UID keeps the two concurrent clones apart.
A warm adoption does not cold-boot again, so the pod keeps the identifier
that VM's cold boot already minted.

Virtualization.framework publishes no vmgenid. This tree does not restore
a saved VM. A per-job ephemeral keychain is not implemented. Detail:
[vm-identity.md](vm-identity.md).

## Guest Adopt

`Adopt` is a guest-agent method on protocol 1. It is separate from
`supervisor.Adopt`, which reattaches the node-agent to a slot socket.

`runConnected` dials the agent and then calls `Engine.reseedGuest` before
`Materialize` and before `Workload`. A warm-pool handoff uses that same
path. `restartVM` calls it again after the new dial, before the workload,
and before a copy-mode rematerialize: a VM restart is a new boot.
`recoverAgent` redials a dropped session and does not call `Adopt` again.
Reattaching a live supervisor (`runRecovered`) redials the guest that is
already running and does not call `Adopt`, `Materialize`, or `Workload`.

The guest stays untrusted. The host checks `reseed_ok`, a 32-byte sample
that is not all zeros, a constant-time proof
`SHA256(host_entropy_64b || sample)`, a guest clock within 500ms of the
host, and a UUID-shaped `io_platform_uuid`. A sample this engine has
already accepted is rejected. The engine keeps 1024 samples and then
drops the oldest. Entropy and samples stay out of pod status text.
`Adopt` reports `io_platform_uuid` and does not rotate the ECID.

A guest that omits capability `adopt`, or that answers `unknown_method`,
fails the pod. `DARWIN_NODE_ALLOW_LEGACY_GUEST=1` or `true` skips only
that case and logs a warning. Default off. The variable is read in the
engine. It is not a field on the node config. A bad proof still fails
the pod. `DARWIN_NODE_GUEST_SOFTWARE_PLATFORM=1` or `true` forces the
in-memory platform in `darwin-guest-agent`. SimVZ sets it when it execs
the test child. The launchd job inside a VM image does not set it.
Detail: [guest-adopt.md](guest-adopt.md).

## Cache volumes

Pod annotations `cache.darwin.node/<name>: <absolute-guest-path>`
declare persistent caches. The host restores each one into the pod dir
with APFS `clonefile` (CoW) and shares it over virtio-fs at the
annotated path (link placement). The share is read-only unless the pod
sets `cache-write.darwin.node/<name>: "true"` and `--cache-writers`
names its namespace or `namespace/serviceaccount`. The cache of an
allowed writer that exits 0 is cloned back into
`<cache-dir>/cache-store/<ns>/<name>/` before the pod directory is
removed, including a workload that ends by itself. A failed exit, a
kill, and a delete of a pod that is still running leave the previous
store. Failed starts never snapshot. The commit clones to `<store>.tmp`
and renames it into place. A write annotation outside the allowlist
emits `CacheReadOnly` and the share stays read-only. No new guest
protocol; no PVC/CSI. The guest-side check that a read-only virtio-fs
mount cannot be written is hardware-pending. Details:
[cache-volumes.md](cache-volumes.md).

## hostPort and attach

`hostPort` is a userspace TCP proxy in `pkg/hostport` from the host port
to `PodIP:containerPort`. Pod teardown calls `Release` when the pod is
deleted or has failed. That closes the pod's listeners and every
connection still spliced, so a removed hostPort does not leave the guest
dialed. When one side ends, the proxy half-closes the other and fully
closes both after 5 seconds. A dial to the guest gives up after 5
seconds. One pod may hold 256 connections and the node 512, counting a
dial that has not finished. The next connection is closed and is not
dialed. There is no flag for these bounds. UDP is rejected.

`kubectl attach` returns
`attach is not supported; use kubectl exec or kubectl logs -f` for the
VM container and for a sidecar. The guest agent can exec a new process
and can follow the workload log. It has no RPC that joins the running
workload's stdio, so attach does not start a shell.

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
  The supervised machine does not expose that socket.
  `--serial-console` on the in-process runtime is unchanged.

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
the normal path (preStop, guest shutdown RPC). A named cache is written
back only when that workload already exited 0 and the pod may write it.
Each pod uses its `terminationGracePeriodSeconds`. The warm replenisher
is paused for the duration; freed slots serve departing pods. Pods still
running when the budget expires are force-stopped by `Engine.Close`,
which releases their slots and does not replace a cache that has not
exited 0. A Create, Start, Stop, or
RemovePod that has not returned after 30 seconds does not hold that
close: the error is logged, and the slot stays held until the call
returns and the normal path releases it. A second signal cancels the
rest of that drain and still runs `Close` before exiting 130. A crash
still skips the stop; the next process reclaims leftover pod directories.
That reclaim is the in-process runtime (`--vm-supervisor` left off). With
the flag on, a supervisor that is still alive is reattached and a dead
supervisor stays on the node-restart settlement. See
[Per-VM supervisor](#per-vm-supervisor).

## Per-VM supervisor

A `VZVirtualMachine` cannot outlive the process that created it. The
default, `--vm-supervisor=off`, keeps that VM in the agent, so a crash
or upgrade stops it. `--vm-supervisor=on` moves each VM into
`darwin-node --role=supervisor --slot=N` (`N` is 0 or 1). The two
processes speak a versioned Unix protocol on `<run dir>/slot-N.sock`:
`StartVM`, `InspectVM`, `PauseAndSaveVM`, `StopVM`, and
`AttachChannels`. `AttachChannels` passes two `SCM_RIGHTS` descriptors:
the supervisor's end of the datagram socketpair used as
`VZFileHandleNetworkDeviceAttachment`, and a connected stream toward
the guest agent. The supervisor does not parse the guest protocol.

Production examples are LaunchAgents `io.darwin-node.slot-0` and
`io.darwin-node.slot-1` in the `gui/<uid>` domain, with `KeepAlive`
`{SuccessfulExit=false}`. After a supervisor exits 0, launchd leaves
that slot stopped until `launchctl kickstart gui/$UID/io.darwin-node.slot-N`
or the agent is using `--vm-supervisor-spawn=subprocess`.

The supervised adapter calls `supervisor.Adopt` on first attach and on
`Recover`: a fresh `AttachChannels` Connect over `SCM_RIGHTS`, never a
saved fd. Supervisor death is `SlotLost` / `VMFailed` with message
`supervisor lost` within about 2 seconds. The watch is edge-triggered
epoll on Linux and kqueue on Darwin, and it does not wait for the
dead-man switch. With `--vm-supervisor` on, `Engine.Recover` calls that
adapter `Recover` before it deletes unheld pod directories. The metrics
wrapper forwards the call. A live supervisor whose lease matches
`cache/pods/<uid>` is reattached as `Running`: the slot stays held, the
disk and `control/agent-token` stay, the guest is redialed through the
adopted fd, and no new VM is booted. The restart count already on the
pod is kept, and so is `status.startTime` when it is set. Init and the
workload are not applied again. Sidecars are created again. A dead
supervisor keeps the node-restart settlement (`Never` stays `Failed` /
`NodeRestarted`, slot free, disk removed; `Always`, `OnFailure`, and an
empty policy receive one restart credit and boot again). A supervisor with no
matching pod directory is stopped and its slot is released. A
supervisor lost while the pod is `Running` is reason and message
`supervisor lost` on the pod. Restart policy does not boot a
replacement, and the slot is released once. Any other unexpected stop
is still the engine's `macOS VM Failed unexpectedly`. The flag stays
default off. With it off, `Recover` returns no machines.
`Connect` during `Start` on real Virtualization.framework is
hardware-pending. The guest agent still dials the host, while
`AttachChannels` dials the guest, and those meet
when the guest listens. The datagram descriptor is handed to the agent
and is not bridged, so a guest booted with the flag on has no host
network. The lease does not store the guest-agent token. On reattach
the engine reads `cache/pods/<uid>/control/agent-token`. `PauseAndSaveVM`
returns the framework error on macOS 13; the save API is macOS 14, and
the minimum stays macOS 13. LaunchAgents, `launchctl`, and these
Virtualization.framework calls are hardware-pending. The plist text is
what CI checks. The engine half is
[vm-supervisor-recovery.md](vm-supervisor-recovery.md).

`--vm-supervisor` stays default off until a Mac Mini passes the
file-descriptor, App Nap, and `kickstart -k` checks in
[vm-supervisor-hardening.md](vm-supervisor-hardening.md). Lease JSON
includes `host_boot_id` (the host boot session, separate from the VM
`boot_id`) and `supervisor_proc_start` (the OS start time of the
supervisor pid). `supervisor.Adopt` rejects a lease that omits either
field before it dials, and that pid is not signalled. `Recover` then
asks a supervisor that is still listening to `StopVM` and deletes the
lease, so the VM is not reattached. The hello/ready handshake and
`ProtocolMajor` 1 are unchanged.

`Machine.Stop` may spend the pod's termination grace in a clean guest
shutdown and then hang inside `VZVirtualMachine.Stop`. The hang cap is
added to that grace. The supervisor waits grace plus 10s and then exits
1. The node-agent waits grace plus 12s, sends `SIGKILL`, and waits until
the process has exited before it releases the slot. A pid whose host
boot id or OS start time does not match the lease is not killed.

A supervisor the agent spawns (`--vm-supervisor-spawn=subprocess`, the
default) has stdin `/dev/null` and appends stdout and stderr to
`<run dir>/slot-N.supervisor.log` (mode 0600). The parent closes its
copies after `Start`. Slot LaunchAgents still log under
`/var/log/darwin-node/`. The node plist
`deploy/launchd/io.darwin-node.node.plist` sets `AbandonProcessGroup`
and `ProcessType` `Interactive`, so `kickstart -k` leaves supervisor
sessions running and App Nap does not defer the agent heartbeat. Those
two checks, and the fd-flatness check, are hardware-pending. The plist
is installed as a LaunchDaemon; see [installation.md](installation.md).

The operator notes, the 103-byte macOS socket-path limit, and the
install steps are in [vm-supervisor.md](vm-supervisor.md) and
[installation.md](installation.md). The decision is
[ADR 0010](adr/0010-per-vm-supervisor.md).

## Cluster startup

A process that joins a cluster calls `clusterClient` in
`cmd/darwin-node/run.go` before virtual-kubelet registers the node.
With `--rotate-server-certificates` and `--rotate-client-certificates`
both off, that loads `--kubeconfig` once through
`nodeutil.ClientsetFromEnv` and does not create `--cert-dir`. The
kubelet serving files still reload when their mtime or size changes.
That is the previous code. The ServiceAccount kubeconfig remains the
default.

With `--rotate-client-certificates`, a `certificate.Manager` in
`pkg/nodecert` requests signer
`kubernetes.io/kube-apiserver-client-kubelet` using
`--bootstrap-kubeconfig`. The manager sits in this cluster-startup
path. Startup waits until the certificate exists, then API calls run
as `system:node:<nodename>` in `system:nodes`, so NodeRestriction and
the Node authorizer can apply. Later rotations build a new HTTP
transport for requests that start after the swap. A request already
inside `RoundTrip`, including a pod status write, stays on the
transport that started it and is not dropped. darwin-node does not
approve the CSR.

`--rotate-server-certificates` starts a second manager for signer
`kubernetes.io/kubelet-serving`. Startup does not wait for that
certificate. See [node-certificates.md](node-certificates.md) and
[kubeconfig-rotation.md](kubeconfig-rotation.md).

## Linux stand-in

[simvz.md](simvz.md) is `pkg/runtime/simvz`, a `runtime.Runtime` that
opens real sockets so the engine can boot, dial, and stop a VM on
Linux. `--runtime` still selects `vz` or `fake`. SimVZ is not wired to
that flag.

## Package map

| Package | Responsibility |
|---|---|
| `pkg/provider` | Virtual Kubelet `PodLifecycleHandler` |
| `pkg/engine` | Pod state machine, restart policy, hooks, probes |
| `pkg/runtime` | VM interface |
| `pkg/runtime/vz` | Virtualization.framework (`darwin/arm64`). Cold boot calls `pkg/vmidentity` |
| `pkg/vmidentity` | Per-pod machine identifier and private NVRAM clone. No Virtualization.framework import, so Linux tests run it. See [vm-identity.md](vm-identity.md) |
| `pkg/runtime/fake` | In-process VM for tests |
| `pkg/runtime/simvz` | Linux stand-in that opens real sockets. See [simvz.md](simvz.md). Not selected by `--runtime` |
| `pkg/runtime/supervised` | Agent-side adapter for `--vm-supervisor=on`. First attach and `Recover` call `supervisor.Adopt` (a fresh Connect over `SCM_RIGHTS`) |
| `pkg/supervisor` | Slot control socket, `supervisor.Adopt`, boot lease, peer-uid check, edge-triggered death watch. See [vm-supervisor.md](vm-supervisor.md) |
| `pkg/guest` | Agent protocol, client, server, transports |
| `pkg/capacity` | Slot table, fail-closed acquire/release |
| `pkg/image` | OCI pull/cache/verify, clonefile overlay |
| `pkg/volume` | Materialize emptyDir/hostPath/ConfigMap/Secret/projected |
| `pkg/net` | NAT/bridged, IP, MAC |
| `pkg/hostport` | Userspace hostPort proxy (half-close, 5s dial and linger, 256/512 connection caps) |
| `pkg/sidecar` | Host containers |
| `pkg/node` | Capacity, allocatable, conditions, labels |
| `pkg/event` | Kubernetes events |
| `pkg/observability` | slog, Prometheus, OpenTelemetry |
| `pkg/config` | Flags, env, file |
| `pkg/nodecert` | Opt-in CSR bootstrap and rotation for the node client and the kubelet serving certificate. See [node-certificates.md](node-certificates.md) |

## Extension points (CRDs later)

`pkg/runtime.Runtime` and `pkg/image.Store` have no Pod types on their
public surfaces. A future `VirtualMachine` CRD controller can call them
directly. The provider stays the Kubernetes *Pod* API.

## Trust boundaries

- Host agent is a privileged, entitled process (Virtualization, optional vmnet).
- With `--vm-supervisor=on`, the run directory (mode `0700`, owned by the service uid) and a same-uid peer check on the control socket are the local trust boundary for that VM. The in-process path is unchanged while the flag is off.
- Guest agent runs as root *inside* the VM, not on the host. `Adopt` does not change that: the host checks the proof and rejects a reused sample. See [guest-adopt.md](guest-adopt.md).
- vsock is host-process-only. The guest agent has no TCP transport.
  Guest-reported metrics and status text are untrusted.
- Secrets exist on the host disk under a 0700 pod directory for the pod lifetime.
- Sidecars run on a per-pod Docker bridge with `no-new-privileges`. Probes dial that container's address, and the response body is not published on the pod.
