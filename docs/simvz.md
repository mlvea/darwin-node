# SimVZ

SimVZ is a Linux-runnable stand-in for the Virtualization.framework
runtime. It implements `runtime.Runtime` in `pkg/runtime/simvz` so the
engine can boot, dial, and stop a VM without a Mac. The fake runtime stays
the synchronous stub used by the rest of the unit tests. SimVZ is the one
that opens real file descriptors and can fail them on purpose.

It is not wired to `--runtime`. That flag still selects `vz` or `fake`.
Pointing the node process at SimVZ means editing `pkg/config` and
`cmd/darwin-node`, which this change does not do.

## What it actually opens

- An `AF_UNIX` `SOCK_STREAM` socket carries guest-agent frames. That is the
  virtio-vsock stand-in. On hardware the guest dials the host. Here the
  guest listens and the host dials, because the agent's test mode is a
  listener. After connect, the bytes are the same `pkg/guest` frames.
- An `AF_UNIX` `SOCK_DGRAM` socketpair stands in for
  `VZFileHandleNetworkDeviceAttachment`. Both ends stay open until `Stop`.
- `Options.GuestBinary` execs that `darwin-guest-agent`. Leave it empty and
  the protocol server runs in-process on the same socket. `GuestAgentBinary`
  builds `cmd/guest-agent` once per process.

The 2-VM cap is enforced in the simulator. `Create` does not take a slot.
A third `Start` or `Restore` returns `ErrVirtualMachineLimitExceeded`
asynchronously and ends in `Error` (`Start`) or stays `Stopped`
(`Restore`, state unchanged). The error text includes
`VZErrorVirtualMachineLimitExceeded` and `VZErrorDomain code 6`. Code 9
is `VZErrorOperationCancelled`, not the limit. `MaxRunning` may only
lower the cap. The slot is held in `Starting`, `Running`, `Pausing`,
`Paused`, `Saving`, `Resuming`, `Stopping`, and `Restoring`. Only
`Stopped` and `Error` release it.

## States

`Start` is legal from `Stopped` or `Error` and moves through `Starting` to `Running`.
`Stop` is legal from `Running` or `Paused` and moves through `Stopping`.
`Pause`, `Resume`, and `Save` are on the concrete machine, not on
`runtime.Machine`:

| Call | From | Through | To | Slot |
| --- | --- | --- | --- | --- |
| `Start` | Stopped or Error | Starting | Running | acquired |
| `Pause` | Running | Pausing | Paused | kept |
| `Resume` | Paused | Resuming | Running | kept |
| `Save` | Paused | Saving | Paused | kept |
| `Restore` | Stopped or Error | Restoring | Paused | acquired |
| `Stop` | Running or Paused | Stopping | Stopped | released |

`Save` writes a JSON descriptor (machine id, hardware model, MAC, CPU
count, memory size) and leaves the VM `Paused`. `Restore` requires
`Stopped` and the same identity; a mismatch is `VZErrorRestore` (code 12)
and does not change state. Any other illegal call returns `ErrInvalidState`
(`VZErrorInvalidVirtualMachineState`, code 3). `Status` maps Pausing,
Paused, Saving, and Resuming onto `VMRunning`, Stopping onto `VMStopping`,
Restoring onto `VMStarting`, and Error onto `VMFailed`. `State()` returns
the framework name. `Status().Message` is that same name.

`Stop` and `RequestStop` publish `Stopping` first. `guestDidStop`, the
KVO `Stopped` notification, and the completion handler then arrive in an
order selected by `Options.Order`:

- `kvo_before_stop_completion` (the default): guest, KVO, completion
- `stop_completion_before_kvo`: guest, completion, KVO
- `jitter`: a permutation of those three. `OrderSeed` reproduces it. Seed
  0 draws one from the wall clock; a failing test prints it.

`ArmBarrier`, `WaitBarrier`, and `ReleaseBarrier` park the transition at
the named hook (`before_kvo_stopped`, `before_stop_completion`, and the
other `before_*` names in `barriers.go`). Per-VM callbacks run on one
actor goroutine. A synchronous call back onto that actor panics with
`synchronous re-entry`.

`Connect` does not block when the agent has not registered. It waits
`Faults.ConnectDelay` (logical time, including an injected 50ms–2s) and
returns `ECONNREFUSED`. `HoldAgent` keeps `Start` from listening early, so
a pre-created socket cannot hide that race. `RegisterAgent` is what makes
the next connect succeed. A live vsock fd stays open across `Pause` and
`Save`: writes fill the buffer and then block, and reads block with no
EOF. `Stop` and `Crash` make reads return EOF and writes return `EPIPE`
or `ECONNRESET`. `Restore` does not revive the old connection.

