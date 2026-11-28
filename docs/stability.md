# Stability

This document says exactly what is proven, how it is proven, and what
still stands between this tree and a production fleet. It exists so the
word "alpha" in the README has a precise meaning.

## Verification tiers

| Tier | What it covers | Status |
|---|---|---|
| Unit + race (`make test`) | Engine lifecycle, warm pool, caches (write-back only after exit 0), drain, hostPort proxy lifecycle (half-close, release on pod teardown, dial timeout, connection caps), attach refusal, agent protocol, image format, deltas | Linux CI and hosted macOS CI run this on every push and pull request, with `-race`. A hosted macOS job can be cancelled when GitHub has no runner ("not acquired by runner"), so a push is not guaranteed a finished macOS result. When the job runs, the race detector is part of it |
| Leak discipline (`internal/leakcheck`) | No module goroutines survive engine/guest lifecycle tests | Enforced continuously |
| Failure injection (`pkg/engine/inject_test.go`) | Runtime Create/Start/Dial failures: fail closed, slots freed, state reclaimed, node reusable | Green |
| Adversarial protocol (`pkg/guest/adversarial_test.go`) | Garbage bytes, oversized frames, wrong versions, stray stream frames, unknown methods | Green |
| Hardware gate (`make test-hardware`) | Real Virtualization.framework boot, agent handshake over vsock, exec through PTY, logs, metrics, console socket, graceful delete | Manual command; requires a baked image and a signed binary. Run it on target hardware before any fleet use |
| Adopt/delete soak (`make soak`) | Continuous create/delete of cache-volume pods on every VM slot (never more than 2), watching fd, directory, and slot drift | Short fake-runtime pass is in `go test ./pkg/soak` and `make soak` (Linux, no Virtualization.framework). The 24h Apple Silicon run (`make soak IMAGE=<baked image>`) is still an open gate |
| Production authn (TokenReview / SubjectAccessReview) | kubelet HTTP authentication and authorization | API-server integration test passes (`make test-kubelet-auth`, Linux, `//go:build integration`, kube-apiserver + etcd 1.35). With `ClientCA` set: a client cert signed by that CA is authenticated; a ServiceAccount bearer token is authenticated via TokenReview; SubjectAccessReview allows and denies `nodes/log` and `nodes/proxy` for the node name; anonymous requests are 401; a missing or invalid CA fails closed. Cluster mode refuses to start when `ClientCA` is empty, so that path does not install `nodeutil.NoAuth`. The nil-bundle `NoAuth` wrapper remains covered by unit tests. The client CA file is re-read while the process is up; an empty or invalid replacement keeps the previous bundle (unit tests). The kubelet listen address defaults to `127.0.0.1`. Still open: hardware gate `make test-hardware`, and a 24h `make soak IMAGE=...` result |
| Per-VM supervisor (`--vm-supervisor`, default `off`) | Versioned Unix control protocol, plus `supervisor.Adopt`: a fresh `AttachChannels` Connect over `SCM_RIGHTS`, and supervisor death as `SlotLost` / `VMFailed` `supervisor lost` within about 2s. The watch is edge-triggered epoll or kqueue. The contract test's dead-man is 1 hour; the operator default is 5m. The supervised adapter calls Adopt on first attach and on `Recover`, and releases the slot once. With the flag on, engine startup calls `Recover`: a live supervisor matching `cache/pods/<uid>` is reattached as `Running` (slot held, disk and `control/agent-token` kept, guest redialed, no new VM, restart count and `startTime` kept, init not re-applied, workload sent once only when the handshake says it has not started, sidecars recreated). A wire-major mismatch, a pre-handshake supervisor, or a protocol-major rejection holds the slot and cordons the node; it does not stop the VM. The node is made schedulable again only while `darwin-node.io/cordoned-by=supervisor-skew` is still set, so an operator cordon is left in place. A dead supervisor keeps the node-restart settlement. An orphan supervisor is stopped. A supervisor lost at runtime is reason and message `supervisor lost`, with no replacement boot. Lease JSON includes `host_boot_id` and `supervisor_proc_start`; a lease without them fails closed on re-adopt and that pid is not signalled. Stop waits are termination grace plus 10s in the supervisor and grace plus 12s in the node-agent. A spawned supervisor logs to `<run dir>/slot-N.supervisor.log` (mode 0600) with stdin `/dev/null`. The node plist sets `AbandonProcessGroup` and `ProcessType` `Interactive` | Linux `go test` covers the socket, the SimVZ restart cases, the engine reattach (`pkg/engine`), the lease identity fence, and the stop budgets. LaunchAgents, `launchctl kickstart`, and Virtualization.framework (file-handle NIC frames, vsock `Connect` during `Start`, pause/save on macOS 14) are hardware-pending. The fd-flatness, App Nap, and `kickstart -k` checks are also hardware-pending, so the flag stays default off. With it off, `Recover` returns no machines and the previous node-restart settlement runs. See [vm-supervisor.md](vm-supervisor.md), [vm-supervisor-hardening.md](vm-supervisor-hardening.md), [vm-supervisor-recovery.md](vm-supervisor-recovery.md), [vm-supervisor-versioning.md](vm-supervisor-versioning.md), and [testing.md](testing.md) |
| Per-pod VM identity | Cold boot mints a `MacMachineIdentifier` and clonefiles golden `aux.img` to a pod-private path removed on stop. Image `machineIdData` is left unused. A missing, empty, or symlink golden aux fails closed. No new flag. No vmgenid on restore; save/restore is not implemented | Linux `go test` of `pkg/vmidentity`. Distinct guest `IOPlatformUUID` values are hardware-pending. See [vm-identity.md](vm-identity.md) and [testing.md](testing.md) |
| Guest Adopt | After agent dial and before Materialize and Workload, including on VM restart. A redial does not Adopt again. The host checks the proof and rejects a reused sample (1024 remembered, then the oldest is dropped). `DARWIN_NODE_ALLOW_LEGACY_GUEST` defaults off and skips only a missing `adopt` or `ephemeral-keychain` method. `DARWIN_NODE_GUEST_SOFTWARE_PLATFORM` is for the SimVZ child, not a VM image. Adopt does not rotate the ECID | Linux `pkg/vzcontract/uniqueness_test.go`. The Mac Mini checklist in [guest-adopt.md](guest-adopt.md) is still open |
| Guest ephemeral keychain | Before Workload, each pod gets a fresh user keychain. The password is drawn in the guest. Golden login keychains are hidden. The System keychain is untouched. A dropped-session redial does not install another keychain. A VM restart does. An equal-major re-adopt that still has to send Workload installs it on that call. Missing capability fails the pod unless `DARWIN_NODE_ALLOW_LEGACY_GUEST` is set | Linux and SimVZ use an in-memory platform (`pkg/guest.MemoryKeychain`). The `security` checks in [guest-keychain.md](guest-keychain.md) are hardware-pending |
| Node certificates (`--rotate-server-certificates`, `--rotate-client-certificates`, `--bootstrap-kubeconfig`, `--cert-dir`) | Opt-in CSR bootstrap and rotation through `k8s.io/client-go/util/certificate`. Client rotation waits for `system:node:<nodename>` in `system:nodes` before join. The client transport swaps without dropping an in-flight pod status write. darwin-node does not approve CSRs | The flags default to off. With them off, startup still calls `nodeutil.ClientsetFromEnv` once and the serving files still reload on mtime and size, which is the previous code. Linux `go test` uses a fake clientset and `testing/synctest`. Approving a CSR on a Mac Mini is still manual. See [node-certificates.md](node-certificates.md) and [testing.md](testing.md) |

