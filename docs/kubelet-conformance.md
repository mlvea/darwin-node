# Kubelet conformance on SimVZ

`pkg/kubeletconformance` is the pod-lifecycle suite for design decision Q12.
It runs on every `go test` (including `-short` and `-race`) through the
provider and the engine, with `pkg/runtime/simvz` as the VM runtime. It
does not boot Virtualization.framework. The cases lock behavior that is
already implemented, so a regression fails in CI instead of depending on
a Mac.

```
go test ./pkg/kubeletconformance
go test -race -count=1 ./pkg/kubeletconformance
```

The package is test-only. Nothing in `cmd/` imports it.

## Clock

Probe periods, liveness CrashLoop delays, and image-pull / boot retries
sleep on `engine.VirtualClock`. The suite starts that clock at the Unix
epoch and calls `PendingTimersForTest` before `Advance`, so a wait cannot
be skipped by moving time before the timer exists. There is no `time.Sleep`
for a backoff. A few tens of milliseconds of wall time are only there to
show that a timer which should still be armed did not fire.

Two delays are real wall time, because the engine does not put them on
the probe clock:

- Sidecar CrashLoop uses `flowcontrol.NewBackOff(10s, 5m)` and
  `time.Now`. The suite lets the first restart happen (it is immediate),
  then advances the probe clock by an hour. That wakes the watch while
  the 10s wall window is still open, and the sidecar must not be created
  again.
- Guest-agent redial sleeps on `time.NewTimer`. The guest-loss case
  shortens it with `SetAgentRedialPaceForTest` and restores the production
  values afterwards.

`restartVM` stops the machine with a 5s grace. That grace is shutdown
time for the guest, not the CrashLoop delay. The suite's runtime wrapper
forwards `Stop` with grace 0 so the 5s does not land in the test. Delete
uses `terminationGracePeriodSeconds: 0` for the same grace reason.

The wrapper does not serialize `Stop` against `Start`. SimVZ cleanup
waits only for the accept loops of the generation it stopped, so the
guest watch can call `Start` from inside that wait. `docs/simvz.md`
describes that rule.

`Succeeded` and `Failed` are stored before teardown stops the machine
and releases the slot. The suite waits for that release after it
observes the terminal phase.

## SimVZ and a real Mac

`HaltGuest` is a no-op off Darwin, so a workload that exits leaves the
SimVZ machine Running. The engine publishes Succeeded or Failed only once
the machine is Stopped or Failed. After `WorkloadExitCode` is set, the
harness stops the VM. That is the Linux stand-in for the guest halt, not
a second product behavior.

SimVZ does not mount virtio-fs. A cache case writes the host directory
`cache/pods/<uid>/caches/<name>`, which is the directory a Darwin guest
write reaches through the share, then lets the workload exit. The store
checked afterwards is `cache/cache-store/<namespace>/<cache-name>`.

## What is pinned

| Case | Guards | Rule |
| --- | --- | --- |
| `ready-transition-time` | PR #34 | `LastTransitionTime` changes only when the condition status flips |
| `liveness-restart-policy` | PR #17 | Liveness failure on Always, OnFailure, and an empty policy waits 10s, then restarts, keeps the slot, and does not fail the pod |
| `liveness-never-fails` | PR #17 | RestartPolicy Never fails the pod and releases the slot |
| `liveness-backoff-resets` | PR #17 | After 10 minutes without a restart the delay is 10s again. The restart count is not cleared |
| `liveness-clears-started` | PR #17 | A liveness restart clears `Started` until the startup probe passes |
| `node-restart-keeps-succeeded` | PR #34 | A Succeeded Never pod stays Succeeded across a process restart and is not booted |
| `node-restart-never-running` | PR #34 | A previously Running Never pod becomes Failed / `NodeRestarted` and is not booted. Its restart count is unchanged |
| `node-restart-always-reboots` | PR #34 | Always boots again and the restart count increases by one |
| `fresh-never-still-starts` | PR #34 | A Never pod that had no sandbox still starts |
| `guest-agent-loss-stays-running` | PR #32 | A dead guest agent leaves the pod Running, not Ready, reason `GuestAgentDisconnected`. The VM is not restarted |
| `cache-committed-before-succeeded` | PR #56 | When phase is Succeeded the cache store already holds the new bytes |
| `cache-nonzero-does-not-replace` | PR #49 | Exit non-zero leaves the previous store in place |
| `named-port-resolves` | PR #23 | An HTTP readiness probe's named port is the container port |
| `unknown-named-port` | PR #23 | An unknown name does not become Ready (`ReadinessProbe`) |
| `init-order` | init containers | Init containers run in spec order, then the VM starts |
| `init-failure-skips-vm` | init containers | A non-zero init exits Failed and never boots a VM |
| `workload-exit-policy` | PR #27 | Exit 0 with Never or OnFailure is Succeeded. Always and an empty policy restart. Non-zero Never is Failed with that code. OnFailure and Always restart |
| `phase-monotonic` | PR #27 | The observed phases are Pending, Running, Succeeded, and Succeeded stays Succeeded. Every status read also rejects leaving a terminal phase or going from Running back to Pending |
| `exec-exit-and-attach` | PR #48, PR #35 | `kubectl exec` returns `k8s.io/utils/exec.ExitError` for exit 7 and nil for exit 0. Attach is refused |
| `exec-backpressure` | PR #32 | A stdout reader that blocks still receives every byte |
| `log-follow-snapshot` | PR #36 | A line written before follow starts is returned. A follower that cannot keep up is still allowed to drop later lines; that drop is intentional |
| `image-pull-backoff` | PR #33 | A refused pull stays Pending with `back-off`, holds no slot, and another pod can reach Running |
| `boot-backoff` | PR #34 | One failed boot stays Pending with `back-off` and no slot. `Advance(1s)` lets it reach Running |
| `sidecar-crashloop` | PR #16, PR #17 | The first sidecar restart is immediate. The next one waits, and the slot stays held |
| `sidecar-never-fails` | PR #16 | A Never sidecar that exits non-zero fails the pod and releases the slot |

Liveness delays are 10s, 20s, 40s, 80s, 160s, then 5m. Image pull and VM
boot retry from 1s, doubling to 30s, with the wait chosen in `[d/2, d]`.
`Advance(1s)` therefore always releases the first of those waits.

## client-go BackOff

Sidecar CrashLoop already uses `k8s.io/client-go/util/flowcontrol.NewBackOff(10s, 5m)`
(`k8s.io/client-go` v0.35.6). Replacing that call does not change the
sidecar case.

Replacing the hand-rolled liveness delay or the image-pull / boot retry
with that same constructor would change delays this suite fails on. The
comparison is in the pull request that added the suite; the numbers above
are the ones the tests lock in.

## Out of scope

The suite does not start `cmd/darwin-node`, does not talk to an API
server, and does not open a real registry. Pull backoff is a connection
refused to `127.0.0.1:1`. Apple hardware is not required. A guest halt
inside a Virtualization.framework VM, and a virtio-fs write from inside
that guest, are not what these cases execute.