The datagram socketpair has `SO_SNDBUF` raised toward 256KiB and
`SO_RCVBUF` toward 1MiB before either end is returned, matching
`pkg/runtime/vz`. The kernel may clamp that. A full nonblocking send
returns `ENOBUFS` or `EAGAIN`. `Faults.DatagramENOBUFS` injects a burst
of `ENOBUFS` before a real write.

## Faults

`Options.Faults` is the plan. `Options.Seed` fills it from `FaultsFromSeed`
only when `Faults` is the zero value. The same seed always returns the same
plan.

| Field | Effect |
| --- | --- |
| `FailStart` | That many `Start` calls open the NIC and the vsock listener, then fail with `simvz: start() failed` and close both. The budget is per runtime, so a retry that creates a new machine still consumes it. |
| `VsockEOFAfter` | The host read of the vsock stream returns EOF after this many bytes. `6` cuts the first frame after its 4-byte length. |
| `Stall` | The host's first write waits this long, so the handshake does not reach the guest. The agent idle watchdog, armed at accept, closes the silent connection. Logical time. |
| `CloneDelay` | The disk clone waits this long, then writes `disk.img`. Logical time. |
| `CrashGuest` | The guest is killed. `CrashAfter == 0` kills before `Start` returns. A positive value waits that logical duration after `Running`. The VM stays `Running` with `AgentOK` false. |
| `HoldAgent` | `Start` reaches `Running` without a guest listener. `Connect` and `DialAgent` return `ECONNREFUSED` until `RegisterAgent`. |
| `ConnectDelay` | Logical wait before `Connect` or `DialAgent` samples the listener. It does not poll. |
| `DatagramENOBUFS` | That many host datagram writes return `ENOBUFS` and do not queue the payload. |

`Options.Clock` scales those waits. `NewScaledClock(8)` turns 30s into 3.75s
and 50s into 6.25s. The guest idle watchdog polls every 250ms and does not
see this clock, so the scale also shortens the idle timeout passed to the
agent. A scale that shrinks a 30s idle under 250ms will not reproduce the
stall. The tests use scale 8 and a 50s stall against the 30s idle so a late
watchdog tick still loses. A 35s stall is the same fault with a smaller gap.

`NewManualClock` advances `Now` inside `Sleep` and reports `Wall` as zero,
so the 35s-versus-30s case finishes in milliseconds. `testing/synctest`
cannot: the guest is blocked in a socket read, which does not let the
bubble's fake clock run. On a manual clock the stalled conn closes itself
after the logical idle instead of waiting for that watchdog.

`Options.Transition` is the logical pause spent inside Starting, Pausing,
Saving, and Stopping.

`Stop`'s `graceful` argument is wall time. The engine passes a real
termination grace, and scaling it would hide a stuck stop.

## Guest agent listen mode

`darwin-guest-agent` does not listen unless asked. Both default off:

- `--unix-listen` or `DARWIN_NODE_AGENT_UNIX_LISTEN`
- `--idle-timeout` or `DARWIN_NODE_AGENT_IDLE` (empty keeps the 30s protocol default)

There is no `--listen` and no TCP listener. With `--unix-listen` set, the
process accepts on that socket and does not dial vsock. Without it, behavior
is unchanged.

## Tests

```
go test ./pkg/runtime/simvz/ ./pkg/engine/ -count=1 -race -run 'SimVZ|TestLifecycle|TestFault|TestState|TestGuest|TestThird|TestStart|TestMid|TestStall|TestChild|TestClone|TestDial|TestMax|TestStop|TestPause|TestWrong'
```

`go test ./pkg/runtime/simvz/ -race` runs goleak and compares `/proc/self/fd`
(or `/dev/fd`) before and after each lifecycle. `pkg/engine/simvz_engine_test.go`
runs the engine against an injected runtime: a real guest-agent child for
the happy path, and in-process guests for start failure, mid-frame EOF, the
scaled stall, and a guest crash.

The shared suite lives in `pkg/vzcontract`. `go test ./pkg/runtime/simvz/ -run 'TestVZContract|TestVZTraceReplay'` runs it, including the JSONL corpus under `pkg/vzcontract/testdata/vz_traces`. How to run the same suite on a Mac Mini is `docs/vzcontract.md`.

## Not covered here

SimVZ does not boot a `VZVirtualMachine`, does not call
`hv_apple_isa_vm_quota`, and does not attach a virtio socket device. Those
checks stay on Apple hardware.