## What "production ready" requires, in order

1. The hardware gate passes on every Mac model in the target fleet,
   including cold boot after OS updates.
2. A 24 hour soak on Apple Silicon: `make soak IMAGE=<baked image>`
   runs continuous adopt/delete cycles with cache volumes and fails on
   fd, directory, or slot drift. The harness is in tree (`make soak`
   without IMAGE is the short fake-runtime pass). The 24h result itself
   is still required; it has not been recorded for this tree.
3. TokenReview / SubjectAccessReview is covered by `make test-kubelet-auth`:
   client certificate authentication, ServiceAccount bearer TokenReview,
   SubjectAccessReview allow and deny, anonymous 401, and fail-closed on a
   missing or invalid CA. That is the API-server check. The hardware gate
   and the 24h soak are still required before any fleet claim.
4. At least one external operator runs CI on it for a month.

Until those four lines are checked, the honest label is: **alpha,
control-plane verified, hardware pending**.

## Running the hardware gate

The full procedure, including the 24 hour soak and the result sheet, is
[HARDWARE_VALIDATION.md](HARDWARE_VALIDATION.md). The commands below are
the same gate.

```bash
# Bake an image once (needs macOS IPSW):
bin/darwin-image restore --ipsw <path> --out out/macos-15
bin/darwin-image inject-agent --image out/macos-15
bin/darwin-image pack --image out/macos-15 --hardware-model <...>

# Gate:
make test-hardware IMAGE=out/macos-15
```

