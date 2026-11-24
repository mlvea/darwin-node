# Limitations

darwin-node is **alpha**. The node agent was built quickly (largely with
coding agents) and still needs review and testing on real Apple Silicon
hardware. What follows is the intended contract, not a production SLA.
The software is provided AS IS; see [LICENSE](../LICENSE).

darwin-node is not a Linux kubelet. Some Kubernetes features are impossible
on Apple Silicon macOS VMs; others are degraded. This file is the contract.

## Physically or legally impossible

| Feature | Why |
|---|---|
| More than 2 concurrent macOS VMs per host | Apple EULA + XNU `hv_apple_isa_vm_quota`. Third VM -> `VZErrorVirtualMachineLimitExceeded`. |
| Nested virtualization | Virtualization.framework does not nest macOS VMs. |
| Exclusive GPU passthrough | Only paravirtualized Metal (`VZMacGraphicsDevice`). Shared with host and the other VM. |
| Intel Macs | Out of scope. Apple Silicon only. |
| Linux VM as container[0] | This project is a *macOS* node. Linux workloads belong on Linux nodes. |
| cgroups / Linux securityContext | The primary container is a full macOS guest. |
| `privileged: true` meaning host kernel | The VM is already a separate kernel. Host devices are not passed through except virtio. |
| In-place pod spec updates | Recreate. Best-effort state transfer is Phase 6. |

## Degraded (supported with caveats)

