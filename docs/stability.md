# Stability

This document says exactly what is proven, how it is proven, and what
still stands between this tree and a production fleet. It exists so the
word "alpha" in the README has a precise meaning.

## Verification tiers

| Tier | What it covers | Status |
|---|---|---|
| Unit + race (`make test`) | Engine lifecycle, warm pool, caches, drain, agent protocol, image format, deltas | Green on every push; race detector clean |
| Leak discipline (`internal/leakcheck`) | No module goroutines survive engine/guest lifecycle tests | Enforced continuously |
| Failure injection (`pkg/engine/inject_test.go`) | Runtime Create/Start/Dial failures: fail closed, slots freed, state reclaimed, node reusable | Green |
| Adversarial protocol (`pkg/guest/adversarial_test.go`) | Garbage bytes, oversized frames, wrong versions, stray stream frames, unknown methods | Green |
| Hardware gate (`make test-hardware`) | Real Virtualization.framework boot, agent handshake over vsock, exec through PTY, logs, metrics, console socket, graceful delete | Manual command; requires a baked image and a signed binary. Run it on target hardware before any fleet use |
| Adopt/delete soak (`make soak`) | Continuous create/delete of cache-volume pods on every VM slot (never more than 2), watching fd, directory, and slot drift | Short fake-runtime pass is in `go test ./pkg/soak` and `make soak` (Linux, no Virtualization.framework). The 24h Apple Silicon run (`make soak IMAGE=<baked image>`) is still an open gate |
| Production authn (TokenReview / SubjectAccessReview) | kubelet HTTP authentication and authorization | API-server integration test passes (`make test-kubelet-auth`, Linux, `//go:build integration`, kube-apiserver + etcd 1.35). With `ClientCA` set: a client cert signed by that CA is authenticated; a ServiceAccount bearer token is authenticated via TokenReview; SubjectAccessReview allows and denies `nodes/log` and `nodes/proxy` for the node name; anonymous requests are 401; a missing or invalid CA fails closed. Empty `ClientCA` stays `nodeutil.NoAuth` (unit tests). Still open: hardware gate `make test-hardware`, and a 24h `make soak IMAGE=...` result |

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
unconditionally; pass `--strict` to fail instead.

## Running the soak

`make soak` is the adopt/delete harness. Each cycle fills every configured
VM slot (never more than 2) with a pod that declares a cache volume, waits
until the guest agent is running, writes a marker into that cache, deletes
the pod, and checks:

- slot occupancy is back to zero, and one more create while the slots were
  full was refused with `VMCapacityExhausted`
- no pod or warm directories were left behind, and cache snapshots did not
  leave `.tmp` or `.old` entries or grow the store's file count
- process file descriptors and darwin-node goroutines stayed at the
  pre-cycle baseline (fds allow a small slack)

The marker is written on the host directory that virtio-fs shares into the
guest. That is the same directory a guest write lands in, and it is what
graceful delete snapshots. Cache-volume pods cannot adopt a warm VM
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
