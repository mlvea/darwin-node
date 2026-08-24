# darwin-node

**Native macOS workloads on Kubernetes — on Apple Silicon, as pods.**

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
time, using Apple’s
[Virtualization.framework](https://developer.apple.com/documentation/virtualization).
Optional Linux sidecars run on the host next to the VM. Inside every
guest, a **Guest Agent** is the control plane for exec, logs, probes,
and shutdown — vsock first, not a login session.

> **Hard cap:** two concurrent macOS VMs per physical host (Apple EULA
> and XNU `hv_apple_isa_vm_quota`). Capacity, taints, and admission are
> built around that number. A third pod is rejected, not queued.

Intel Macs, nested virtualization, and exclusive GPU passthrough are
out of scope. See [docs/limitations.md](docs/limitations.md).

## Why this project exists

Apple Silicon can run macOS as a first-class VM, at near-native speed,
on the same machine that already has Xcode, Metal, and the signing
stack CI needs. Kubernetes still expects a *node*: honest capacity,
probes, exec, logs, secrets, and a scheduler that is told the truth.

darwin-node is that node. The shape came from asking what a production
kubelet-equivalent looks like when the runtime is two
Virtualization.framework guests instead of a container engine:

- **Admission that matches physics.** `pods=2` and `darwin.node/vm=2`
  are real slots. Full means `NoSchedule`, not a hidden queue.
- **A Guest Agent as the in-guest kubelet surface.** Exec, logs,
  readiness/liveness, metrics, and shutdown speak a length-prefixed
  protocol over vsock (TCP is opt-in; SSH is last resort).
- **Hybrid pods that CI actually uses.** `containers[0]` is the macOS
  VM; `containers[1..]` are host Docker sidecars (logs, artifacts,
  caches) with volume mounts and resource requests.
- **A kubelet contract you can operate.** TLS on the exec/logs port,
  hostPath default-deny, image provenance, crash recovery of on-disk
  pod dirs, and stats that report *usage*, not capacity.
- **Pods that start like containers.** A warm VM pool pre-boots idle
  guests into free slots (`--warm-slots`), and matching pods adopt a
  running VM instead of cold-booting one.
- **Caches that survive the pod.** Annotation-declared cache volumes
  restore via APFS `clonefile` before boot and re-snapshot on delete:
  DerivedData that stays warm across CI runs, with no PVC and no CSI.

## Two things no SSH-era node can do

**Warm VM pool: pod startup without the cold boot.** Every pod on an
SSH-driven macOS node pays a full macOS boot before the first command
runs. darwin-node can keep up to two pre-booted, agent-ready guests idling in
free slots (`--warm-slots=1 --warm-image=registry.example.com/macos:15`);
a pod whose image matches adopts one and is Running in seconds. The
pool obeys every invariant: warm VMs hold real slots from the same
fail-closed table, the pool only fills capacity pods don't need right
now, and real demand evicts a warm guest *before* any pod is rejected.
Only pods whose primary container mounts nothing can adopt — virtio-fs
shares are fixed at VM creation.

```bash
./bin/darwin-node --runtime=vz --allow-nat-workloads \
  --warm-slots=1 --warm-image=registry.example.com/macos:15
```

**Cache snapshots: warm builds with zero storage infrastructure.**
Annotate a pod and the node does the rest:

```yaml
metadata:
  annotations:
    cache.darwin.node/derived-data: "/Users/mac/Library/Developer/Xcode/DerivedData"
    cache.darwin.node/spm: "/Users/mac/Library/Caches/org.swift.swiftpm"
```

Before the VM boots, each declared path is restored from the node's
cache store as an APFS clone — instant, copy-on-write. The directory is
shared read-write over virtio-fs at exactly that guest path, so the
build writes straight through to host disk. On graceful delete the
final state is cloned back into the store for the next pod. No PVC, no
CSI, no registry round-trip — and no equivalent anywhere in the
upstream lineage, whose guests are pristine and reachable only over
SSH. See [examples/pod-cache.yaml](examples/pod-cache.yaml).

## Lineage

The first public yes to this idea is Agoda’s
[macOS-vz-kubelet](https://github.com/agoda-com/macOS-vz-kubelet)
(Apache License 2.0, 2024): a Virtual Kubelet provider whose pods are
Virtualization.framework VMs, with host sidecars beside them, disks
cloned with APFS `clonefile`, and macOS images shipped as OCI
artifacts.

That yes is why this repo exists. darwin-node is the next question:
what that node looks like when upstream's three central defaults are
inverted.

**1. The guest is a peer, not an SSH target.** Upstream reaches into
the VM over SSH: exec sessions, the readiness probe, even graceful
shutdown dial port 22 with credentials supplied through environment
variables. darwin-node ships `bin/darwin-guest-agent`, a launchd
daemon baked into every image (`darwin-image inject-agent`), serving
exec, logs, probes, metrics, and shutdown over a length-prefixed
protocol on vsock — a channel reachable only from the host process —
with a token-authenticated TCP fallback and SSH kept as last resort.
There is now a control plane *inside* the guest, with its own binary,
lifecycle, and version-skew story.

**2. Capacity is enforced, not queued.** Both trees respect Apple's
two-guest ceiling (XNU `hv_apple_isa_vm_quota`) and advertise `pods=2`.
But upstream blocks a third create until a slot frees — the pod waits.
darwin-node fails closed: the third pod is rejected
(`VMCapacityExhausted`), the node taints itself
`darwin.node/vm-full=true:NoSchedule` while both slots are held, and
admission goes through an explicit slot table (`pkg/capacity`). The
scheduler is never left holding a promise the hardware cannot keep.

**3. Operability is a default, not a flag.** The kubelet HTTP server
refuses to start plaintext; hostPath is default-deny behind an
allowlist; pulled images are digest-verified; on-disk pod state is
recovered after an agent crash; and stats report measured *usage*, so
autoscaling reacts to what the VMs consume rather than the static
numbers the node advertises.

None of these are additive features. Each rewrites a load-bearing
contract — where the guest boundary lies, what admission means when a
slot is gone, which guarantees arrive on by default — which is the
shape of a fresh Go module honoring upstream's license, not edits
bolted beside someone else's defaults. Where the ideas still align —
Virtual Kubelet as the Kubernetes surface, hybrid pods, APFS
`clonefile` overlays, OCI disk artifacts — they are shared
deliberately, and Agoda-format artifacts are still accepted on read,
so existing registries and images migrate without a flag day.

The Apache-2.0 record — the derived-file list, adapted algorithms, and
license obligations — is in [NOTICE](NOTICE) and
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
| `bin/darwin-image` | IPSW restore, agent inject, OCI pack/pull |

**No cluster** (engine + fake runtime):

```bash
./bin/darwin-node --runtime=fake --standalone --allow-nat-workloads --nodename=debug-mac
# capacity pods=2 vm=2 …
```

**Join a cluster** (TLS required; the kubelet HTTP server will not
start plaintext):

```bash
export APISERVER_CERT_LOCATION=$HOME/.kube/tls.crt
export APISERVER_KEY_LOCATION=$HOME/.kube/tls.key
# optional: APISERVER_CA_CERT_LOCATION for mTLS client auth

./bin/darwin-node --nodename="$(hostname | tr '[:upper:]' '[:lower:]')" --allow-nat-workloads
kubectl get node
# Capacity: pods=2, darwin.node/vm=2, darwin.node/metal=2
```

Hardware workloads need `--runtime=vz` (the default on `darwin/arm64`),
a signed binary, and a baked macOS image (`darwin-image restore` /
`inject-agent` / `pack`, or an OCI pull). Example manifests:
[examples/pod.yaml](examples/pod.yaml),
[examples/pod-hybrid.yaml](examples/pod-hybrid.yaml),
[examples/pod-cache.yaml](examples/pod-cache.yaml).

Full install, launchd, entitlements, Helm:
[docs/installation.md](docs/installation.md).

## How it works

```
Kubernetes API
      │
      ▼
┌─────────────────────────────────────┐
│ darwin-node  (Virtual Kubelet)      │
│   provider → engine → runtime/vz    │
│   sidecars on host Docker           │
│   capacity: 2 VM slots, fail-closed │
│   warm pool: pre-booted, adoptable  │
│   cache store: CoW snapshots        │
└──────────────┬──────────────────────┘
               │ vsock (primary)
               │ TCP  (opt-in fallback)
               │ SSH  (last resort)
               ▼
┌─────────────────────────────────────┐
│ macOS VM  (≤ 2 per host)            │
│   darwin-guest-agent (launchd)      │
│   paravirtual Metal (shared GPU)    │
└─────────────────────────────────────┘
```

- **GPU** is paravirtual Metal (`VZMacGraphicsDevice`), shared, never
  PCIe passthrough. Labels tell the truth. [docs/gpu.md](docs/gpu.md)
- **Volumes**: emptyDir, hostPath (allowlisted), ConfigMap, Secret,
  projected, downwardAPI. Host materializes; virtio-fs shares; the
  agent places copies in the guest.
- **Networking**: NAT (laptop/CI) or bridged (Services). hostPort is a
  userspace TCP proxy. PodIP comes from the Guest Agent.
- **Images**: Darwin-Node OCI media types; Agoda-format artifacts are
  still accepted on read so existing registries keep working.
- **Warm pool & caches**: pre-booted guests adopt matching pods;
  `cache.darwin.node/*` annotations persist guest paths across pods via
  CoW snapshots. Details in "Two things no SSH-era node can do" above.

## Kubernetes surface

| Area | Status |
|---|---|
| Node Ready, capacity, taints, labels | Yes |
| Pod create / delete / status / conditions | Yes |
| Fail-closed 2-VM admission | Yes |
| Warm VM pool + adoption (`--warm-slots`) | Yes (matching image; no primary mounts) |
| Pod cache snapshots (`cache.darwin.node/*` annotations) | Yes — CoW restore/snapshot |
| Hybrid sidecars (mounts + resource requests) | Yes |
| Host-side init containers | Yes |
| In-guest probes (exec / httpGet / tcpSocket) | Yes |
| Exec / logs via Guest Agent | Yes (stdout/stderr; follow/TTY still maturing) |
| Image pull secrets, digest verify, clonefile CoW | Yes |
| NAT, bridged, TCP hostPort | Yes |
| Metrics-server summary + Prometheus `/metrics` | Yes |
| OpenTelemetry traces | Yes (OTLP env) |
| PVC / CSI | Not shipped |
| In-place pod updates | Recreate |
| Interactive `kubectl exec -it` / `logs -f` | Partial — see limitations |
| >2 VMs, nested VMs, Intel, GPU passthrough | No |

## Documentation

| Doc | What it is |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the pieces fit |
| [docs/installation.md](docs/installation.md) | Build, sign, launchd, Helm |
| [docs/security.md](docs/security.md) | Threat model and fail-closed defaults |
| [docs/limitations.md](docs/limitations.md) | What Kubernetes this is *not* |
| [docs/testing.md](docs/testing.md) | Unit tests, fake runtime, hardware e2e |
| [docs/gpu.md](docs/gpu.md) | Shared Metal |
| [docs/credits.md](docs/credits.md) | Lineage and third-party licenses |
| [NOTICE](NOTICE) / [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES) | Agoda attribution; Go-module licenses |
| [docs/adr/](docs/adr/0000-index.md) | Architecture decision records |

Visual debug (no cluster):

```bash
./bin/darwin-node debug-dump -o debug-snapshot.json
open debug.html    # file:// — plain <script src>, not an ES module
```

## Layout

```
cmd/darwin-node      host agent
cmd/guest-agent      in-guest agent
cmd/darwin-image     bake / pack / pull
pkg/provider         Virtual Kubelet adapter
pkg/engine           pod lifecycle
pkg/runtime          vz + fake
pkg/guest            agent protocol + vsock/TCP/SSH
pkg/capacity         2-VM slot table
deploy/              launchd, Helm, Homebrew formula
examples/            pod manifests
```

This is the darwin-node repository (standalone; not nested under
macOS-vz-kubelet).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). `make test` is the unit-test
entry point; it does not boot a VM.

## License

Apache License 2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE), and
[THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES). Release archives (`make dist`)
must ship all three next to the binaries.

This software is provided on an **“AS IS” BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND**, either express or implied. You are solely
responsible for deciding whether to use it and you assume all risk.
The authors and contributors are not liable for damages of any kind
arising from use (or inability to use) this software, including data
loss, cluster disruption, or hardware issues, even if advised that
such damage was possible — except where applicable law requires
otherwise. That is [LICENSE](LICENSE) §§ 7–8 in plain language. Do not
run this as production kubelet infrastructure unless you accept that.