| Feature | Behaviour |
|---|---|
| NAT networking | PodIP is host-local. Cluster Services from other nodes do not reach it. Node is tainted `darwin.node/nat-only` unless `--allow-nat-workloads`. |
| Resource limits | Applied at VM *create* as a cap on vCPU/memory. Cannot be enforced after start. |
| `emptyDir.medium: Memory` | Not tmpfs; ordinary directory. |
| `emptyDir.sizeLimit` | The watch loop sums regular files on the host directory and does not follow symlinks. Over the limit, or a relative symlink that lexically leaves the directory, the pod fails and the slot is released. The pod message does not include the host path. An absolute or dangling symlink does not fail the pod. The guest can write past the limit between checks. An APFS quota that refuses the write is still future work. `medium: Memory` is a normal directory. |
| ServiceAccount token rotation | Each projected token is minted on its own. A mint error or an empty token fails pod creation. The watch loop refreshes at half the remaining lifetime, rewrites the host file, and re-copies it into the guest. Refresh keeps the last good file until the recorded expiry; after expiry a failed refresh fails the pod. |
| Init containers | Host Docker only, sequential, before the VM starts. A macOS image as an init container is rejected. |
| Crash-looping hybrid sidecar | The container waits in `CrashLoopBackOff` (10s, then 20s, doubling, capped at 5m) with that reason and a `back-off … restarting failed container` message. Same rule as kubelet: the pod stays `Running` and is not failed for looping. It keeps its VM slot until the pod is deleted. Delete during the backoff returns and frees that slot; the backoff does not sleep. Deleting the pod drops that backoff entry. The other slot still admits a pod. The 2-VM cap is not raised. |
| Shutdown during a stuck boot | `Close` returns after 30s and keeps the slot. Process exit then waits up to 5s more. If `Start` returns in that window the VM is stopped and the slot is released once. If it does not, the pod UID or warm slot id is logged and written to `cache/abandoned-boots.json` (mode 0600) by writing a sibling `.tmp` and renaming it. The next start deletes that bundle when the slot is not held, and does not pin a slot for it. A truncated record, or a leftover `.tmp`, is removed and does not keep the bundle. |
| Crash before the next start | A killed process leaves pod bundles, warm leftovers, and cache-store `.old`/`.tmp` files. The next start deletes unheld bundles, keeps a bundle whose slot is held, and republishes a committed cache store. An image left only under `cache/image-replacing/<key>` is published when `images/<key>` is missing and the tree has provenance; `image-partial` is deleted and never published. A symlink inside those directories, or one standing in for `pods`, `cache-store`, `image-partial`, or `image-replacing`, is removed as a symlink and is not followed. Break-glass sockets named `darwin-console-<16 hex>.sock` in the temp directory are removed at process start, before any pod listens. |
| Host health probe | A failed host probe (disk, CPU, or memory), an unavailable VM runtime, or a cache directory that cannot be written sets `Ready=False`. Memory and disk pressure become `Unknown` only when the host probe itself failed; a runtime or cache failure keeps the last real pressure bits. `Ping` still returns nil so the node status patch and the lease continue. New pods are rejected until the next successful probe. Pods already running are left in place. |
| Workload exit phase | A guest `Exit` of 0 with `RestartPolicy` `Never` or `OnFailure` sets phase `Succeeded`, `terminated.exitCode` 0, and reason `Completed` (or the guest reason). `Always`, and an empty policy, restarts the VM and the pod stays `Running`; the container `restartCount` increments. A non-zero code under `Always` or `OnFailure` restarts the same way. `Never` with a non-zero code stays `Failed` and publishes that code. A stop with no guest code stays `Failed`: `VMStatus.ExitCode` when the runtime set one, otherwise 1, reason `VMStopped` or `VMFailed`. A guest reason of 1–128 characters, starting with a letter and continuing with letters or digits (`OOMKilled`), becomes `terminated.reason`; anything else leaves the host reason. Delete, drain, and eviction of a pod whose workload has not exited set `Failed` with exit 137 when the grace period is 0 (SIGKILL) and 143 otherwise (SIGTERM), reason `Error`. A pod that is already `Succeeded` or `Failed` keeps that phase and code. The Virtualization.framework runtime still does not set `ExitCode`. `darwin-guest-agent run -- <command>` sends the workload's code (`Completed` for 0, `Error` otherwise, `128+N` for a signal) and then halts. The send gives up after 2s. An agent with no `run` command still does not invent a code. A liveness restart drops the previous code. The VM liveness probe still fails the pod after more than 5 restarts. |
| Pod label and annotation updates | Applied to the running pod. Virtual-kubelet rebuilds container env from a map, so a reordered env slice is the same spec. Any other spec change is rejected and the VM keeps its slot. |
| Hybrid sidecar startup probe | Past the failure threshold the container is killed and the pod restart policy applies. `Always` and `OnFailure` start it again and the pod stays up, holding its VM slot. `Never` fails the pod with the runtime exit code (137 for the kill). A runtime that cannot stop the container still fails the pod and releases the slot. Liveness and readiness do not run until this probe has passed. |
| Hybrid sidecar liveness and readiness | Liveness past the failure threshold kills the container and uses the same restart policy and crash backoff as an exit. The VM is not restarted. Readiness does not kill the container: the container and the pod stay unready until `successThreshold` successes, and a later `failureThreshold` of failures clears Ready with reason `ReadinessProbe`. The slot stays held. |
| Probe period | Each startup, liveness, and readiness probe, on the VM and on a sidecar, waits its own `periodSeconds` (default 10s) after the previous sample. The first sample after the container starts, and the first after a restart, is not delayed by that period. `initialDelaySeconds` still applies. Sidecar probe I/O does not run on the watch, so a slow sidecar sample does not hold the next VM sample. The watch applies the result and joins those goroutines before it returns. The watch still wakes at least every `ProbeInterval` for exits, volumes, and backoff. A finished sidecar probe wakes it sooner. |
| VM probe thresholds | VM startup and readiness use the same consecutive-sample streak as a sidecar. Readiness starts false and becomes Ready after `successThreshold` successes (default 1), then stays Ready until `failureThreshold` failures (default 3). Startup stays not-started until `successThreshold` successes; `failureThreshold` failures fail the pod. Liveness restarts the VM after `failureThreshold` failures, and one success clears that streak (`successThreshold` is 1). `initialDelaySeconds`, `periodSeconds`, and `timeoutSeconds` apply to all three. A liveness restart clears the three streaks. The pod still fails after more than 5 liveness restarts. |
| `kubectl logs` of the VM | Via Guest Agent (unified log + agent ring buffer). Not a container runtime log driver. |
| Guest agent session | The agent closes a session after 30s with no frames when no interactive exec is open. The host sends `Health` every 10s, one third of the idle time the guest advertises (`idleMs` on handshake; an older guest that omits it is treated as 30s). A dropped session is dialed again, backoff 1s doubling to 5s, for as long as the pod is running. The VM is not restarted for a connection drop. After 30s of consecutive failure the pod stays `Running` and is not Ready, reason `GuestAgentDisconnected`. `Started` stays true. A later dial clears that reason. No process flag. |
| Exec stdin | A guest that advertises `stdinWindow` (1 MiB) grants those bytes back as the process reads them. The host does not drop frames, and EOF is a frame that waits for credit. A non-TTY exec frees its slot when the process exits, including when the client has not sent EOF. An older guest advertises no window. The host then sends without waiting, and that guest can still drop a full queue. A new guest talking to an older host does not drop stdin; the older host ignores the credit frames. |
| GPU scheduling | `darwin.node/metal` is a token, not isolation. See [gpu.md](gpu.md). |
| hostPath | Shared into the guest via virtio-fs. Semantics ≠ Linux bind-mount. |
| DaemonSets | Must not request a VM slot. This node does not run node-exporter as a VM. Scrape darwin-node `/metrics`. |

## Not yet implemented (tracked)

| Feature | Target |
|---|---|
| PVC / local CSI (APFS clonefile) | Phase 6+ |
| `VirtualMachine` / `VirtualMachineTemplate` CRDs | Phase 6+ |
| containerd sidecar runtime | When needed; Docker is the default |
| zstd image layers | When we bump media types |
| `mount_null` bind in the guest | Phase 4 hardening |
| Pod recreate with state transfer | Phase 6 |

## What *does* work (Phase 1-5 contract)

- Node registration, leases, taints, honest capacity.
- Create/delete pods, status, conditions, events.
- Hybrid pods (VM + host sidecars).
- Guest Agent exec, logs, probes (exec/httpGet/tcpSocket).
- emptyDir, hostPath, ConfigMap, Secret, projected, downwardAPI.
- Image pull with dockerconfigjson, digest verify, clonefile CoW.
- NAT + bridged, hostPort, agent-reported PodIP.
- Graceful stop via agent `Shutdown`, then VZ stop.
- Metrics (Prometheus + kubelet stats summary) and OTLP traces.
- launchd install for the host agent and the guest agent.
