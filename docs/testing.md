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
- Cold-boot identity in `pkg/vmidentity`, and guest Adopt in
  `pkg/vzcontract/uniqueness_test.go` (see below)
- macOS-only admission and the always-on OS taint:
  `pkg/provider/admission_test.go` and `pkg/node/taint_test.go`. The
  rules are in [admission.md](admission.md)
- SimVZ kubelet conformance (`pkg/kubeletconformance`, 25 cases) runs on every `go test`, including `-short` and `-race`. The harness forwards `Stop` with grace 0 and does not take a lock around `Stop`. Cleanup waits only for the boot generation captured when the machine entered `Stopping`. See [kubelet-conformance.md](kubelet-conformance.md) and [simvz.md](simvz.md).
- Guest idle deadline in `pkg/guest`. Tests inject a clock (`idleClock`
  in `pkg/guest/idle.go`). A nil clock is the wall clock. `IdleMS` on
  the wire is unchanged, and no operator setting changed

## Per-VM supervisor

`--vm-supervisor` defaults to `off`. Linux tests drive a supervisor with
the fake runtime over a real Unix socket. They cover the handshake, a protocol-major rejection that holds the
slot instead of stopping the VM, all five RPCs (`StartVM`, `InspectVM`,
`PauseAndSaveVM`, `StopVM`, `AttachChannels`), `SCM_RIGHTS` descriptors
that can be written on one end and read on the other, reattach that
leaves the VM running, a lease mismatch and a deleting pod leading to
`StopVM`, peer-uid rejection, `ENOTCONN` (the connection closes, and
that is not an auth failure), the dead-man timer, and goleak plus
fd-count checks. The adapter's `Recover` and the lease check are in
that set. With `--vm-supervisor` on, `pkg/engine` startup calls that
`Recover` before it deletes unheld pod directories. The metrics wrapper
forwards the call. With the flag off, the wrapper returns no machines
and the node-restart settlement is unchanged. See
[vm-supervisor-recovery.md](vm-supervisor-recovery.md).

A node-agent restart re-adopts through `supervisor.Adopt`
(`pkg/supervisor/session.go`). Adopt dials, fences the lease, and calls
`AttachChannels`. The guest fd is a fresh Connect passed with
`SCM_RIGHTS`. A saved fd is not reused. `pkg/runtime/supervised` calls
Adopt on the first attach and on `Recover`. `Recover` does not spawn a
supervisor. A wire-major mismatch, a supervisor that sends no
`wire_major`, and a protocol-major rejection hold the slot and do not
call `StopVM`. The node is cordoned `SupervisorVersionSkew` /
`WireMajorMismatch`, and made schedulable again only while
`darwin-node.io/cordoned-by=supervisor-skew` is still set. Linux coverage is
`TestWireMajorMismatchHoldsSlotUntilSupervisorExits` and
`TestProtocolMajorMismatchHoldsWithoutStopping` in
`pkg/runtime/supervised`, and
`TestWireMajorMismatchHoldsBudgetAndCordons` in `pkg/engine`. An
equal-major re-adopt whose handshake says the workload has not started
sends the command once (`TestRecoveredWorkloadRunsOnce`). See
[vm-supervisor-versioning.md](vm-supervisor-versioning.md). Supervisor
death becomes `SlotLost` and `VMFailed` with
message `supervisor lost` within about 2 seconds. The watch is
edge-triggered epoll (`EPOLLET`) on Linux and kqueue (`EV_CLEAR`) on
Darwin. It does not wait for the dead-man switch. The contract test
sets that switch to 1 hour; `--vm-supervisor-deadman` still defaults to
5 minutes. The slot is released once. A supervisor that disappears
while the pod is `Running` is reason and message `supervisor lost`.
Restart policy does not boot a replacement. Any other unexpected stop
is still reason `VMFailed` and message `macOS VM Failed unexpectedly`.
`Connect` during `Start` on Virtualization.framework is
hardware-pending.

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

`pkg/supervisor/identity_test.go` (`linux || darwin`) covers a lease
whose `host_boot_id` or `supervisor_proc_start` does not match:
`CheckProcessLease` fails closed. `TestEnforceStopDoesNotKillReusedPID`
in `pkg/runtime/supervised/kill_test.go` leaves that pid running.
Stop budgets are grace plus 10s in the supervisor and grace plus 12s in
the node-agent (`pkg/supervisor/stop_deadline_test.go`,
`TestAgentStopBudget`). `deploy/launchd/node_plist_test.go` requires
`AbandonProcessGroup` and `ProcessType` `Interactive` on the node plist.
The three Mac Mini checks in
[vm-supervisor-hardening.md](vm-supervisor-hardening.md) (fd flatness,
App Nap, `kickstart -k`) are still open.

## VM identity and guest Adopt

`pkg/vmidentity` has no build tag. Linux `go test` covers a cold boot
that discards image `machineIdData`, refuses a missing, empty, or
symlink golden aux, and copies a private `aux.img` that `Release`
removes. On Darwin that copy is `clonefile(2)`. There is no new flag.

