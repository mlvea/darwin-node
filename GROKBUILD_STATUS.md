# GrokBuild status — darwin-node

Owner: Madushan via GrokBuild
Harness: Grok Build CLI · model `grok-4.7` · effort `xhigh`
Clone: `/workspace/repos/darwin-node`

## Goal
Find bugs and push the idea toward its **penultimate** (near-final) version: production-credible native macOS workloads on Kubernetes (Apple Silicon, ≤2 VMs/host), without pretending hardware gates already ship.

## Log

### 2026-11-13 (Asia/Singapore) — startup reset, sidecar probes, exec exit, crash backoff

- **HEAD:** `main` is `6646bb40` (PRs #13, #14, and #16 merged 12 Nov; CI green on that commit). PR #11 (`cursor/ca-reload-second-signal-66c0`, client CA reload and a second signal calling `Engine.Close`) is open and unmerged. This branch does not edit signal handling, kubelet auth, the client CA, `TLSConfig`, or the docs PR #11 touches. Hardware gate and the 24h vz soak are still unrun. No Mac worker is online.
- **Started (confirmed):** `watch` kept a local `startupOK` that stayed true, and `restartVM` never cleared `rec.startupDone`. `buildStatusLocked` reports `Started` from `startupDone && agentOK && Running`, so a liveness restart left `ContainerStatus.Started` true and the startup probe did not run again.
- **Sidecar startup (confirmed):** nothing executed a sidecar startup probe. `sidecarStarted` was true as soon as the container was running, so `Started` and `PodReady` did not wait. Sidecars are host containers, so the probe cannot go through the guest agent.
- **Docker exec (confirmed):** `Docker.Exec` copied the hijacked stdout and returned nil. It did not call `ContainerExecInspect`. A non-zero exec, an inspect error, and an exec that was still running all looked like success. The client is the unexported `dockerAPI`, so this was tested without a daemon.
- **Crash loop (confirmed):** `restartExitedSidecars` called `Create` on every watch tick. A sidecar that exited on every create was restarted once per probe interval (20ms in the engine tests) with no delay, and the pod kept one of the two VM slots.
- **Init statuses (confirmed):** `runInitContainers` ran the container and `waitSidecarExit` already had the exit, and `buildStatusLocked` never set `InitContainerStatuses`. A successful init and an init that exited 9 both published an empty slice. The failed pod's message already contained `exited 9`.
- **Not a dropped exit code:** every `PodFailed` main container still reports exit 1. `buildStatusLocked` hardcodes that 1. `types.VMStatus` has no exit code, and the runtime does not record one, so there is no real code to publish. Per-reason codes would be a new policy. Left unchanged.
- **vz.Stop (confirmed, not changed):** when `m.vm.Stop()` returns an error, `Stop` returns before `m.listener.Close()` and `m.console.Close()`, and it does not set `finished`. The file is `//go:build darwin && arm64` and does not compile on this Linux host. Closing the listener and the console on that error path, without setting `finished` and without changing the success path, is the safe edit. It was not made because it cannot be compiled or run here.
- **Fix:** A liveness restart of a main container that has a startup probe clears `startupDone` and the failure count before the new agent is stored, sets `ready` false with reason `StartupProbe` and message `container restarted`, and records `startupSince` so the initial delay applies again. Pod `StartTime` stays `rec.startedAt`. The next watch tick reads `startupDone` off the record and runs the startup probe. Liveness and readiness wait until it passes. When it passes and there is no readiness probe, `ready` becomes true and a `StartupProbe` reason is cleared, so the pod can become Ready again. A main container with no startup probe keeps `Started` true across the restart. The liveness restart limit is still more than 5, then the pod fails.
- Sidecar startup probes run on the watch, against the host container. Exec uses `Runtime.Exec` (a non-nil error is failure). HTTP GET does not follow redirects; 200–399 is success and the body is drained up to 4KiB. TCP dials from the host. While the sidecar is running and the probe has not passed, `Started` and `Ready` stay false, and `PodReady` / `ContainersReady` use reason `StartupProbe` and message `sidecar <name> <msg>`. A sidecar that is not running still uses `SidecarNotRunning`. Past the failure threshold the pod fails and the VM slot is released, the same as the VM startup probe. Cancelling the context does not count as a failure. The result is keyed by the container's `StartedAt`, so a new start time runs the probe again. A sidecar with no startup probe keeps the previous `Started` rule: running, or terminated with a start time.
- Repeated sidecar exits use `client-go` `flowcontrol.NewBackOff(10s, 300s)` with no jitter, the same helper kubelet uses. The first exit restarts immediately. `Next` then waits 10s, 20s, 40s, and so on, capped at 5 minutes. The entry resets when the next exit is more than twice that cap after the last update. The key is the pod UID, `/`, and the container name. During the wait the container is `Waiting` with reason `CrashLoopBackOff` and message `back-off <duration> restarting failed container=<name> pod=<ns>/<name>`. `LastTerminationState` keeps the exit (`Error`, or `Completed` when the code is 0). `RestartCount` increments only when `Create` runs. The pod stays `Running` and not Ready. The VM is not restarted and the slot stays held. A `BackOff` warning is emitted when that message changes. A zero `FinishedAt` is latched by container id and exit code so the window can elapse, and the latch moves after a restart. `OnFailure` with exit 0, and `Never`, do not restart and do not show `CrashLoopBackOff`. A running sidecar clears the waiting reason. A `Create` error still fails the pod and releases the slot.
- `Docker.Exec` calls `ContainerExecInspect` after copying stdout. A non-zero exit returns `exit code N`. An inspect error returns `exec inspect: ...`. An exec that is still running returns `exec still running`. Exit 0 returns the copy error, or nil. `dockerAPI` gained `ContainerExecInspect`. `*client.Client` already has that method. `sidecar.Runtime.Exec` is unchanged.
- `waitSidecarExit` returns the sidecar status it observed. That status is stored on the pod and copied onto `PodStatus.InitContainerStatuses`. Exit 0 is reason `Completed` and `Ready`. A non-zero exit is reason `Error` and keeps the exit code and the sidecar error string. A status the runtime never returned is not recorded.
- The 2-VM cap, the CLI flags, and the public method signatures are unchanged. `sidecar.Runtime` did not grow a method.
- **Tests that failed first:** `TestLivenessRestartResetsStartedUntilStartupPasses`: `liveness restart left Started true before the startup probe passed again` (RestartCount 1, Started true, boots=2). `TestSidecarStartupProbeGatesStarted`: `sidecar startup probe was not executed and Started was already true`. `TestSidecarStartupProbeFailureReleasesSlot`: phase stayed `Running` for 5s, want `Failed`. `TestSidecarHTTPStartupProbeHitsTheHost`: `HTTP startup probe was not executed and Started was already true`. `TestSidecarTCPStartupProbeDialsTheHost`: `TCP startup probe was not executed and Started was already true`. `TestDockerExecSurfacesExitCode/nonzero`: `err <nil>, exec exit 7 was not surfaced`. `/zero`: `inspect calls 0`. `/inspect_error`: `err <nil>, inspect failure was swallowed`. `/still_running`: `exec still running returned nil`. `TestHybridSidecarCrashLoopBacksOff`: `sidecar restarted 3 times with no backoff` in 0.03s. `TestInitContainersRun` and `TestInitContainerFailureKeepsExitCode`: `InitContainerStatuses []` (the failure case already had `exited 9` in the pod message). `TestSidecarStartupProbeRerunsAfterRestart` passed on its first run; the `StartedAt` generation check was part of the startup fix, so it was not a separate red. `TestTerminatedSidecarStaysStarted` now uses `OnFailure` so an exit of 0 stays `Terminated`; under `Always` that exit restarts and then waits in `CrashLoopBackOff`.
- **Verification (Linux):** `go vet ./...` PASS. `go test ./... -short -race -count=1 -timeout 600s` PASS (`pkg/engine` 44.878s). `make soak` PASS (4 cycles, 67ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`). `make test-kubelet-auth` was not run; those files were not edited.
- **Left alone:** PR #11's files, other than this log. Every `PodFailed` main container still reports exit 1, as above. `vz.Stop` still returns before closing the vsock listener when `vm.Stop` errors. Sidecar liveness and readiness probes are not executed. Probe `PeriodSeconds` is still the watch interval. A sidecar startup probe that passes its failure threshold fails the whole pod, because `sidecar.Runtime` has no kill and the VM startup probe already fails the pod. A sidecar that keeps exiting holds its VM slot until the pod is deleted; there is no sidecar restart limit. A crash between `commitCacheDir`'s two renames, `NodeReady` on a failed host probe, and the virtual-kubelet lease while the API server is down are unchanged from the 2026-11-12 entry.
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
  - PR #11 is unmerged. This file will conflict at the top of the log when the second of #11 and this branch merges; keep both entries, this one above the 2026-11-12 entry.
  - A crash-looping sidecar occupies one of the two VM slots for as long as the pod exists. The cap itself is unchanged.
  - A sidecar whose startup probe never passes fails the pod and frees the slot. Kubelet would kill that container and leave the pod.
- **Next priority:**
  1. Hardware gate `make test-hardware IMAGE=...` and the 24h vz soak. Still blocked on a Mac.
  2. On a Mac, close the vsock listener and the serial console in `vz.Stop` when `vm.Stop` returns an error, and leave `finished` unset on that path.
  3. Publish a real main-container exit code on `PodFailed` only if the runtime starts recording one. Do not invent per-reason codes.



- **HEAD:** `main` is `543faa9b` (PR #12 merged 9 Nov). This branch stacks on PR #14 (`cursor/restart-exec-started-fail-closed-d8e8`, `a9fd3cc`), which stacks on PR #13. PR #11 (`cursor/ca-reload-second-signal-66c0`, client CA reload and a second signal calling `Engine.Close`) is open and unmerged. This branch does not edit signal handling, kubelet auth, the client CA, `TLSConfig`, or the docs PR #11 touches. Hardware gate and the 24h vz soak are still unrun. No Mac worker is online.
- **Sidecar restart (confirmed):** a hybrid sidecar that exited under `RestartPolicyAlways`, or under `OnFailure` with a non-zero status, was left terminated. The watch did not call `Create` again. The pod stayed `Running` and not Ready, `RestartCount` stayed 0, and the VM kept one of the two slots. `OnFailure` with exit 0 already stayed down; that test passed before this change. `Memory.Create` appended a second status instead of replacing the exited one. `Docker.Create` ignored a container it was already tracking and pulled and created another one, which the daemon would reject because the name is still taken.
- **Fix:** the watch starts a terminated sidecar again when the policy is `Always` (an empty policy is `Always`) or `OnFailure` and the exit is non-zero. `Create` is that restart. Docker starts the stopped container in place; if start fails or the daemon no longer has it, the container is removed and created again, and a start that fails after create removes that new container. Memory replaces the same container's status. The VM is not restarted. `RestartCount` increments and `LastTerminationState` keeps the exit. The pod becomes Ready again when the sidecar is running, and the slot stays held because the VM is still the pod. A `Create` error fails the pod, stops the VM, and releases the slot, and the cap is still 2. A zero exit under `OnFailure` or `Never` is not restarted. `Never` with a non-zero exit still fails the pod. The 2-VM cap, the CLI flags, and the public method signatures are unchanged. `sidecar.Runtime` did not grow a method. The Docker client field is an unexported interface so the restart can be tested without a daemon.
- **Tests that failed first:** `TestHybridSidecarRestartsWhenPolicySaysSo/always-nonzero`, `/always-zero`, and `/on-failure`: `creates=1 phase=Running ready=false RestartCount=0`. `TestHybridSidecarExitedBeforeReadyIsRestarted`: exit 9, `creates=1`, pod stayed not Ready. `TestHybridSidecarRestartLeavesSiblingRunning`: the exited container was not started again. `TestHybridSidecarRestartFailureReleasesSlot`: `creates=1 phase=Running slots=1`, the restart error left the pod up. `TestMemoryCreateReplacesExitedContainer`: the second `Create` left both a terminated and a running status. `TestDockerCreateStartsStoppedContainer`: `pulls=1 creates=1 starts=1 started=[new-id]`. `TestDockerCreateLeavesRunningContainer`: the running container was recreated. `TestDockerCreateReplacesContainerThatWillNotStart` and `TestDockerCreateReplacesContainerMissingFromDaemon`: `removes=0`. `TestHybridSidecarOnFailureZeroDoesNotRestart` passed before the fix.
- **Verification (Linux):** `go vet ./...` PASS. `go test ./... -short -race -count=1 -timeout 600s` PASS. `make soak` PASS (4 cycles, 67ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`). `make test-kubelet-auth` was not run; those files were not edited.
- **Left alone:** PR #11's files, other than this log. A liveness restart does not re-run the startup probe, so `Started` stays true across that restart. Sidecar startup probes are not executed. `InitContainerStatuses` are still empty, and every `PodFailed` main container reports exit 1. `vz.Stop` still returns before closing the vsock listener when `vm.Stop` errors; that file is `darwin && arm64` and was not executed here. An exec that ends in a protocol error still returns that error; the exit code stays 0 when no exit frame arrived, and `ExecInVM` returns the error. Docker sidecar `Exec` still copies stdout and returns nil; it does not call `ContainerExecInspect`. The client is now an interface, and this diff does not use that to inspect execs. A zero exit under `OnFailure` or `Never` stays terminated: the pod stays `Running` and not Ready and holds the slot. A sidecar that keeps exiting is restarted once per probe interval. There is no `CrashLoopBackOff` and no restart limit, so that pod keeps its VM slot while the policy says to keep starting the container. A crash between `commitCacheDir`'s two renames, `NodeReady` on a failed host probe, and the virtual-kubelet lease while the API server is down are unchanged from the 2026-11-11 entry.
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
  - PR #11 is unmerged. This file will conflict at the top of the log when the second of #11 and this stack merges; keep both entries, this one above the 2026-11-11 entry.
- **Next priority:**
  1. Re-run the startup probe after a liveness restart so `Started` resets until it passes. Linux-provable.
  2. Run sidecar startup probes. Linux-provable.
  3. Docker sidecar `Exec` should surface the container exit code. The client seam is in place. Not a fail-open.
  4. Hardware gate remains the production gate and is still blocked on a Mac.

### 2026-11-11 (Asia/Singapore) — restart materialize, token copy, exec exit, Started

- **HEAD:** `main` is `543faa9b` (PR #12 merged 9 Nov). This branch stacks on PR #13 (`cursor/materialize-sidecar-fail-closed-28ab`, `40dbebb`). PR #11 (`cursor/ca-reload-second-signal-66c0`, client CA reload and a second signal calling `Engine.Close`) is open and unmerged. This branch does not edit signal handling, kubelet auth, the client CA, `TLSConfig`, or the docs PR #11 touches. Hardware gate and the 24h vz soak are still unrun. No Mac worker is online.
- **restartVM (confirmed):** copy-mode `Materialize` warned on an RPC error, ignored `OK: false`, and returned nil. A liveness restart then left the pod `Running`. The watch kept restarting until the restart limit, so the VM ran without those volumes. `rec.agent` is the new dial, so `fail` closes it the same way it does after the agent is stored.
- **Projected token (confirmed):** `rematerializeTokenVolumes` returned only the RPC error. `OK: false` was success, `tokensNeedCopy` was cleared, and the watch emitted `ServiceAccountTokenRefreshed`.
- **Exec (confirmed):** `ExecInteractive` left `exit` at 0 when the stream channel closed before `Exited`. A terminal response with no exit frame returned 0 and a nil error.
- **Started (confirmed):** the VM container used `rec.ready && rec.agentOK`, so a failing readiness probe cleared `Started`. A sidecar used the same bit as `Ready`, so a container that had run reported `Started` false after it exited.
- **Fix:** `restartVM` uses the same `materializeFailure` check as `runConnected`. The error returns to the liveness watch, which fails the pod, stops the VM, and releases the slot. The dial is the one `teardown` closes. A projected-token `OK: false` is the same error: the watch logs `TokenRefreshFailed`, leaves `tokensNeedCopy` set, and retries on the next tick. It does not emit `ServiceAccountTokenRefreshed`, and the pod stays `Running` until the token actually expires. An exec stream with no exit frame returns -1 and `exec ended without an exit status`. `ContainerStatus.Started` is true once the VM is `Running` and its startup probe has passed (or it has none), including while readiness is failing. A sidecar stays started after it exits if it has a start time. The 2-VM cap, the CLI flags, and the existing method signatures are unchanged.
- **Tests that failed first:** `TestRestartVMMaterializeNotOKFailsPod/message` and `/empty`: `starts=6, materialize OK:false left the restarted VM running: liveness probe failed (restarts=6): exit 1`. `TestRestartVMMaterializeRPCErrorFailsPod`: `starts=6, materialize RPC error left the restarted VM running: liveness probe failed (restarts=6): probe failed`. `TestProjectedTokenRematerializeNotOKRetries`: `OK:false was recorded as ServiceAccountTokenRefreshed`. `TestExecStreamWithoutExitFrameIsAnError`: `exit 0 err <nil>, stream without an exit frame reported success`. `TestReadinessFailureKeepsContainerStarted`: `readiness failure cleared Started`. `TestStartupProbeGatesStarted`: `startup passed but Started stayed false while not ready`. `TestTerminatedSidecarStaysStarted`: `sidecar that had started reports Started false after exit`.
- **Verification (Linux):** `go vet ./...` PASS. `go test ./... -short -race -count=1 -timeout 600s` PASS. `make soak` PASS (4 cycles, 67ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`). `make test-kubelet-auth` was not run; those files were not edited.
- **Left alone:** PR #11's files, other than this log. A hybrid sidecar is not restarted when the policy is `Always` or `OnFailure`; the pod stays `Running`, not Ready, and holds the slot. A liveness restart does not re-run the startup probe, so `Started` stays true across that restart instead of resetting until the probe passes again. Sidecar startup probes are not executed. `InitContainerStatuses` are still empty, and every `PodFailed` main container reports exit 1. `vz.Stop` still returns before closing the vsock listener when `vm.Stop` errors; that file is `darwin && arm64` and was not executed here. An exec that ends in a protocol error still returns that error; the exit code stays 0 when no exit frame arrived, and `ExecInVM` returns the error. Docker sidecar `Exec` still copies stdout and returns nil; it does not call `ContainerExecInspect`. The client is a concrete `*dockercl.Client` and there is no fake, so that path was not executed without a daemon. A crash between `commitCacheDir`'s two renames, `NodeReady` on a failed host probe, and the virtual-kubelet lease while the API server is down are unchanged from the 2026-11-09 entry.
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
  - PR #11 is unmerged. This file will conflict at the top of the log when the second of #11 and this stack merges; keep both entries, this one above the 2026-11-10 entry.
- **Next priority:**
  1. Restart a hybrid sidecar when the policy is `Always`, or `OnFailure` and the exit is non-zero. Until that exists the pod stays `Running` and not Ready and holds one of the two VM slots. Linux-provable.
  2. Re-run the startup probe after a liveness restart so `Started` resets until it passes. Linux-provable.
  3. Docker sidecar `Exec` should surface the container exit code. That needs a client seam or a daemon. Not a fail-open.
  4. Hardware gate remains the production gate and is still blocked on a Mac.

### 2026-11-10 (Asia/Singapore) — materialize fail-closed, hybrid sidecar readiness

- **HEAD:** `main` is `543faa9b` (PR #12 merged 9 Nov; CI green on macOS test, Linux test, kubelet auth envtest; e2e self-hosted skipped). PR #11 (`cursor/ca-reload-second-signal-66c0`, client CA reload and a second signal calling `Engine.Close`) is open and unmerged. This branch does not edit signal handling, kubelet auth, the client CA, or `TLSConfig`. Hardware gate and the 24h vz soak are still unrun. No Mac worker is online.
- **Materialize (confirmed):** `runConnected` discarded `MaterializeRes` and only warned when `Client.Materialize` returned an error. The guest reports a refused placement as `OK: false` on a successful RPC (`defaultMaterialize` does this). Both shapes left the pod `Running` with no volumes. `rec.agent` is still nil at that point, so `teardown` does not close the dial the way it does after the agent is stored.
- **Hybrid sidecar (confirmed):** `PodReady` was `phase == Running && rec.ready && rec.agentOK`. A sidecar container's own status already went `terminated`, and `PodReady` stayed true. `watch` refreshed that status and did not look at it. Restart policy was only consulted for the VM liveness probe.
- **Not a bug:** a non-zero sidecar exit under `RestartPolicyAlways` or `RestartPolicyOnFailure` does not fail the pod. Those policies say restart, and a zero exit is success under every policy, including `Never`. The pod stays `Running` and not Ready, and it keeps the slot. Restarting the container is not implemented. The three optional items below are real; they are not in this diff.
- **Fix:** `Materialize` `OK: false` and an RPC error fail the pod before it is published `Running`. The dial is closed first, then `fail` stops the VM and releases the slot. `PodReady` and `ContainersReady` are false unless every hybrid sidecar is `running` (reason `SidecarNotRunning`); the VM container's `Ready` stays the probe bit. A sidecar that has exited non-zero under `RestartPolicyNever` fails the pod, including when it is already dead before `Running` is set and when it exits later on the watch. The 2-VM cap, the CLI flags, and the existing method signatures are unchanged.
- **Tests that failed first:** `TestMaterializeNotOKFailsPodAndStopsVM/message` and `/empty`: `phase Running msg="", materialize OK:false left the pod running`. `TestMaterializeRPCErrorFailsPodAndStopsVM`: `phase Running msg="", materialize RPC error left the pod running`. `TestHybridSidecarExitClearsPodReady` (`always`, `on-failure`, `never-zero`): `exited sidecar left PodReady true after the VM readiness probe passed`. `TestHybridSidecarExitAfterRunningClearsPodReady`: `PodReady stayed true after the sidecar exited`. `TestHybridSidecarNonZeroExitOnNeverFailsPod`: `RestartPolicyNever left the pod Running after sidecar exit 1: ready=true`. `TestHybridSidecarNonZeroExitAfterRunningFailsPod`: `phase Running msg= want Failed`.
- **Verification (Linux):** `go vet ./...` PASS. `go test ./... -short -race -count=1 -timeout 600s` PASS. `make soak` PASS (4 cycles, 67ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`). `make test-kubelet-auth` was not run; those files were not edited.
- **Left alone:** PR #11's files. `restartVM` still warns on a copy-mode `Materialize` RPC error, ignores `OK: false`, and returns nil, so a liveness restart can leave the pod `Running` without those volumes. Projected-token rematerialize returns the RPC error (the watch already warns until expiry) and treats `OK: false` as success. A hybrid sidecar is not restarted when the policy is `Always` or `OnFailure`; the pod stays `Running`, not Ready, and holds the slot. `ContainerStatus.Started` is still `rec.ready && rec.agentOK` on the VM, and on a sidecar it is still the same bit as `Ready`. `InitContainerStatuses` are still empty, and every `PodFailed` main container reports exit 1. `vz.Stop` still returns before closing the vsock listener when `vm.Stop` errors; that file is `darwin && arm64` and was not executed here. An exec stream that ends without an exit frame still returns 0 with a nil error (`ExecInteractive` leaves `exit` at zero when the channel closes before `Exited`). Docker sidecar `Exec` still copies stdout and returns nil; it does not call `ContainerExecInspect`. No Docker daemon was available, so that path was not executed. A crash between `commitCacheDir`'s two renames, `NodeReady` on a failed host probe, and the virtual-kubelet lease while the API server is down are unchanged from the 2026-11-09 entry.
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
  - PR #11 is unmerged. This file will conflict at the top of the log; keep both entries, this one above the 2026-11-09 entry.
- **Next priority:**
  1. Restart a hybrid sidecar when the policy is `Always`, or `OnFailure` and the exit is non-zero. Until that exists the pod stays `Running` and not Ready and holds one of the two VM slots. Linux-provable.
  2. The same fail-closed `Materialize` check on `restartVM`'s copy-mode rematerialize, and treat `OK: false` as an error in projected-token rematerialize. Linux-provable. Not a fail-open.
  3. `ContainerStatus.Started` should stay true once the container has started. An exec stream that ends without an exit frame should not report 0. Docker sidecar `Exec` should surface the container exit code (needs a daemon or a client seam).
  4. Hardware gate remains the production gate and is still blocked on a Mac.

### 2026-11-09 (Asia/Singapore) — partial pull cache, failed-pod phase, warm Start leak

- **HEAD:** `main` is `1a0a41fb` (PRs #8, #9, and #10 merged; CI green). PR #11 (`cursor/ca-reload-second-signal-66c0`, client CA reload and a second signal calling `Engine.Close`) is open and unmerged. This branch does not edit signal handling, kubelet auth, the client CA, or `TLSConfig`. Hardware gate and the 24h vz soak are still unrun. No Mac worker is online.
- **Pull cache (confirmed):** `Pull` and `resolveImage` treated `config.json` + `disk.img` + `aux.img` with no `provenance.json` as a good image. `VerifyOptional` skips that shape on purpose for a local bake. An unfinished pull — the process died before `WriteProvenance`, or `copyFile` returned a short `disk.img` — has the same shape, and the next pod cloned it. `pullOnce` also created the live cache directory before the registry responded, so a failed pull published that directory. A stage directory named by suffixing the cache path is itself a legal cache key (`v1` stages onto the directory of tag `v1.partial`) and `RemoveAll` deleted the other image.
- **Failed phase (confirmed):** `Delete` always set `PodSucceeded`. `buildStatusLocked` then reported exit 0. A pod that had already failed stayed visible as `Succeeded` for the whole teardown, including the cache snapshot. The `Succeeded` write exists so the death watch cannot flip a graceful delete of a running pod into `Failed`. That part stays.
- **Warm Start (confirmed):** `bootWarmMachine` returned a nil machine when `Start` failed, and `bootWarm` released the slot. `Start` can return after it has already published the machine: the vz runtime listens on vsock before `vm.Start`, and a `Start` that returns after launch leaves the VM running. The test runtime did the latter. Two further pods were admitted. `Running()` was 3 with 2 slots held, and the guest `ReadFrame` goroutine was still up.
- **Fix:** a pull-cache hit requires `provenance.json` (`LoadCommitted`). The bytes are written under `cache/image-partial/<key>` and renamed over `images/<key>` only after the lockfile is written. A failure deletes the partial and leaves a committed cache in place. The previous tree sits in `cache/image-replacing/<key>` only during that rename, not on a path a tag can name. Local directory bakes still load without provenance. `Delete` leaves an already-`Failed` pod `Failed` (exit 1). A warm `Start` error force-stops the machine before the slot is released. The 2-VM cap, the CLI flags, and the existing method signatures are unchanged.
- **Tests that failed first:** `TestPullDoesNotServeUncommittedCache`: `uncommitted cache served as a hit`. `TestFailedPullDoesNotPublishCache`: `failed pull published cache ...: <nil>`. `TestResolveImageRejectsUncommittedPullCache`: `uncommitted pull cache was used, hardware "abc" disk "SHORT"`. `TestDeleteKeepsFailedPhase`: `phase Succeeded exit 0, failed pod was rewritten during delete`. `TestWarmStartErrorDoesNotLeaveVMPastCap`: `running VMs = 3 with 2 slots held; warm start failure leaked a VM past the cap`. `TestFailedPullDoesNotClobberSiblingTag`, against a `.partial` suffix: `sibling tag cache = "" err=open .../v1.partial/disk.img: no such file or directory`.
- **Test flakes (test-only):** `TestWaitSidecarExitDockerDown` no longer bounds wall time from `Create`. That bound was 11.91s on macOS run [42002323637](https://github.com/mlvea/darwin-node/actions/runs/42002323637) while the pod had already failed with `docker down`. The test now requires the message and that the two consecutive errors are at most a second apart. `TestProviderRefreshesProjectedToken` uses a 20s token lifetime (the engine test already did), waits until the pod is `Running`, then allows 15s for the half-life refresh. The 120ms expiry and a 3s poll from `Create` is what reported `token "first" after 2 mints` on that run.
- **Verification (Linux):** `go vet ./...` PASS. `go test ./... -short -race -count=1 -timeout 600s` PASS. `make soak` PASS (4 cycles, 88ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`). `make test-kubelet-auth` PASS (kube-apiserver + etcd 1.35, 4.4s).
- **Left alone:** PR #11's files. `runConnected` still ignores `Materialize` `OK: false` and only warns on an RPC error, so a pod can reach `Running` without its volumes. A terminated hybrid sidecar does not clear `PodReady`. `ContainerStatus.Started` is still the same bit as `Ready`. `InitContainerStatuses` are still empty, and every `PodFailed` main container reports exit 1. `vz.Stop` still returns before closing the vsock listener when `vm.Stop` errors; that file is `darwin && arm64` and was not executed here. A crash between `commitCacheDir`'s two renames leaves the previous tree under `cache/image-replacing/<key>`, which is not republished; the next pull fetches again and does not boot a partial disk. `NodeReady` stays true when the host probe fails. An unreachable API server is the virtual-kubelet lease: this process cannot patch the Node object while the apiserver is down. An exec stream that ends without an exit frame can still report 0. Docker sidecar `Exec` still ignores the container exit code.
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
  - PR #11 is unmerged. This file will conflict at the top of the log; keep both entries, this one above the 2026-11-06 entry.
- **Next priority:**
  1. Fail the pod when guest `Materialize` returns `OK: false` or the RPC fails. The pod currently becomes `Running`. Linux-provable. Not a fail-open.
  2. A hybrid sidecar that has exited should clear `PodReady`, and should fail the pod when the restart policy says so.
  3. Hardware gate remains the production gate and is still blocked on a Mac.

### 2026-11-06 (Asia/Singapore) — client CA reload; second signal still stops pods

- **This PR:** https://github.com/mlvea/darwin-node/pull/11 (`cursor/ca-reload-second-signal-66c0`).
- **HEAD:** `main` is `1a0a41fb` (merge of PR #9). Its parent `aa5503f6` is the merge of PR #10. PR #10 landed first: the VM slot stays held through `Create`, and `Engine.Close` force-stops remaining pods. PR #9 then landed: startup reclaim of orphan pod directories, a dead VM fails the pod, and the Ready-failure path closes the agent client. No open PRs or issues at the start of this run. CI on `1a0a41fb` is green: [run 42002503335](https://github.com/mlvea/darwin-node/actions/runs/42002503335) (macOS `test`, Linux `test`, kubelet auth envtest). `e2e (self-hosted)` skipped. No Mac worker is online.
- **`aa5503f6` macOS `test`:** [run 42002323637](https://github.com/mlvea/darwin-node/actions/runs/42002323637) failed job `test` ([127160335605](https://github.com/mlvea/darwin-node/actions/runs/42002323637/job/127160335605)) on `macos-latest`. Linux `test` (127160335642) and kubelet auth envtest (127160335251) passed. `e2e` skipped. The failures were `TestWaitSidecarExitDockerDown` (`pkg/engine/engine_test.go:408`, `error did not surface quickly: 11.91s msg=docker down`) and `TestProviderRefreshesProjectedToken` (`pkg/provider/token_test.go:187`, `token "first" after 2 mints`). Those two files and `pkg/provider` are identical between `aa5503f6` and `1a0a41fb`. `probes.go` did change in #9; neither failure is in that file. The same tests passed on the tip run. This is a one-off loaded-runner flake. It is not a product regression from merging #10 before #9. Left unfixed unless it comes back.
- **Client CA (confirmed still open on tip):** `NewDynamicCAContentFromFile` loaded the bundle once. Nothing called `DynamicFileCAContent.Run`, so a replaced file was ignored until process restart. `TLSConfig` also pinned `ClientCAs` on the handshake config, so starting `Run` alone would not have accepted a rotated CA. Webhook auth and the handshake did not share one live bundle.
- **Second signal (confirmed still open on tip):** after the first SIGINT/SIGTERM, another of those signals called `os.Exit(130)` and skipped `Engine.Close`. Pod VMs, slots, and cache snapshots from that stop were abandoned. The first-signal drain itself was already in place.
- **Fix:** one `ClientCABundle` is loaded at startup and `Run` watches the file. Webhook auth verifies client certificates with that provider. `GetConfigForClient` copies `ClientCAs` from the same bundle on every handshake. An empty or unparsable replacement leaves the last good bundle in place. A missing or invalid file at startup still fails closed and does not install `NoAuth`. A second signal cancels the in-flight drain and does not return until `Close` has force-stopped remaining pods (grace 0), then exits 130. The first signal still drains for `--shutdown-grace-period` and honors `terminationGracePeriodSeconds`. `--shutdown-grace-period=0` still skips the drain and still Close's. `Close` does not run at the same time as `Drain`. The 2-VM cap, the CLI flags, and the `TLSConfig` / `Engine.Close` / `Engine.Drain` signatures are unchanged.
- **Tests that failed first:** `TestClientCAReloadServesRotatedBundle` against a bundle that pinned the first pool: `rotated CA was not accepted: client=<nil> server=tls: failed to verify certificate: x509: certificate signed by unknown authority`. `TestGracefulDrainUsesPodTerminationGrace` against a second-signal path that only Close'd: `RemovePod graces [0], want [9]`. `TestSecondSignalClosesWithoutDrain`: `second signal exited with 2 VM(s) still running` and `2 slot(s) held`.
- **Verification (Linux):** `go vet ./...` PASS. `go vet -tags=integration ./cmd/darwin-node/` PASS. `go test ./... -short -race -count=1 -timeout 600s` PASS. `make soak` PASS (4 cycles, 69ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`). `make test-kubelet-auth` PASS (live kube-apiserver + etcd 1.35, 5.1s).
- **Left alone:** Virtualization.framework and `pkg/runtime/vz` `Stop`. `docs/blog/two-machines-per-universe.md` still describes graceful drain as pending; that essay is historical. The macOS timing flake above. A process crash still skips `Close`; the next start reclaims leftover pod directories. Soak and guest-agent signal handlers. A second signal that arrives after the graceful drain has already returned can still race exit 0 against exit 130; the pods are already stopped. A finished `.tmp` cache clone is still dropped if the process dies before the commit rename. A dead VM is still noticed on the probe interval (default 10s).
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
- **Next priority:**
  1. If macOS `test` flakes again, look at `TestWaitSidecarExitDockerDown` (the 2s ceiling includes `Create`) and `TestProviderRefreshesProjectedToken` (120ms token expiry). Test-only. Leave them until they fail again.
  2. Hardware gate remains the production gate and is still blocked on a Mac.

### 2026-11-05 (Asia/Singapore) — shutdown stops pod VMs; delete keeps the slot through create

Filed under the 2026-11-03 section until PR #9 merged, so it would not collide with that PR's 2026-11-04 entry.

- **HEAD:** `main` was `e5ab058` (merge of PR #8). PR #9 (`cursor/startup-orphans-vm-death-c968`) was still open. Linux CI on `e5ab058` was green. Hardware gate and the 24h vz soak are still unrun.
- **Slot during create (confirmed):** `Delete` ran `teardown` before `startDone.Wait`. `teardown` released the slot from the `rec.machine` pointer it had copied, and that pointer was still nil while `start` was inside `Runtime.Create`. The slot was free, and `start` still booted the VM. Two further pods were admitted against a cap of 2. `startDone` was also incremented only after the pod was published, so a delete in that gap returned from `Wait` immediately.
- **Close (confirmed):** `Engine.Close` stopped the warm pool only. A drain that ran out of budget, or `--shutdown-grace-period=0`, left pod VMs running and their slots held. Those pods' cache volumes were not snapshotted.
- **Fix:** `Delete` cancels the pod, waits until `start` returns, then stops the machine and releases the slot. The wait is counted before the pod is visible, and the hostPort rejection path marks it done. `Close` rejects new creates, stops the warm pool, and force-stops every remaining pod through `Delete` (grace 0), which snapshots caches and releases slots. A second `Close` is safe. The 2-VM cap and the public signatures are unchanged.
- **Drain test:** `TestDrainHonorsContextBudget` no longer pairs a 25ms budget with a 30ms sleep and a 200ms ceiling. The sidecar blocks in `RemovePod` until the drain context is canceled. The in-flight delete finishes; the next pod is not started. With the context check in `Drain` removed, the test fails with `drain started pb after its context ended`.
- **Verification (Linux):** the new tests failed first. `TestDeleteDuringInFlightCreateDoesNotAdmitAThirdVM` admitted two pods while `Runtime.Create` was blocked (`slots at Delete wait: 0`). `TestCloseStopsRunningPodVMs` left 2 VMs running. `TestCloseSnapshotsCacheOfPodStillRunning` found no cache snapshot. After the fix: `go vet ./...` PASS, `go test ./... -short -race -count=1 -timeout 600s` PASS, `make soak` PASS (4 cycles, 69ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`).
- **Left alone:** Hardware gate and the 24h vz soak. WebhookAuth still loads the client CA once. Nothing calls `DynamicFileCAContent.Run`, and `TLSConfig` also pins `ClientCAs` for the handshake, so starting `Run` by itself does not accept a rotated CA. Not a fail-open. A dead VM is still noticed on the probe interval (default 10s); that watch is in `probes.go`, which PR #9 is already editing. A second SIGINT/SIGTERM still calls `os.Exit(130)` and skips `Close`.
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
  - PR #9 is unmerged. This branch is based on `e5ab058`. It does not edit `probes.go`, `recover.go`, `recover_test.go`, `pkg/soak/soak_test.go`, `docs/security.md`, or `docs/blog/two-machines-per-universe.md`. The `engine.go` edits are `Close`, `Create`'s `startDone` accounting, and `Delete`'s wait-before-release.
- **Next priority:**
  1. WebhookAuth CA reload, both layers. Start `DynamicFileCAContent.Run`, and refresh the TLS `ClientCAs` pool from that same bundle (a `GetConfigForClient` hook). `loadCABundle` already keeps the previous bundle when the file is empty or invalid. Linux-provable. Not a fail-open.
  2. Hardware gate remains the production gate and is still blocked on a Mac.

### 2026-11-04 (Asia/Singapore) — startup orphans, dead-VM status, Ready-path fd
- **HEAD:** `main` was `e5ab0586` (merge of PR #8). Linux CI on that commit was green. Hardware gate and the 24h vz soak are still unrun.
- **Young orphans (confirmed):** `Recover` / `New` kept unheld `cache/pods/<uid>` directories for 24h and never touched `cache/warm`. A crash, a kill during create or delete, or a drain that exhausts its budget and then exits leaves the VMs dead (`Engine.Close` still only stops the warm pool, on purpose) and does not snapshot cache volumes. Secrets and multi-GB overlays sat until the directory mtime was older than 24h. Slots were not rebuilt from those directories; that part was already right.
- **Dead VM (confirmed):** `watch` only noticed a dead guest through probes. With no liveness probe, `Stop` or a `VMFailed` status left the pod `Running` and held one of the two slots. `buildStatusLocked` read the runtime state and discarded it.
- **Ready failure (confirmed):** `runConnected` closes the agent from `teardown` only after `rec.agent` is set, which happens after `Ready` returns. A Ready error (or `Ready: false`) dropped the client. On the vz path that is the accepted vsock; nothing else closes it.
- **Fix:** Startup deletes every unheld pod directory regardless of age, and deletes `cache/warm` before the pool starts. A uid that already holds a slot is left alone, and nothing is pinned. The committed cache store is not rewritten from the orphan. The watch fails a `Running` pod whose machine is `Stopped` or `Failed`, releases the slot, and removes the directory. `Delete` sets `Succeeded` before it stops the machine, and `failWith` does not overwrite that. The Ready-failure path closes the client before `fail`. The 2-VM cap is unchanged.
- **Verification (Linux):** the new tests failed first. `TestEngineNewSweepsStalePodDirs` still had the young directory. `TestNewDropsYoungOrphansWithoutPinningSlots` still had the secret file. Both VM-death tests stayed `Running` until the 5s wait. `TestReadyFailureClosesAgentConn` reported the connection still open and two `ReadFrame` goroutines. After the fix: `go vet ./...` PASS, `go test ./... -short -race -count=1 -timeout 600s` PASS, `make soak` PASS (4 cycles, 67ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`). The soak test that planted a pod dir and expected end-of-run drift now expects startup to reclaim that dir and the warm token; a `.tmp` file outside recovery still fails the run.
- **Left alone:** `TestDrainHonorsContextBudget` is still timing-sensitive (25ms budget, 30ms sidecar sleep, 200ms ceiling). WebhookAuth still loads the client CA once; nothing calls `DynamicFileCAContent.Run`, so a replaced CA waits for process restart. Truncation does not unload the CA already in memory. Not a fail-open. A dead VM is noticed on the probe interval (default 10s), not on the next status poll. `Engine.Close` still does not stop pod machines. A finished `.tmp` cache clone is still dropped if the process dies before the commit rename.
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
- **Next priority:**
  1. WebhookAuth CA reload. `NewDynamicCAContentFromFile` loads the bundle once. `loadCABundle` refuses an empty or invalid file without replacing a good bundle, but `Run` is never started, so a CA file replaced while the node is up is not picked up until restart. Linux-provable. Not a fail-open.
  2. `TestDrainHonorsContextBudget` is timing-sensitive. Test-only.
  3. Hardware gate remains the production gate and is still blocked on a Mac.

### 2026-11-03 (Asia/Singapore) — cache-store crash windows and serial-console fd leak
- **HEAD:** `main` was `4c7c8b77` (merge of PR #7). Linux CI on that commit was green. Hardware gate and the 24h vz soak are still unrun.
- **Cache crash window (confirmed):** `snapshotPodCaches` renames the live store to `name.old`, then renames `name.tmp` onto the store. `Recover` only swept old pod directories. `prepareCaches` stats the live path and, if that path is missing, creates an empty directory. A crash between the two renames left the last committed tree only under `.old`, so the next pod restored nothing. A later graceful delete could then remove `.old` and commit that empty tree. `.tmp` beside a live store did not change restore contents, and it was not reclaimed until the next snapshot of that same cache. A `.tmp` with no live store and no `.old` is uncommitted: a crash during the clone and a crash after the clone leave the same shape, so that directory is not published.
- **Serial console fd leak (confirmed):** `serveConsole` takes the console pipe (`os.Pipe` on the vz runtime) and returned without closing it when `Listen` or `Chmod` failed.
- **Fix:** `Recover` (from `New`), the next restore, and the next snapshot republish `name.old` when the live store is missing, and delete `.tmp` and leftover `.old`. Snapshot heals a torn store before it mutates, and it deletes `.old` only after the new tree is renamed into place. Cache names ending in `.tmp` or `.old` are rejected at `ValidatePod`, before a slot is taken, so those directories cannot be a real store. The serial pipe stays open only after the console hub owns it. The 2-VM cap and fail-closed admission are unchanged.
- **Verification (Linux):** the new tests failed first. Between the renames, restore wanted `committed-snapshot` and the file was missing. Startup left `.tmp` and `.old` in place. `TestSnapshotAfterCrashRecoveryCommitsAndCleans` lost the committed bytes. Both console tests reported the pipe fd still open (`F_GETFD`). `TestCreateRejectsCacheStagingSuffix` saw `Create` accept a `.tmp` name. After the fix: `go vet ./...` PASS, `go test ./... -short -race -count=1 -timeout 600s` PASS, `make soak` PASS (4 cycles, 68ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`, `store_files=4`).
- **CI follow-up (same day):** pull_request run [41558174176](https://github.com/mlvea/darwin-node/actions/runs/41558174176) failed macOS `test` on `6f62a43` (`TestProjectedTokenRefreshRewritesAndRematerializes`: file already held `second`). The push run of that same SHA passed. `start()` schedules refresh at half the remaining lifetime before the pod is Running; a 120ms expiry (60ms half-life) was already due before the assertion on `macos-latest`. The goroutine leak in that log is `t.Fatal` leaving the pod's watch running. The test now uses a 20s lifetime (half-life past `waitPhase`'s 5s) and deletes the pod on cleanup. `TestNextTokenRefreshAtHalfRemainingLifetime` keeps the half-life check off the clock. Production refresh is unchanged. `TestDrainHonorsContextBudget` is still the separate timing risk below.
- **Open risks:**
  - Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework.
  - `e2e (self-hosted)` stays skipped.
  - A `.tmp` tree that finished cloning is dropped if the process dies before the commit rename. The previous committed tree is what comes back. That matches the cache-volume rule that a crash loses at most the last successful run.
- **Next priority:**
  1. Young orphan pod dirs. `RecoverCache` keeps unheld `cache/pods/<uid>` directories for 24h. A drain that exhausts its budget leaves those pods running; `Engine.Close` only stops the warm pool, which matches the comment that pod machines keep running. Process exit does not snapshot them, and secrets plus multi-GB overlays sit until the directory mtime is older than 24h. Linux-provable. Do not pin slots for them.
  2. `TestDrainHonorsContextBudget` is timing-sensitive (25ms budget, 30ms sidecar sleep, 200ms ceiling). It passed here under `-race`. It can still flake on a loaded runner. Test-only.
  3. WebhookAuth CA reload. `NewDynamicCAContentFromFile` loads the bundle once, and `loadCABundle` refuses an empty or invalid file without replacing a good bundle. Nothing in this process calls `DynamicFileCAContent.Run`, so a CA file replaced while the node is up is not picked up until restart. Truncation after startup does not unload the CA already in memory. Not a fail-open.
  4. Hardware gate remains the production gate and is still blocked on a Mac.

### 2026-11-02 — warm-pool readiness dial held the only guest session
- **HEAD:** `main` was `199a29e7` (merge of PR #6). Linux CI on that commit was green. Hardware gate and the 24h vz soak are still unrun.
- **Gap:** `bootWarmMachine` dialed the guest agent, checked Ready, and dropped the client without `Close`. `darwin-guest-agent` dials the host, `Serve`s that one connection, and dials again only after `Serve` returns. TCP fallback is off unless `--agent-tcp-fallback` is set. A warm VM therefore sat in the probe session for its whole idle life, and adoption's `DialAgent` could not get a connection. The fake runtime hid it: `DialAgent` handed back the same client `Start` had opened.
- **Fix:** the readiness client stays on the warm entry until the pod is about to dial (after init containers) or the VM is stopped, then it is closed so the agent can connect again. Closing at boot time would leave the guest waiting on a handshake nobody is accepting. The fake runtime now opens a new session per `DialAgent`, matching that reconnect. The 2-VM cap and fail-closed admission are unchanged.
- **Verification (Linux):** the new tests failed first (`previous vsock session still open`, and `read/write on closed pipe` after closing the only fake client). After the fix: `go vet ./...` PASS, `go test ./... -short -race -count=1 -timeout 600s` PASS, `make soak` PASS (4 cycles, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`).
- **Not this PR:** cache-store crash recovery, young orphan pod dirs, and the other gaps listed on the pull request.

### 2026-10-27 ~09:03 SGT — macOS `test` flake on main after PR #5
- **HEAD:** `main` was `92a3c52` (merge of PR #5). CI run [40334540713](https://github.com/mlvea/darwin-node/actions/runs/40334540713): macOS `test` job `121612472861` failed (`go test ./... -short`, exit 1). `test (linux)` and `kubelet auth envtest` passed. `e2e (self-hosted)` skipped. The merge tree `1665fc76` is the PR tip `515517e4`, whose macOS `test` job passed twice on 23 Oct.
- **Root cause:** `TestRuntimeCreateFailureFailsClosed` in `pkg/engine` (`inject_test.go:145`: `failed pod left state behind: <nil>`). `failWith` publishes `PodFailed` and `teardown` releases the slot before `os.RemoveAll` of `cache/pods/<uid>`. The helper treated that phase as proof the directory was already gone. On the loaded `macos-latest` runner `os.Stat` still succeeded, so removal had not finished. Same window fails `TestVMStartFailureFailsClosed` and `TestAgentDialFailureFailsClosed` (they share `assertFailedAndClean`). Not a WarmSlots regression and not a darwin-only product bug: Linux was green on this tree, and a 50ms sleep before `RemoveAll` reproduces the identical `Stat` error on Linux. The 2-VM cap and fail-closed path are unchanged; `PodFailed` stays visible while a large overlay is deleted.
- **Fix:** after the pod is `Failed`, `assertFailedAndClean` waits on `startDone`. That goroutine returns only after `RemoveAll`. The checks are the same: failure message set, slots in use is 0, the pod directory is gone, and the next pod reaches `Running`.
- **Verification (Linux):** with the 50ms sleep still in `failWith`, the three tests failed before the wait and passed after it. Sleep removed. `go test ./pkg/engine/ -count=20 -short -timeout 600s` PASS (238s). `go test ./pkg/engine/ -race -count=1 -short -timeout 180s` PASS.
- **Remaining risks:** hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun. No Mac worker is connected, so nothing here touched Virtualization.framework. `e2e (self-hosted)` stays skipped.
- **Next priority:** connect the Mac Mini as a worker and run `docs/HARDWARE_VALIDATION.md`.

### 2026-10-23 — weekday reflection (Asia/Singapore)
- **HEAD:** `main` is `d0ed5e5f` (PR #3 soak harness and PR #4 emptyDir limits / projected tokens / `docs/HARDWARE_VALIDATION.md`, both merged 22 Oct 2026). This branch fixes the warm-pool race that PR #4's verification noted.
- **What changed:**
  1. **Race, test-only.** `go test -race ./pkg/engine/` reported a data race in `TestWarmOverlayReapedOnAdoptAndEvict`: the test wrote `e.cfg.WarmSlots` while `fillWarm` read it. `TestHostPortConflictDoesNotLeakAdoptedVM` did the same write. Production copies `WarmSlots` into the engine at `New` (flag / `DARWIN_NODE_WARM_SLOTS`) and never writes it again, so there is no runtime budget to synchronize. Both tests now leave `WarmSlots` at 0, which does not start the replenisher, and call `bootWarm` directly. Adopt still keeps the overlay while the VM runs and delete still removes it; a non-adoptable pod still evicts the warm VM and that overlay is still removed; a hostPort clash still must not leave a running VM without a slot. The 2-VM cap is unchanged.
  2. **CI.** The Linux `test` job runs `go test ./... -short -count=1 -race`. The short suite was race-clean on this box (`go test -race -count=1 -short ./...`, exit 0). Darwin CI stays without `-race`.
- **Compile / tests (Linux box):**
  - `go test -race -count=1 -short -timeout 600s ./...` PASS (no other races)
  - `go test -race -count=1 -timeout 180s ./pkg/engine/ -run 'TestWarmOverlayReapedOnAdoptAndEvict|TestHostPortConflictDoesNotLeakAdoptedVM'` PASS
  - `go test ./... -short -count=1 -timeout 180s` PASS
  - `go vet ./...` PASS
  - `make soak` PASS: 4 cycles, 161ms, `runtime=fake`, `slots=0/2`, `fds=6/6`, `cap_rejects=4`

### Open risks
- Hardware gate `make test-hardware IMAGE=...` and the 24h `make soak IMAGE=...` are still unrun because no Mac worker is connected
- `e2e (self-hosted)` is always skipped, so nothing in CI touches Virtualization.framework
- Digest fast-path is a fingerprint plus a process-local MAC
- `emptyDir` `sizeLimit` is a polling directory walk, not an APFS quota
- `make licenses` still cannot regenerate `THIRD_PARTY_NOTICES` off Darwin

### Next priority
1. Connect the Mac Mini as a worker and run `docs/HARDWARE_VALIDATION.md`

### 2026-10-22 — volume contract on the fake runtime
- **HEAD:** `main` is the squash of PR #3 (24h soak harness). Hardware gate and the 24h `make soak IMAGE=...` run are still blocked on a Mac.
- **What landed:** host-side `emptyDir.sizeLimit` (directory walk on the watch interval; symlink escape fails the pod and frees the slot) and projected ServiceAccount tokens (one TokenRequest per projection, empty and failed mints refused, refresh at half the remaining lifetime). `docs/HARDWARE_VALIDATION.md` is the one-shot Mac checklist. Linux CI runs `go test ./... -short`.
- **Still blocked:** `make test-hardware IMAGE=...`, `make soak IMAGE=...` for 24h, APFS quotas, digest fast-path is still a fingerprint plus a process-local MAC.

### 2026-10-16 ~08:50 SGT — weekday reflection
- **HEAD:** `main` still `c2a5b62` (PR #2 TokenReview S003 already merged); PR #3 (`grokbuild/soak-harness`) tip `9f5a4d13` before this docs-only commit
- **What changed since 2026-10-15:**
  1. Overnight nothing moved. Still waiting on Madushan's merge call. No open issues besides PR #3. This branch's log already has the 2026-10-14 and 2026-10-15 entries; `main`'s copy is still stuck at 2026-10-06.
  2. PR #3 https://github.com/mlvea/darwin-node/pull/3 stays open on `grokbuild/soak-harness`: +1540/-7, 11 files, 3 commits. `mergeable=true`, `mergeable_state=clean`, 0 review comments. CI on `9f5a4d13`: `test` success, `kubelet auth envtest` success, `e2e (self-hosted)` skipped (both workflow runs).
  3. No new code this run. PR #3 remains review-ready. Staying quiet on feature work to avoid churn before the merge decision.
- **Not run / blocked:** no private Cursor workers online, so the hardware gate and the real 24h vz soak stay blocked. This box still has no local clone, no grok CLI, and no Grok OAuth session.

### Open risks
- Hardware gate and 24h vz soak are both unproven on Apple Silicon; the fake-runtime soak (`make soak`, 4 cycles) does not substitute for them
- `e2e (self-hosted)` is always skipped, so nothing in CI touches Virtualization.framework
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local
- `make licenses` still cannot regenerate `THIRD_PARTY_NOTICES` off Darwin

### Next priority
1. Madushan's merge call on PR #3
2. Bring the Mac Mini online as a private worker, then `make test-hardware IMAGE=...`, then the 24h `make soak IMAGE=...`

### 2026-10-15 ~08:49 SGT — weekday reflection
- **HEAD:** `main` still `c2a5b62`; PR #3 (`grokbuild/soak-harness`) tip `d3b8583` before this docs-only commit
- **What changed since 2026-10-14:**
  1. The soak-harness agent (`bc-9293abb7`) finished cleanly, unlike the three earlier attempts, and opened https://github.com/mlvea/darwin-node/pull/3 (+1522/-7, 11 files: `pkg/soak`, `cmd/darwin-soak`, `make soak`, docs).
  2. CI on `d3b8583`: `test` and `kubelet auth envtest` pass on both workflow runs; `e2e (self-hosted)` skipped (no Mac runner online).
  3. No new code this run. The PR is reviewable as-is, and piling more on it before the merge decision would only add churn.
- **Not run / blocked:** no private workers (Mac Mini) connected, so `make test-hardware` and the real 24h `make soak IMAGE=...` stay unrun. This box still has no clone, no grok CLI, and no Grok OAuth session.

### Open risks
- Hardware gate and 24h vz soak are both unproven on Apple Silicon; the fake-runtime soak (`make soak`, 4 cycles) does not substitute for them
- `e2e (self-hosted)` is skipped on every PR, so nothing in CI touches Virtualization.framework
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local
- `make licenses` still cannot regenerate `THIRD_PARTY_NOTICES` off Darwin

### Next priority
1. Madushan's call on merging PR #3 (CI green; the agent advises not to treat the fake pass as the production soak)
2. Bring the Mac Mini online as a private worker (`agent worker start`), then `make test-hardware IMAGE=<baked image>` and the 24h `make soak IMAGE=<baked image>`

### 2026-10-14 ~08:51 SGT — weekday reflection
- **HEAD (start):** `c2a5b62` on `main` (PR #2 TokenReview S003 already merged; origin/main unchanged)
- **HEAD (end):** this commit on `grokbuild/soak-harness`
- **What changed:**
  1. **Soak harness:** `pkg/soak` repeatedly creates and deletes cache-volume pods through every configured VM slot (1 or 2, never more). Each cycle execs `/usr/bin/true`, writes a host-side cache marker (the virtio-fs share a guest write lands on), deletes, and fails on fd, directory, slot, live-VM, cache-snapshot, or darwin-node goroutine drift. A create while both slots are held must return `VMCapacityExhausted`. Warm pool stays off: cache volumes cannot adopt a pre-booted VM, and a warm guest would consume one of the two slots.
  2. **Short path:** `make soak` (default 4 cycles) and `go test ./pkg/soak` use `pkg/runtime/fake`. No image, no signature, no Virtualization.framework. `cmd/darwin-soak` refuses `--image` on a non-darwin/arm64 host.
  3. **24h path:** `make soak IMAGE=<baked image>` signs `darwin-soak` when `codesign` exists and runs the vz runtime for 24h (override with `SOAK_CYCLES` or `SOAK_DURATION`). Not executed here.
  4. **Docs:** `docs/stability.md` and `docs/testing.md` describe the harness. The 24h result is still an open production gate.
- **Compile / tests (Linux box):**
  - `go vet ./pkg/soak/ ./cmd/darwin-soak/` PASS
  - `go test -count=1 -race -timeout 180s ./pkg/soak/` PASS
  - `go test -count=1 -timeout 60s ./pkg/soak/ ./cmd/darwin-soak/` PASS (`TestImageRejectedOffAppleSilicon` included)
  - `go test -count=5 -timeout 180s ./pkg/soak/` PASS
  - `make soak` PASS: 4 cycles, ~68ms, `runtime=fake`, `slots=0/2`, `fds=8/8`, `cap_rejects=4`, `store_files=4`
- **PR / branch:** https://github.com/mlvea/darwin-node/pull/3 (`grokbuild/soak-harness`, not merged)

### Open risks
- Hardware gate (`make test-hardware`) still unrun on real Apple Silicon
- Mac Mini / private workers are offline, so this box cannot boot Virtualization.framework
- The 24h `make soak IMAGE=...` result is still required before any production-ready claim. The short fake pass does not substitute for it
- Prior soak agents errored with empty transcripts (`bc-cc33fa88`, `bc-8564da74`, `bc-4fd2ded6`); this branch starts fresh and does not rewrite them
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local
- `make licenses` on this Linux box still refuses to rewrite `THIRD_PARTY_NOTICES` (generator requires the Darwin module graph for the codehex notice)

### Next priority
1. Hardware gate (`make test-hardware`) on the Mac Mini once a baked image and a signed binary are available
2. Then the real 24h soak: `make soak IMAGE=<baked image>`
3. Do not call the tree production-ready on the strength of the short fake soak

### 2026-10-06 ~08:51–09:10 SGT — TokenReview S003 API-server e2e
- **HEAD (start):** `0666780` on `grokbuild/tokenreview-s003`
- **HEAD (end):** this commit on `grokbuild/tokenreview-s003`
- **What changed:**
  1. **Live API server:** `cmd/darwin-node/kubelet_auth_envtest_test.go` (`//go:build integration`) starts controller-runtime envtest (kube-apiserver + etcd 1.35) and serves the real `kubeletAuth` handler over `pkg/config.TLSConfig`. `make test-kubelet-auth` downloads the binaries into `.cache/envtest`. `go test ./...` does not build the file. A tagged run with no binaries skips the live cases and still checks fail-closed CA. Linux CI job `kubelet auth envtest` runs the make target.
  2. **Bug — TLS blocked TokenReview:** `TLSConfig` used `tls.RequireAndVerifyClientCert`, so a bearer-only client never reached `WebhookAuth`. It is now `tls.VerifyClientCertIfGiven`: presented client certificates are still verified, and a connection with no certificate completes the handshake. Anonymous callers get 401 from the handler.
  3. **Bug — Helm identity could not call the auth webhooks:** the chart ClusterRole had no `create` on `tokenreviews.authentication.k8s.io` or `subjectaccessreviews.authorization.k8s.io`. Those rules are in the chart. The envtest kubelet client is a ServiceAccount with only those two verbs; TokenReview and SubjectAccessReview succeed with it.
  4. **Docs:** `docs/security.md` and the S003 row in `docs/stability.md` state what the envtest proves. Hardware gate (`make test-hardware`) and the 24h adopt/delete soak are still open.
- **What the envtest proved (all passed on this Linux box):**
  - (a) client cert signed by `ClientCA` is authenticated (`GET /logs/` → 204; RBAC is `nodes/log` for this node name)
  - (b) ServiceAccount bearer token is authenticated via TokenReview (`GET /exec` → 204; RBAC is `nodes/proxy`)
  - (c) SubjectAccessReview denies the cert user on `nodes/proxy`, denies a cert bound to a different node name, denies an unbound ServiceAccount, and denies the proxy ServiceAccount on `nodes/log` (403)
  - (d) anonymous request is 401; invalid bearer token is 401
  - (e) missing CA and invalid CA fail closed
  - a client certificate signed by a different CA is rejected in the TLS handshake
- **Compile / tests (Linux box):**
  - `go test -count=1 ./pkg/config/ ./cmd/darwin-node/` PASS (no integration tag)
  - `make test-kubelet-auth` PASS
  - `go vet` and `go vet -tags=integration` on those packages PASS
- **PR / branch:** https://github.com/mlvea/darwin-node/pull/2 (`grokbuild/tokenreview-s003`)

### Open risks
- Hardware gate (`make test-hardware`) still unrun on real Apple Silicon
- 24h adopt/delete soak with cache volumes still required before any production-ready claim
- A request that presents a valid client certificate is authenticated as that certificate; a bearer token on the same request is not reviewed
- Digest fingerprint is fail-closed but not a full rehash; MAC key is process-local
- `make licenses` on this Linux box still refuses to rewrite `THIRD_PARTY_NOTICES` (generator requires the Darwin module graph for the codehex notice). controller-runtime is not yet listed there

### Next priority
1. Hardware gate (`make test-hardware`) when a Mac with a baked image is available
2. After that, the 24h adopt/delete soak
3. Do not call the tree production-ready on the strength of this API-server test

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
- **PR / branch:** https://github.com/mlvea/darwin-node/pull/2 (`grokbuild/tokenreview-s003`)
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
