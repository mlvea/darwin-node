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
| More than 2 concurrent macOS VMs per host | Apple EULA + XNU `hv_apple_isa_vm_quota`. Third VM → `VZErrorVirtualMachineLimitExceeded`. |
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
| `emptyDir.sizeLimit` | Best-effort accounting, not APFS quota (yet). |
| ServiceAccount token rotation | Re-materialized on provider resync, not kubelet's projected-volume refresher. |
| Init containers | Host Docker only, sequential, before the VM starts. A macOS image as an init container is rejected. |
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

## What *does* work (Phase 1–5 contract)

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