Linux coverage of distinct host entropy and guest samples, including a
liveness restart that Adopts again, is
`pkg/vzcontract/uniqueness_test.go` (`TestGuestIdentityUniqueness`).
`TestAdoptReseedFailClosed` in that file rejects a reused sample and
does not start the workload. Ready, then Adopt, then Materialize, then
Workload, is `TestAdoptBeforeMaterializeAndWorkload` in
`pkg/engine/adopt_test.go`. `TestAdoptSampleSetEvictsOldest` shows a
full sample set drops the oldest; the engine's cap is 1024
(`adoptSampleLimit`). These tests run
on Linux. They do not boot Virtualization.framework. The Mac Mini
checklist in [guest-adopt.md](guest-adopt.md) is still open: distinct
`/dev/random` samples across two pods, a guest clock within 500ms, no
`/etc/ssh/ssh_host_*` at workload start, and the pod hostname on
HostName, LocalHostName, and ComputerName. Distinct `IOPlatformUUID`
values for two cold boots of one image are the open list in
[vm-identity.md](vm-identity.md).

`pkg/runtime/simvz` is a separate Linux stand-in that opens real
descriptors for the engine. `--runtime` does not select it. No
operator default changed. Each `Start` or `Restore` installs a boot
generation. `Stop` waits only for
the generation it captured on entry to `Stopping`, so a `Start` after
`Stopped` is published is not joined by that cleanup. The rule is in
[simvz.md](simvz.md). The conformance harness that relies on it is in
[kubelet-conformance.md](kubelet-conformance.md). The supervisor
feature write-up is [vm-supervisor.md](vm-supervisor.md).

## Guest ephemeral keychain

Linux and SimVZ cover the per-job keychain with an in-memory platform
(`pkg/guest.MemoryKeychain`). They do not call Security.framework.
`TestEphemeralKeychainBeforeWorkload` in `pkg/engine/keychain_test.go`
checks Ready, then Adopt, then Materialize, then `EphemeralKeychain`,
then Workload. `pkg/vzcontract/keychain_test.go` fails a create closed.
SimVZ installs the same in-memory directory on the in-process guest
(`pkg/runtime/simvz/keychain.go`). `cmd/guest-agent` on non-darwin
installs it too. `DARWIN_NODE_GUEST_SOFTWARE_PLATFORM=1` selects it on
darwin, which is how the SimVZ child avoids the Mac it is running on.
The `security` checks in [guest-keychain.md](guest-keychain.md) need a
Mac.

## Node certificates

`--rotate-server-certificates` and `--rotate-client-certificates`
default to off. With both off, startup still loads the kubeconfig once
and the kubelet serving files still reload on mtime and size. The CSR
suite in `pkg/nodecert` uses a fake clientset
(`k8s.io/client-go/kubernetes/fake`) and `testing/synctest` as the
certificate manager's clock. It covers serving and client issuance,
renewal in the 70–90% window, a denied CSR, an apiserver outage through
expiry, cert-directory modes, and a symlink that must stay inside the
directory. `TestClientRotationDoesNotDropInflightPodStatus` keeps an
in-flight pod status request on the certificate that started it.
`cmd/darwin-node/certs_test.go` covers startup wiring, including the
flag-off path. Approving a CSR on the Mac Mini, for
`kubernetes.io/kube-apiserver-client-kubelet` and for
`kubernetes.io/kubelet-serving`, is still manual. See
[node-certificates.md](node-certificates.md).

## FuzzNodeLifecycle

`FuzzNodeLifecycle` (`pkg/provider/lifecycle_fuzz_test.go`) drives the
virtual-kubelet provider on SimVZ. Each input is a sequence of pod
create, delete, guest crash, start failure, process restart, and node
drain. The engine probe and start-backoff clock is a virtual clock.
Guest redial still uses the wall clock; the fuzzer shortens that delay
and restores the production values when the input ends. The committed
seeds are `pkg/provider/testdata/fuzz/FuzzNodeLifecycle`.

After every step, after drain, and after shutdown, the test checks six
invariants:

1. Running VMs stay at or below 2. `darwin.node/vm-slot` capacity and
   allocatable stay at 2. After drain, both slots are free and no VM is
   running.
2. When nothing is running, the process fd count is back within 8 of
   the baseline taken before boot (the same slack the soak census uses).
3. `go.uber.org/goleak` is clean once the node is idle, and again after
   the step goroutine has exited.
4. No per-pod directory remains for a pod that is gone, no SimVZ clone
   directory sits outside the live set, no `.tmp` or `.old` snapshot is
   left, and the process has no new child.
5. A pod that has reached Succeeded or Failed does not return to
   Running or Pending. A Never pod, or a pod that was already terminal,
   stays down across a process restart.
6. Boot, each step, and shutdown run under a 5 second watchdog. On
   expiry it dumps every goroutine stack and fails the input.

The fault-free subset, with no guest crash and no start failure, is
`pkg/provider/testdata/fuzz/node_lifecycle_hardware.jsonl`. That list is
the Mac Mini hardware gate. `TestNodeLifecycleHardwareReplay`
(`pkg/provider/hardware_replay_darwin_test.go`, built with
`darwin && arm64`) skips unless `DARWIN_NODE_VZ_HARDWARE=1`. With that
variable set, the test still does not drive Virtualization.framework: it
fails and names the list, so a Mac Mini run is not reported as a pass.
Linux CI does not compile it. This replay is hardware-pending.

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
that checklist. They do not mark it done. The fault-free
`FuzzNodeLifecycle` replay list is
`pkg/provider/testdata/fuzz/node_lifecycle_hardware.jsonl`
(`TestNodeLifecycleHardwareReplay`, `darwin && arm64`). That run is
hardware-pending. See [FuzzNodeLifecycle](#fuzznodelifecycle).

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
