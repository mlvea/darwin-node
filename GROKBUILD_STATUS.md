# GrokBuild status — darwin-node

Owner: Madushan via GrokBuild
Harness: Grok Build CLI · model `grok-4.7` · effort `xhigh`
Clone: `/workspace/repos/darwin-node`

## Goal
Find bugs and push the idea toward its **penultimate** (near-final) version: production-credible native macOS workloads on Kubernetes (Apple Silicon, ≤2 VMs/host), without pretending hardware gates already ship.

## Log

### 2026-09-30 ~08:57–09:05 SGT — TokenReview S003
- **HEAD (start):** `20596bc` on `main` (local reflection; origin/main = `5a93de1`)
- **HEAD (end):** this commit on `grokbuild/tokenreview-s003` (local only, not pushed)
- **What changed:**
  1. **Kubelet routes:** `runVK` creates `http.NewServeMux`, sets `nc.Handler`, and passes `nodeutil.AttachProviderRoutes(mux)` into `NewNode` so exec/logs/stats can register.
  2. **Auth, fail-closed:** `kubeletAuth` installs `nodeutil.WebhookAuth` (TokenReview + SubjectAccessReview) only when `ClientCA` is set, loading that file with `dynamiccertificates.NewDynamicCAContentFromFile("client-ca", ...)`. Empty `ClientCA` installs `nodeutil.NoAuth` (anonymous after TLS). A missing or invalid CA fails the opt and does not fall back to NoAuth.
  3. **Tests:** `cmd/darwin-node/kubelet_auth_test.go` covers mode selection, NoAuth with no CA file, webhook opt build against a fake clientset plus a temp CA PEM (anonymous request is 401), nil client, and missing/invalid CA.
  4. **Docs:** `docs/security.md` and the S003 row in `docs/stability.md`. Wiring is unit-tested; a real API-server soak is still required before a production authn claim. `k8s.io/apiserver` is now a direct module.
- **Compile / tests (Linux box):**
  - `go build ./...` OK
  - PASS `go test -count=1 ./cmd/darwin-node/` and the same with `-race`
- **PR / branch:** `grokbuild/tokenreview-s003` is local only. Do not push from this session.
- **Grok CLI:** `grok-4.7` / `xhigh` implemented the wiring (~9.5m); parent verified tests and handled push/PR.

### Open risks
- Hardware gate (`make test-hardware`) still unrun on real Apple Silicon; Darwin single-shot `clonefile` CoW not runtime-tested here
- TokenReview / SubjectAccessReview is wired and unit-tested only. No live API-server proof that the apiserver mTLS client is allowed, bearer TokenReview succeeds, or SAR denies other callers. Without `ClientCA`, the kubelet HTTP handler stays anonymous after TLS
- Soak (24h adopt/delete + cache volumes) still required before any "production ready" claim per `docs/stability.md`
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local
- Local `gh` CLI still unauthenticated (parent agent pushes via GitHub MCP)

### Next day priority (Thu 2026-10-01)
1. API-server soak / e2e of TokenReview + SubjectAccessReview (mTLS client allowed, anonymous denied, SAR allow/deny). Do not claim production authn until that passes
2. Optional: Darwin-host smoke of cache CoW / `make test-hardware` when a Mac runner is available
3. Defer the 24h adopt/delete soak until that auth e2e or the hardware gate has a first result

