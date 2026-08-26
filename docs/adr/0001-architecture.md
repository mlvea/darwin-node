# ADR 0001: Core architecture: enhanced Virtual Kubelet + VM runtime + Guest Agent

- Status: Accepted
- Date: 2026-08-23
- Phase: 0

## Context

We need a production node agent that presents Apple Silicon Macs as Kubernetes nodes
and runs native macOS workloads as Pods. The Agoda `macOS-vz-kubelet` is a useful
baseline: Virtual Kubelet provider, Virtualization.framework (via Code-Hex/vz),
hybrid pods (VM + Docker sidecars), OCI disk images, clonefile CoW.

It is not production-grade as-is:

- Guest control is SSH-only (exec, readiness, stats, shutdown).
- Kubernetes probes are not evaluated.
- VM container logs are unsupported.
- Capacity advertises host CPU/memory in full, with `pods=2` and a *queueing*
  semaphore, overscheduled pods hang instead of failing closed.
- Node conditions are hardcoded healthy.
- Secrets are not mounted; ConfigMaps only as projected sources.
- IP discovery is ARP/pcap only.
- No vsock, no guest agent, no installable launchd contract beyond an example plist.

Three architectural families were considered.

## Options

### A. Custom kubelet (fork kubernetes/kubelet)

Full CRI + kubelet semantics. Enormous surface, constant rebase cost, and the
Linux CRI model does not map onto a 2-VM Apple runtime. Rejected.

### B. CRI shim under a stock kubelet (containerd/CRI-O style)

Would give the best API compatibility for *Linux* sidecars, but macOS VMs are
not OCI runtime spec containers. Hybrid pods (one VM + N host containers) would
need a fake sandbox and a custom runtime. Stock kubelet still assumes Linux
cgroups, CNI, and pod PID namespaces. Rejected for Phase 1-6; revisit only if
Apple ships a CRI-shaped runtime.

### C. Enhanced Virtual Kubelet provider + internal VM runtime + Guest Agent (chosen)

Virtual Kubelet already implements the node-lease, pod-sync, kubelet HTTP
(exec/logs/stats), and auth webhook surface. We keep that as the Kubernetes
facing API and put the real complexity in an internal **macOS VM Runtime** that
owns Virtualization.framework, plus a **Guest Agent** that owns in-guest work.

Optional CRDs (`VirtualMachine`, `VirtualMachineTemplate`) can sit *on top* of
the same runtime later without rewriting the provider.

## Decision

darwin-node is three cooperating programs plus a library:

1. **`darwin-node`**, host node agent. Virtual Kubelet provider. Owns node
   identity, capacity, pod lifecycle, sidecars, image cache, host networking,
   observability.
2. **`darwin-guest-agent`**, in-guest daemon, started by launchd. Owns exec,
   logs, probes, metrics, secret/config materialization, graceful shutdown, IP
   reporting.
3. **`darwin-image`**, image bake/pack/inject tooling (IPSW restore, agent
   injection, OCI push/pull).

Internal layering (host):

```
cmd/darwin-node
  └─ pkg/provider     Virtual Kubelet PodLifecycleHandler (thin)
       └─ pkg/engine  Pod lifecycle, restart policy, hooks, probes, events
            ├─ pkg/runtime   VM create/start/stop (VZ or fake)
            ├─ pkg/guest     Agent client (vsock / TCP / SSH)
            ├─ pkg/sidecar   Host Docker/containerd containers
            ├─ pkg/volume    Host-side materialization + virtio-fs share list
            ├─ pkg/image     OCI pull, digest, clonefile overlay
            ├─ pkg/net       NAT/bridged, IP, hostPort proxy
            └─ pkg/capacity  Hard 2-VM accounting, fail-closed
```

The runtime interface is the extension point. A future CRD controller would
call `pkg/runtime` and `pkg/image` directly, bypassing the Pod API.

## Consequences

- Broad Pod API compatibility without maintaining a kubelet fork.
- Honest feature gaps (no cgroups, no privileged Linux semantics inside the VM)
  are documented rather than faked.
- Tests can run against `pkg/runtime/fake` on any machine; VZ code is
  `darwin && arm64`.
- Hybrid pods remain first-class: container[0] is always the macOS VM.

## Agoda decisions we are *not* preserving

- Silent permit queue when both VM slots are taken.
- SSH as the only guest control plane.
- Allocatable == Capacity with no host reservation.
- Fire-and-forget DeletePod goroutine that then force-deletes the Pod object.
- Overlay clones in `os.TempDir()`.
- Importing `k8s.io/kubernetes` solely for event reason constants.