The gate builds and ad-hoc signs its own binary (Virtualization.framework
entitlements), then reports one line per step:

```
STEP BOOT        OK   boot+agent ready in 48s ip=192.168.64.2
STEP EXEC        OK   stdin/stdout round trip through the agent protocol
...
HARDWARE GATE: PASS
```

Without IMAGE set it prints SKIP and exits zero, so CI can invoke it
unconditionally; pass `--strict` to fail instead. A boot of about 48s
fits in the default `--agent-ready-timeout` of 2m. A dial that still
fails is retried; the pod stays Pending until it succeeds or the error
is permanent. This gate does not boot a guest to confirm that a
read-only named-cache mount rejects writes. That check is still
hardware-pending. The snapshot gate itself (commit only after exit 0)
is covered by the unit tier.

## Running the soak

`make soak` is the adopt/delete harness. Each cycle fills every configured
VM slot (never more than 2) with a pod that declares a cache volume and
opts into write-back, waits until the guest agent is running, writes a
marker into that cache, exits 0 so the trusted snapshot commits, deletes
the pod, and checks:

- slot occupancy is back to zero, and one more create while the slots were
  full was refused with `VMCapacityExhausted`
- no pod or warm directories were left behind, and cache snapshots did not
  leave `.tmp` or `.old` entries or grow the store's file count
- process file descriptors and darwin-node goroutines stayed at the
  pre-cycle baseline (fds allow a small slack)

The marker is written on the host directory that virtio-fs shares into the
guest. That is the same directory a guest write lands in. The soak
namespace is on `--cache-writers` and the pod sets the write annotation,
so exit 0 commits it. A delete while the workload is still running does
not. The ready file the hardware guest waits on is `/tmp/darwin-soak-ready`,
outside the cache, so the next cycle does not restore it. Cache-volume
pods cannot adopt a warm VM
(shares are fixed at boot; see [warm-pool.md](warm-pool.md)), so the soak
leaves the warm pool off and cold-boots. Both slots stay available to the
cycle. Warm adoption is covered by `pkg/engine` tests.

### Short run (any OS, fake runtime)

```bash
make soak
make soak SOAK_CYCLES=8
```

This does not need an image, a signature, or Virtualization.framework.
`go test ./pkg/soak` runs the same checks. CI already includes that
package via `go test ./...`.

### 24 hour run (Apple Silicon, baked image)

```bash
bin/darwin-image restore --ipsw <path> --out out/macos-15
bin/darwin-image inject-agent --image out/macos-15
bin/darwin-image pack --image out/macos-15 --hardware-model <...>

make soak IMAGE=out/macos-15
```

`IMAGE` selects the vz runtime and a 24h budget. Each pod uses the engine
default of 2 vCPU and 4Gi, so a full cycle is two guests. When `codesign`
is on `PATH`, the soak binary is ad-hoc signed with the
Virtualization.framework entitlements before it starts. A shorter hardware
smoke:

```bash
make soak IMAGE=out/macos-15 SOAK_CYCLES=1
make soak IMAGE=out/macos-15 SOAK_DURATION=30m
```

A green `make soak` without `IMAGE` does not substitute for the 24h result.
