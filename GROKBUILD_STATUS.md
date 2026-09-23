# GrokBuild status — darwin-node

Owner: Madushan via GrokBuild
Harness: Grok Build CLI · model `grok-4.7` · effort `xhigh`
Clone: `/workspace/repos/darwin-node`

## Goal
Find bugs and push the idea toward its **penultimate** (near-final) version: production-credible native macOS workloads on Kubernetes (Apple Silicon, ≤2 VMs/host), without pretending hardware gates already ship.

## Log

### 2026-09-23 ~09:23–09:30 SGT — weekday daily reflection
- **HEAD (start):** `b2e9cad` on `grokbuild/fail-closed-hardening` (clean; 1 commit ahead of `main`/`origin/main` @ `2bec4b0`)
- **HEAD (end):** this reflection commit on `grokbuild/fail-closed-hardening` (clean after commit)
- **What changed today:**
  1. **`pkg/debug`:** `TestDebugJSIsNotAModule` now requires embed source `assets/` and only checks gitignored top-level `web/` when that directory exists (matches `.gitignore` "local debug UI" + `docs/testing.md`). Suite passes on Linux.
  2. **Docs:** `docs/testing.md` documents copying `pkg/debug/assets/*` into local `web/` for `file://` browsing.
  3. **Caches:** restored Darwin-optimized directory clone via `cloneCacheDir` — `clonefile.File` (single-shot APFS dir CoW) on Darwin with fall-through to `clonefile.Dir`; Linux always uses `Dir` (no `copy_file_range` on directories).
- **Compile / tests (Linux box):**
  - `go build ./...` OK
  - PASS `go test -count=1 ./pkg/debug/ ./internal/digest/ ./pkg/engine/ ./pkg/volume/ ./pkg/hostport/ ./pkg/image/ ./pkg/guest/`
  - Darwin-only APFS CoW single-shot path not exercised here; hardware gate not run
- **PR / branch:** still on `grokbuild/fail-closed-hardening`; **PR still blocked** (`gh` not authenticated for write — do not push from this box)
- **Grok CLI:** not needed this pass; edits were small and clear

### Open risks
- Feature branch not yet on GitHub / no PR (write auth still missing)
- Hardware gate (`make test-hardware`), TokenReview authn (TODO S003), and soak remain alpha blockers per `docs/stability.md`
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local (sidecar not portable across restarts — intentional for cache; expected digest still required for trust)
- Darwin single-shot directory `clonefile` path is compile-covered via `runtime.GOOS` but not runtime-tested on this Linux box

### Next day priority (Thu 2026-09-24)
1. Authenticate `gh` as mlvea (device OAuth) → push `grokbuild/fail-closed-hardening` + open PR against `main` with clear fail-closed summary
2. After PR: optional Darwin-host smoke of cache CoW (`cloneCacheDir` single-shot) and/or `make test-hardware` when a Mac runner is available
3. Do **not** start another sprawling exploration until the PR is up for review

### 2026-09-22 ~09:23–09:41 SGT — weekday daily reflection
- **HEAD:** `2bec4b0` on `main` (= `origin/main`): "Add stability pass: leak detection, failure injection, adversarial protocol, hardware gate"
- **Open GitHub issues:** none
- **Overnight uncommitted pass (first Grok 4.7 xhigh, hit 80-turn cap):** still local-only on `main` working tree — **27 modified + 4 untracked** Go files (~+1.58k/−142 after today's fixes). Themes:
  - Guest idle watchdog + interactive exec stream byte retention / overloaded-exec unpin
  - Warm pool overlay reaped on adopt/evict; hostPort conflict does not leak adopted VM
  - Digest sidecar same-size overwrite hardening (inode/ctime/mtime + process-local MAC)
  - Console fan-out reaches every client; `ConsoleSocketPath(ns, name)` arity fix
  - Volume subPath is the share (not a guest suffix); symlink/secret escape + `subPathExpr` reject
  - Image delta path-segment / length / offset fail-closed; hostPort range checks
  - TCP fallback after exhausted vsock dial budget (`fallback_test.go`, vz dial)
  - New platform stamp helpers: `internal/digest/stamp_{darwin,linux,other}.go`
- **Compile:** `go build ./...` OK on Linux box
- **Tests this reflection (Linux box):**
  - PASS: `./internal/digest/ ./pkg/volume/ ./pkg/hostport/ ./pkg/image/ ./pkg/guest/ ./pkg/engine/` (`go test -count=1`)
  - FAIL (pre-existing / unrelated to overnight): `./pkg/debug` — `TestDebugJSIsNotAModule` wants missing `web/debug.html`
  - Darwin-only / APFS CoW paths exercised via Linux copy fallback; hardware gate not run here
- **Today's completion (manual after grok CLI stalled ~7m with no edits):**
  1. **Digest:** bind cheap content fingerprint (first+last 4KiB SHA-256) into identity MAC so same-size overwrite fails fast path on overlayfs (timestamps often do not bump)
  2. **Caches:** `prepareCaches` / `snapshotPodCaches` use `clonefile.Dir` for directory trees (Linux `File`→`copyFile` failed with `copy_file_range: is a directory`; was also broken on clean `2bec4b0` on this box)
  3. **Guest Serve:** idle watchdog closes `rw` on `ctx.Done()` so `ReadFrame` unblocks (fixes TCP-fallback leakcheck); fallback test tracks/closes accepted conns + short IdleTimeout
- **PR / branch:** none — work remains **local-only uncommitted on `main`**. Not ready to push until overnight pass is reviewed as one coherent PR on a feature branch.
- **Grok CLI:** attempted `--model grok-4.7 --reasoning-effort xhigh -p …`; process stalled after locating code; finished fixes manually. OAuth still preferred (no API key).

### Earlier (closed out by 2026-09-22 evening branch carve + this reflection)
- Open risks / next priorities from Wed morning were addressed: branch `grokbuild/fail-closed-hardening` exists; `pkg/debug` honest on Linux; Darwin cache CoW path restored with Linux-safe fallback. Remaining: push/PR + hardware.

### Earlier
- 2026-09-22 ~01:02 SGT: clone at `2bec4b0`; first Grok 4.7 xhigh pass starting (stub; incomplete — see above).
