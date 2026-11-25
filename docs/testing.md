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
- hostPort conflict detection, and loopback tests for half-close, linger, release on pod teardown, dial timeout, and connection caps (no Apple networking)
- `kubectl attach` returns an error for the VM container and for a sidecar, and does not start a shell
- Named-cache write-back only after exit 0, including the `CacheReadOnly` event when the write annotation is outside `--cache-writers`
- Adopt/delete soak on the fake runtime (`pkg/soak`): both VM slots, cache
  snapshot after exit 0 and restore, fd/dir/slot drift, a refused third VM,
  recovery after an injected start failure
- Per-VM supervisor control socket on Linux, with the fake runtime over
  real Unix sockets (see below). This does not boot launchd or
  Virtualization.framework

## Per-VM supervisor

`--vm-supervisor` defaults to `off`. Linux tests drive a supervisor with
the fake runtime over a real Unix socket. They cover the handshake and a
refused unknown protocol major, all five RPCs (`StartVM`, `InspectVM`,
`PauseAndSaveVM`, `StopVM`, `AttachChannels`), `SCM_RIGHTS` descriptors
that can be written on one end and read on the other, reattach that
leaves the VM running, a lease mismatch and a deleting pod leading to
`StopVM`, peer-uid rejection, `ENOTCONN` (the connection closes, and
that is not an auth failure), the dead-man timer, and goleak plus
fd-count checks. The adapter's `Recover` and the lease check are in
that set. `pkg/engine` does not call `Recover`.

These paths compile for `darwin/arm64` and have not been run on Apple
hardware:

- A file-handle NIC moving frames. The socketpair and
  `VZFileHandleNetworkDeviceAttachment` call are in place. The agent
  does not bridge those frames to host NAT.
- `VirtioSocketDevice.Connect` on the Virtualization queue, and taking
  the unix descriptor out of the unexported `rawConn` field. `Connect`
  has no context. Today's guest dials the host, so `Connect` completes
  when the guest listens.
- `Pause` plus `SaveMachineStateToPath`. The save API is macOS 14. On
  macOS 13 the framework returns an error. The minimum stays macOS 13.
- LaunchAgents in `gui/<uid>`, `launchctl kickstart`, and codesign of
  this one binary. CI checks the plist text in `deploy/launchd/`.
  launchd and the entitlement are not exercised.

`pkg/runtime/simvz` is a separate Linux stand-in that opens real
descriptors for the engine. `--runtime` does not select it. See
[simvz.md](simvz.md). The supervisor feature write-up is
[vm-supervisor.md](vm-supervisor.md).

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

The virtio-fs read-only bit on a named cache is set in unit tests. A
booted macOS guest has not been used to confirm the guest cannot write
through that mount. That check is hardware-pending.

Host-side checks that Linux does run:

- `emptyDir.sizeLimit`: directory usage on the probe interval. Absolute
  and dangling symlinks do not fail the pod. A relative symlink that
  lexically leaves the directory fails it, and the message has no host path
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

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) runs on every push
and pull request. `workflow_dispatch` runs it by hand. `make test` is the
same `go test` line as the two test jobs.

| Check | Runner | What it runs |
|---|---|---|
| `test` | `macos-latest` | `go vet ./...`, `go test ./... -short -count=1 -race -timeout 600s`, license-file checks, `make dist` |
| `test (linux)` | `ubuntu-latest` | `go vet ./...`, `go test ./... -short -count=1 -race -timeout 600s` |
| `kubelet auth envtest` | `ubuntu-latest` | `make test-kubelet-auth` |
| `lint` | `ubuntu-latest` | golangci-lint v2.12.2 with [`.golangci.yml`](../.golangci.yml) |
| `govulncheck` | `ubuntu-latest` | govulncheck v1.7.0. It reports findings. On Go 1.25 the job stays green for the known standard-library and dependency issues whose published fixes need Go 1.26. |
| `e2e (self-hosted)` | `[self-hosted, macOS, ARM64, darwin-node-e2e]` | `go test ./... -count=1 -timeout 120s`, including clonefile. Only on `workflow_dispatch`. No runner is attached, so the job does not run. |

Hosted macOS runners are themselves VMs and cannot boot a macOS guest, so
they never run the hardware gate. A hosted macOS job is sometimes cancelled
with "not acquired by runner" when GitHub has no free runner. Re-run it.
That cancellation is a missing runner, not a failed test. The hardware
gate still needs a bare-metal Mac
([HARDWARE_VALIDATION.md](HARDWARE_VALIDATION.md)).

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
