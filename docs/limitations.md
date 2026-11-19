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
| `emptyDir.sizeLimit` | The watch loop sums regular files on the host directory. Over the limit, or a symlink that leaves the volume, the pod fails and the slot is released. The guest can write past the limit between checks. An APFS quota that refuses the write is still future work. `medium: Memory` is a normal directory. |
| ServiceAccount token rotation | Each projected token is minted on its own. A mint error or an empty token fails pod creation. The watch loop refreshes at half the remaining lifetime, rewrites the host file, and re-copies it into the guest. Refresh keeps the last good file until the recorded expiry; after expiry a failed refresh fails the pod. |
| Init containers | Host Docker only, sequential, before the VM starts. A macOS image as an init container is rejected. |
| Crash-looping hybrid sidecar | The container waits in `CrashLoopBackOff` (10s, then 20s, doubling, capped at 5m) with that reason and a `back-off … restarting failed container` message. Same rule as kubelet: the pod stays `Running` and is not failed for looping. It keeps its VM slot until the pod is deleted. Delete during the backoff returns and frees that slot; the backoff does not sleep. Deleting the pod drops that backoff entry. The other slot still admits a pod. The 2-VM cap is not raised. |
| Shutdown during a stuck boot | `Close` returns after 30s and keeps the slot. Process exit then waits up to 5s more. If `Start` returns in that window the VM is stopped and the slot is released once. If it does not, the pod UID or warm slot id is logged and written to `cache/abandoned-boots.json` (mode 0600). The next start deletes that bundle when the slot is not held, and does not pin a slot for it. |
| Host health probe | A failed host probe (disk, CPU, or memory), an unavailable VM runtime, or a cache directory that cannot be written sets `Ready=False`. Memory and disk pressure become `Unknown` only when the host probe itself failed; a runtime or cache failure keeps the last real pressure bits. `Ping` still returns nil so the node status patch and the lease continue. New pods are rejected until the next successful probe. Pods already running are left in place. |
| Failed VM exit code | `PodFailed` publishes `VMStatus.ExitCode` when the runtime set one, including 0. The Virtualization.framework runtime does not set it yet, so a failed macOS VM still reports 1. |
| Hybrid sidecar startup probe | Past the failure threshold the container is killed and the pod restart policy applies. `Always` and `OnFailure` start it again and the pod stays up, holding its VM slot. `Never` fails the pod with the runtime exit code (137 for the kill). A runtime that cannot stop the container still fails the pod and releases the slot. |
| `kubectl logs` of the VM | Via Guest Agent (unified log + agent ring buffer). Not a container runtime log driver. |
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