### 2026-09-29 ~08:55–08:57 SGT — weekday daily reflection
- **HEAD (start):** `8fe42a7` on `grokbuild/fail-closed-hardening` (= origin tip; local clean)
- **HEAD (end):** `5a93de1` on `main` (PR #1 squash-merged)
- **What changed today:**
  1. **CI confirm:** both Darwin `test` jobs on `8fe42a7` were success (push run ~2m + merge-ref ~4m); `e2e (self-hosted)` skipped; `mergeable_state=clean`.
  2. **Merged** [PR #1](https://github.com/mlvea/darwin-node/pull/1) via squash → `5a93de1` on `main` ("Fail-closed hardening: digest, volumes, warm pool, guest, image, debug (#1)").
  3. **Local verify:** `go test -count=1` PASS for `./pkg/engine/ ./pkg/guest/ ./internal/leakcheck/ ./internal/digest/ ./pkg/volume/ ./pkg/hostport/ ./pkg/image/ ./pkg/debug/` on Linux box.
- **No new code this reflection** beyond the merge — standing TokenReview/soak hold lifted only after merge landed.
- **Grok CLI:** not needed; CI + mergeable state were decisive.

### Open risks
- Hardware gate (`make test-hardware`) still unrun on real Apple Silicon; Darwin single-shot `clonefile` CoW not runtime-tested here
- TokenReview / SubjectAccessReview authn (TODO S003) still unimplemented — now unblocked to start
- Soak (24h adopt/delete + cache volumes) still required before any "production ready" claim per `docs/stability.md`
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local
- Local `gh` CLI still unauthenticated (MCP `user-GitHub-xai` as mlvea used for merge/push)

### Next day priority (Wed 2026-09-30)
1. Start TokenReview (S003) design/implementation behind a clear fail-closed path, or document the exact kubelet auth surface to wire
2. Optional: Darwin-host smoke of cache CoW / `make test-hardware` when a Mac runner is available
3. Defer soak until TokenReview skeleton exists or hardware gate has a first PASS


### 2026-09-28 ~08:51–09:05 SGT — weekday daily reflection
- **HEAD (start):** `f44d976` on `grokbuild/fail-closed-hardening` (= origin tip)
- **HEAD (end):** this reflection commit on `grokbuild/fail-closed-hardening` (after push)
- **What changed today:**
  1. **CI discovery:** branch-tip push on `f44d976` was green; merge-ref PR run (`d2fb8de`) failed `TestTCPFallbackAfterExhaustedVsockBudget` with `ReadFrame` leakcheck (0.24s). Flake under Darwin merge-ref timing.
  2. **Root cause:** `Engine.teardown` nulls `rec.agent` but never `Close()`s it (unlike `restartVM`), so `Session.readLoop` stays in `ReadFrame` until the peer idles out. Leakcheck also exited early on `len(after) <= len(before)` while a new leak signature was still winding down.
  3. **Fix:** always `agent.Close()` in teardown; wait on the leak *set* in `leakcheck.Check`; fallback test joins Serve via WaitGroup after cancel/close (IdleTimeout 200ms).
- **Compile / tests (Linux box):**
  - `go build ./...` OK
  - PASS `go test -count=10 ./pkg/engine/ -run 'TestTCPFallback|TestFailStops'`
  - PASS `go test -count=1 ./pkg/engine/ ./pkg/guest/ ./internal/leakcheck/`
- **PR / branch:** https://github.com/mlvea/darwin-node/pull/1 — push re-triggers CI; e2e self-hosted still skipped
- **Grok CLI:** not needed; root cause clear from teardown vs restartVM + CI log

### Open risks
- PR #1 Darwin `test` must go green on merge ref after this commit
- Hardware gate (`make test-hardware`), TokenReview authn (TODO S003), and soak remain alpha blockers per `docs/stability.md`
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local
- Darwin single-shot directory `clonefile` path not runtime-tested on this Linux box
- Local `gh` CLI still unauthenticated (MCP `user-GitHub-xai` as mlvea used for push)

### Next day priority (Tue 2026-09-29)
1. Confirm PR #1 Darwin `test` green on merge ref; merge if review-ready
2. Optional: Darwin-host smoke of cache CoW / `make test-hardware` when a Mac runner is available
3. Do **not** start TokenReview (S003) or soak until the fail-closed PR is merged

### 2026-09-25 ~08:54–09:05 SGT — weekday daily reflection
- **HEAD (start):** local had rewritten SHAs vs GitHub; synced to `origin/grokbuild/fail-closed-hardening` @ `6de200e`
- **HEAD (end):** this reflection commit on `grokbuild/fail-closed-hardening` (after push)
- **What changed today:**
  1. **Discovery:** [PR #1](https://github.com/mlvea/darwin-node/pull/1) is already open (opened 24 Sep via Cursor agent). Prior status was stale — write auth is no longer the blocker.
  2. **CI:** macOS `test` job on the merge ref failed once on `TestFailStopsMachineAndFreesSlot` (`TempDir` cleanup: `pods/uid-next` directory not empty). Parallel branch-tip run was green — flake from Delete racing async `Create`/`start` overlay writes.
  3. **Fix:** `podRecord.startDone` WaitGroup; `Delete` waits after teardown before snapshot/`RemoveAll`. Test waits for `next` → Running before Delete.
- **Compile / tests (Linux box):**
  - PASS `go test -count=1 ./pkg/engine/ ./pkg/guest/` (+ earlier core package suite green)
- **PR / branch:** https://github.com/mlvea/darwin-node/pull/1 — mergeable but `unstable` until CI re-runs green; e2e self-hosted skipped
- **Grok CLI:** not needed; root cause was clear from CI logs

### Open risks
- PR #1 CI must go green on Darwin (this fix targets the known flake)
- Hardware gate (`make test-hardware`), TokenReview authn (TODO S003), and soak remain alpha blockers per `docs/stability.md`
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local
- Darwin single-shot directory `clonefile` path not runtime-tested on this Linux box
- Local `gh` CLI still unauthenticated (MCP `user-GitHub-xai` as mlvea used for push)

### Next day priority (Mon 2026-09-28)
1. Confirm PR #1 Darwin `test` job green after this commit; merge if review-ready
2. Optional: Darwin-host smoke of cache CoW / `make test-hardware` when a Mac runner is available
3. Do **not** start TokenReview (S003) or soak until the fail-closed PR is merged

### 2026-09-24 (catch-up; status file was not updated that morning)
- Commit `40143ca` / remote `6de200e`: Wait for idle watchdog on Serve exit (`pkg/guest`)
- PR #1 opened with fail-closed hardening (3 commits on top of `main` @ `2bec4b0`)

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
