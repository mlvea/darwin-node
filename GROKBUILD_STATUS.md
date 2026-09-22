# GrokBuild status — darwin-node

Owner: Madushan via GrokBuild
Harness: Grok Build CLI · model `grok-4.7` · effort `xhigh`
Clone: `/workspace/repos/darwin-node`

## Goal
Find bugs and push the idea toward its **penultimate** (near-final) version: production-credible native macOS workloads on Kubernetes (Apple Silicon, ≤2 VMs/host), without pretending hardware gates already ship.

## Log

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

### Open risks
- Large uncommitted mid-flight diff still needs human / PR review before merge; no branch isolation yet
- `pkg/debug` missing `web/debug.html` (suite not fully green end-to-end)
- Hardware gate (`make test-hardware`), TokenReview authn (TODO S003), and soak remain alpha blockers per `docs/stability.md`
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local (sidecar not portable across restarts — intentional for cache; expected digest still required for trust)
- Darwin directory `clonefile(2)` single-shot CoW path replaced by walk+per-file `Dir` for caches — correct on Linux, slightly less optimal on APFS vs previous `File` on dirs

### Next day priority (Wed 2026-09-23)
1. Carve overnight+reflection work onto a feature branch; split or one PR with clear summary; run `make test` / race where feasible
2. Fix or quarantine `pkg/debug` / restore `web/debug.html` so full `./...` is honest
3. Optional: restore Darwin-optimized directory clone for caches (`File` on darwin, `Dir` elsewhere) without regressing Linux CI
4. Do **not** start another 80-turn sprawl until this pass is branched + reviewed

### Earlier
- 2026-09-22 ~01:02 SGT: clone at `2bec4b0`; first Grok 4.7 xhigh pass starting (stub; incomplete — see above).
