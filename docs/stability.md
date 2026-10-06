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
| Production authn (TokenReview / SubjectAccessReview) | kubelet HTTP authentication and authorization | API-server integration test passes (`make test-kubelet-auth`, Linux, `//go:build integration`, kube-apiserver + etcd 1.35). With `ClientCA` set: a client cert signed by that CA is authenticated; a ServiceAccount bearer token is authenticated via TokenReview; SubjectAccessReview allows and denies `nodes/log` and `nodes/proxy` for the node name; anonymous requests are 401; a missing or invalid CA fails closed. Empty `ClientCA` stays `nodeutil.NoAuth` (unit tests). Still open: hardware gate `make test-hardware`, 24h adopt/delete soak |

## What "production ready" requires, in order

1. The hardware gate passes on every Mac model in the target fleet,
   including cold boot after OS updates.
2. A soak run: 24 hours of continuous adopt/delete cycles with cache
   volumes, watching for fd/dir/slot drift.
3. TokenReview / SubjectAccessReview is covered by `make test-kubelet-auth`:
   client certificate authentication, ServiceAccount bearer TokenReview,
   SubjectAccessReview allow and deny, anonymous 401, and fail-closed on a
   missing or invalid CA. That is the API-server check. The hardware gate
   and the 24h soak are still required before any fleet claim.
4. At least one external operator runs CI on it for a month.

Until those four lines are checked, the honest label is: **alpha,
control-plane verified, hardware pending**.

## Running the hardware gate

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
