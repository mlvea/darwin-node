# Testing

## What unit tests cover (any machine)

```bash
make test-short
```

- Capacity acquire/release, fail-closed third VM, max-vms clamp
- Guest protocol framing, handshake, exec/probe request types
- Volume materialization (emptyDir, hostPath, ConfigMap, Secret, projected)
- Pod status mapping (phases, conditions, Ready gating on agent)
- Image config parse (Darwin-Node + Agoda media types)
- Node capacity math (reserved CPU/memory)
- Fake runtime lifecycle (create/start/stop/IP)
- hostPort conflict detection (no real bind in short tests)

## What requires Apple Silicon hardware

Tagged `//go:build darwin && arm64` and skipped under `-short` when a
helper detects missing entitlements:

| Test | Needs |
|---|---|
| VZ configuration validate | Virtualization.framework, codesigned test binary |
| clonefile overlay | APFS volume |
| vsock round-trip | a running macOS VM with the agent |
| IPSW restore | an IPSW, 30–60+ minutes, lots of disk |
| Bridged IP | vmnet entitlement, real L2 |
| Metal selftest | graphics device + guest agent |

`make test` runs unit tests including clonefile on Darwin. It does **not**
boot a VM.

## End-to-end (self-hosted Mac)

Prerequisites: signed `darwin-node`, a baked image, a cluster, Docker for
sidecars.

```bash
# from the Mac that will be the node
make sign
./bin/darwin-node --nodename=e2e-mac --allow-nat-workloads

kubectl apply -f examples/pod.yaml
kubectl wait --for=condition=Ready pod/macos --timeout=15m
kubectl exec macos -c macos -- /usr/bin/uname -a
kubectl logs macos -c macos --tail=50
```

Hybrid:

```bash
kubectl apply -f examples/pod-hybrid.yaml
```

Documented failure: a third pod must become Failed `VMCapacityExhausted`.

## CI

## Visual debug (no cluster)

```bash
./bin/darwin-node debug-dump -o /tmp/debug-snapshot.json
# writes /tmp/debug-snapshot.json and /tmp/snapshot.js
open web/debug.html   # or copy debug.html + debug.js next to snapshot.js and open file://
```

`web/debug.html` uses plain `<script src>` (not ES modules) so `file://` works.

`.github/workflows/ci.yml` at this repository root:

- `go test ./... -short` on `macos-latest` (compile + unit)
- `go vet`
- No VM boot on GitHub-hosted runners (Virtualization.framework is not a
  reliable nested target). Real e2e is `workflow_dispatch` on a self-hosted
  `macos-arm64` runner labelled `darwin-node-e2e`.
