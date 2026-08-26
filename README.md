# darwin-node

Native macOS workloads on Kubernetes, on Apple Silicon, as pods.

> **Alpha.** This is early software. Most of the implementation was
> written with coding agents and is **pending review and hardware
> testing**. Unit tests and a fake runtime cover the control plane;
> signed Virtualization.framework boots, IPSW restore, and live
> multi-GB image pulls are not a release gate yet. Do not run it as
> the kubelet for a production Mac fleet. APIs, flags, and the guest
> protocol may change. Provided **AS IS**, with no warranty; you assume
> all risk (see [License](#license)).

`darwin-node` is a node agent for M-series Macs. It joins a cluster as a
Kubernetes node and runs **at most two** macOS virtual machines at a
time through Apple's
[Virtualization.framework](https://developer.apple.com/documentation/virtualization).
Optional Linux sidecars run on the host next to the VM. Inside every
guest, a **Guest Agent** (`bin/darwin-guest-agent`, launchd) is the
control plane for exec, logs, probes, metrics, and shutdown, spoken as a
length-prefixed JSON protocol over vsock. TCP is an opt-in fallback,
SSH a last resort; no guest login session is ever used.

> **Hard cap:** two concurrent macOS VMs per physical host (Apple EULA
> and XNU `hv_apple_isa_vm_quota`). Capacity, taints, and admission are
> built around that number. A third pod is rejected, not queued.

Intel Macs, nested virtualization, and exclusive GPU passthrough are
out of scope. See [docs/limitations.md](docs/limitations.md).

## Design invariants

- `pods=2` and `darwin.node/vm=2` are real slots from a fail-closed
  table ([pkg/capacity](pkg/capacity/capacity.go)). Full means the node
  taints itself `darwin.node/vm-full=true:NoSchedule`; it never queues.
- The guest boundary is a daemon speaking vsock, not port 22. Exec is
  full duplex (stdin, PTY, resize); probes run in-guest; shutdown is an
  RPC, not an SSH kill.
- Hybrid pods: `containers[0]` is the macOS VM, `containers[1..]` are
  host Docker sidecars, init containers run host-side only.
- Operate-first defaults: TLS required on the kubelet HTTP server,
  hostPath default-deny behind an allowlist, image digests verified,
  on-disk pod state recovered after a crash, stats report measured
  usage rather than advertised capacity.

## Capabilities beyond the upstream lineage

| Capability | Flag / API | Deep dive |
|---|---|---|
| Warm VM pool: matching pods adopt a pre-booted guest | `--warm-slots`, `--warm-image` | [docs/warm-pool.md](docs/warm-pool.md) |
| Cache volumes that survive the pod (CoW restore + snapshot) | `cache.darwin.node/<name>: <guest-path>` annotations | [docs/cache-volumes.md](docs/cache-volumes.md) |
| Interactive exec: stdin into a guest PTY, resize, true `logs -f` | standard `kubectl exec -it` / `logs -f` | [docs/architecture.md](docs/architecture.md) |
| Break-glass serial console independent of agent and SSH | `--serial-console`, `darwin-node console` | [docs/console.md](docs/console.md) |
| Delta image updates: digest-verified CoW patch apply | `darwin-image delta-create` / `delta-apply` | [docs/delta-images.md](docs/delta-images.md) |

One paragraph each:

**Warm VM pool.** A replenisher keeps up to N pre-booted, agent-ready
guests in slots pods do not need. Adoption matches on image ref and
adds zero boot work; under capacity pressure real demand reclaims warm
slots before any pod is rejected. Warm guests hold real slots from the
same table that fails closed at two.

**Cache volumes.** Annotate a pod and the node restores each declared
path from its cache store via APFS clonefile before boot, shares it
read-write over virtio-fs at exactly that path, and re-snapshots on
graceful delete. DerivedData stays warm across CI runs with no storage
infrastructure.

**Delta images.** Monthly Xcode bumps become megabyte-scale patches:
4 MiB chunk diffs against a pinned base, applied copy-on-write and
verified end to end against the SHA-256 of the whole target disk.

## Lineage

The first public implementation of this idea is Agoda's
[macOS-vz-kubelet](https://github.com/agoda-com/macOS-vz-kubelet)
(Apache License 2.0, 2024): a Virtual Kubelet provider whose pods are
Virtualization.framework VMs, with host sidecars beside them, disks
cloned with APFS `clonefile`, and macOS images shipped as OCI artifacts.
This tree exists because three of its central defaults are inverted here:

1. **The guest is a peer, not an SSH target.** Upstream drives exec,
   readiness, and graceful shutdown over port 22 with credentials from
   environment variables. darwin-node ships a launchd daemon baked into
   every image, serving the same surface over vsock with a
   token-authenticated TCP fallback. There is now a control plane
   inside the guest with its own binary, lifecycle, and version skew.
2. **Capacity is enforced, not queued.** Upstream advertises `pods=2`
   but blocks a third create until a slot frees. darwin-node rejects
   the third pod, taints the node while both slots are held, and drives
   admission through an explicit slot table.
3. **Operability is a default, not a flag.** Plaintext HTTP will not
   start; digest verification and crash recovery are unconditional;
   usage stats feed autoscaling instead of static capacity numbers.

None of these are additive features; each rewrites a load-bearing
contract, which is why this is a separate Go module that honors
upstream's license rather than edits beside someone else's defaults.
Where ideas still align (Virtual Kubelet as the Kubernetes surface,
hybrid pods, clonefile overlays, OCI disk artifacts) they are shared
deliberately, and Agoda-format OCI artifacts remain readable so
existing registries migrate without a flag day. The complete
attribution record is in [NOTICE](NOTICE) and
[docs/credits.md](docs/credits.md).

Thank you to Agoda for publishing that work.

## Quick start

Requires an Apple Silicon Mac and **Go 1.25+**.

```bash
make build
make sign    # ad-hoc Virtualization.framework entitlement
```

| Binary | Role |
|---|---|
| `bin/darwin-node` | Host node agent |
| `bin/darwin-guest-agent` | In-guest daemon (bake into images) |
| `bin/darwin-image` | IPSW restore, agent inject, OCI pack/pull, delta create/apply |

No cluster (engine plus fake runtime):

```bash
./bin/darwin-node --runtime=fake --standalone --allow-nat-workloads --nodename=debug-mac
# capacity pods=2 vm=2 ...
```

Join a cluster (TLS required; the kubelet HTTP server refuses plaintext):

```bash
export APISERVER_CERT_LOCATION=$HOME/.kube/tls.crt
export APISERVER_KEY_LOCATION=$HOME/.kube/tls.key
# optional: APISERVER_CA_CERT_LOCATION for mTLS client auth

./bin/darwin-node --nodename="$(hostname | tr '[:upper:]' '[:lower:]')" --allow-nat-workloads
kubectl get node
# Capacity: pods=2, darwin.node/vm=2, darwin.node/metal=2
```

Hardware workloads need `--runtime=vz` (the default on `darwin/arm64`),
a signed binary, and a baked macOS image (`darwin-image restore`,
`inject-agent`, `pack`, or an OCI pull). Example manifests:
[examples/pod.yaml](examples/pod.yaml),
[examples/pod-hybrid.yaml](examples/pod-hybrid.yaml),
[examples/pod-cache.yaml](examples/pod-cache.yaml).

Full install, launchd, entitlements, Helm:
[docs/installation.md](docs/installation.md).

## How it works

```
Kubernetes API
      |
      v
+-------------------------------------+
| darwin-node  (Virtual Kubelet)      |
|   provider -> engine -> runtime/vz  |
|   sidecars on host Docker           |
|   capacity: 2 VM slots, fail-closed |
|   warm pool: pre-booted, adoptable  |
|   cache store: CoW snapshots        |
+------------------+------------------+
                   | vsock (primary)
                   | TCP  (opt-in fallback)
                   | SSH  (last resort)
                   v
+-------------------------------------+
| macOS VM  (< 2 per host)            |
|   darwin-guest-agent (launchd)      |
|   paravirtual Metal (shared GPU)    |
+-------------------------------------+
```

- **GPU**: paravirtual Metal (`VZMacGraphicsDevice`), shared, never
  PCIe passthrough. Labels tell the truth. [docs/gpu.md](docs/gpu.md)
- **Volumes**: emptyDir, hostPath (allowlisted), ConfigMap, Secret,
  projected, downwardAPI. Host materializes; virtio-fs shares; the
  agent links or copies into the guest.
- **Networking**: NAT (laptop/CI) or bridged (Services). hostPort is a
  userspace TCP proxy. PodIP comes from the Guest Agent.
- **Images**: Darwin-Node media types, Agoda-format artifacts accepted
  on read, per-pod CoW overlays, delta updates on top of cached bases.
- **Console**: optional serial port per VM for break-glass access.

## Kubernetes surface

| Area | Status |
|---|---|
| Node Ready, capacity, taints, labels | Yes |
| Pod create / delete / status / conditions | Yes |
| Fail-closed 2-VM admission | Yes |
| Warm VM pool + adoption | Yes (matching image, mount-free primary) |
| Pod cache snapshots | Yes (annotation-declared, CoW) |
| Interactive `kubectl exec -it` (stdin, PTY, resize) | Yes (vsock) |
| True `logs -f` follow | Yes |
| Break-glass serial console | Yes (opt-in flag) |
| Delta image updates | Yes (digest-verified apply) |
| Hybrid sidecars (mounts + resource requests) | Yes |
| Host-side init containers | Yes |
| In-guest probes (exec / httpGet / tcpSocket) | Yes |
| Image pull secrets, digest verify, clonefile CoW | Yes |
| NAT, bridged, TCP hostPort | Yes |
| Metrics-server summary + Prometheus `/metrics` | Yes |
| OpenTelemetry traces | Yes (OTLP env) |
| PVC / CSI | Not shipped (cache volumes cover the main demand) |
| In-place pod updates | Recreate |
| More than 2 VMs, nested VMs, Intel, GPU passthrough | No |

## Documentation

| Doc | What it is |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the pieces fit, package map |
| [docs/warm-pool.md](docs/warm-pool.md) | Warm VM lifecycle, adoption rules, eviction |
| [docs/cache-volumes.md](docs/cache-volumes.md) | Cache annotation spec and snapshot store |
| [docs/console.md](docs/console.md) | Serial console socket, CLI, trust model |
| [docs/delta-images.md](docs/delta-images.md) | Patch format, apply algorithm, ops model |
| [docs/installation.md](docs/installation.md) | Build, sign, launchd, Helm |
| [docs/security.md](docs/security.md) | Threat model and fail-closed defaults |
| [docs/limitations.md](docs/limitations.md) | What Kubernetes this is not |
| [docs/testing.md](docs/testing.md) | Unit tests, fake runtime, hardware e2e |
| [docs/gpu.md](docs/gpu.md) | Shared Metal |
| [docs/credits.md](docs/credits.md) | Lineage and third-party licenses |
| [NOTICE](NOTICE) / [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES) | Agoda attribution; Go-module licenses |
| [docs/adr/](docs/adr/0000-index.md) | Architecture decision records |

Visual debug without a cluster:

```bash
./bin/darwin-node debug-dump -o debug-snapshot.json
open debug.html    # file:// - plain <script src>, not an ES module
```

## Layout

```
cmd/darwin-node      host agent (+ console subcommand)
cmd/guest-agent      in-guest agent
cmd/darwin-image     bake / pack / pull / delta
pkg/provider         Virtual Kubelet adapter
pkg/engine           pod lifecycle, warm pool, caches, console bridge
pkg/runtime          vz + fake (Consoler for serial)
pkg/guest            agent protocol, transports, PTY exec
pkg/image            OCI pull, overlays, deltas
pkg/capacity         2-VM slot table
deploy/              launchd, Helm, Homebrew formula
examples/            pod manifests
```

This is the darwin-node repository (standalone, not nested under
macOS-vz-kubelet).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). `make test` is the unit-test
entry point; it does not boot a VM.

## License

Apache License 2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE), and
[THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES). Release archives (`make dist`)
must ship all three next to the binaries.

This software is provided on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. You are solely
responsible for deciding whether to use it and you assume all risk.
The authors and contributors are not liable for damages of any kind
arising from use (or inability to use) this software, including data
loss, cluster disruption, or hardware issues, even if advised that
such damage was possible, except where applicable law requires
otherwise. That is sections 7 and 8 of [LICENSE](LICENSE) in plain
language. Do not run this as production kubelet infrastructure unless
you accept that.
