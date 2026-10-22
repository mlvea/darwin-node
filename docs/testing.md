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
- Adopt/delete soak on the fake runtime (`pkg/soak`): both VM slots, cache
  snapshot/restore, fd/dir/slot drift, a refused third VM, recovery after
  an injected start failure

## Soak

```bash
make soak                  # fake runtime, 4 cycles, any OS
make soak SOAK_CYCLES=8
make soak IMAGE=out/macos-15                 # 24h, Apple Silicon, baked image
make soak IMAGE=out/macos-15 SOAK_CYCLES=1   # one real cycle
```

`make soak` without `IMAGE` is the CI-safe path: it does not boot
Virtualization.framework. The 24h command is the production gate from
[stability.md](stability.md). Details and the drift checks live there.

## What requires Apple Silicon hardware

The commands, the expected output, and the result sheet are in
[HARDWARE_VALIDATION.md](HARDWARE_VALIDATION.md). Run that checklist
once the Mac is connected. In short:

```bash
make test
make test-hardware IMAGE=out/macos-15
make soak IMAGE=out/macos-15 SOAK_CYCLES=1
make soak IMAGE=out/macos-15
```

`make soak` with no `IMAGE`, and `go test` on Linux, stay green without
that checklist. They do not mark it done.

Host-side checks that Linux does run:

- `emptyDir.sizeLimit`: directory usage on the probe interval, symlink
  escape fails the pod and frees the slot
- Projected ServiceAccount tokens: one token per projection, empty and
  failed mints refused, refresh rewrites the file before expiry

Tagged `//go:build darwin && arm64` and skipped under `-short` when a
helper detects missing entitlements:

| Test | Needs |
|---|---|
| VZ configuration validate | Virtualization.framework, codesigned test binary |
| clonefile overlay | APFS volume |
| vsock round-trip | a running macOS VM with the agent |
| IPSW restore | an IPSW, 30-60+ minutes, lots of disk |
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
mkdir -p web && cp pkg/debug/assets/debug.html pkg/debug/assets/debug.js web/
open web/debug.html   # or open the copies written next to snapshot.js
```

Embed source of truth is `pkg/debug/assets/` (`go:embed`). Top-level `web/` is a
gitignored local convenience for `file://` browsing. The page uses plain
`<script src>` (not ES modules) so `file://` works.

`.github/workflows/ci.yml` at this repository root:

- `go test ./... -short` on `macos-latest` and `ubuntu-latest` (compile + unit)
- `go vet` on both
- `make test-kubelet-auth` on `ubuntu-latest`
- No VM boot on GitHub-hosted runners (Virtualization.framework is not a
  reliable nested target). Real e2e is `workflow_dispatch` on a self-hosted
  `macos-arm64` runner labelled `darwin-node-e2e`.
