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

The 2-VM cap is enforced in the simulator. A third `Start` returns
`ErrVirtualMachineLimitExceeded`. The error text includes
`VZErrorVirtualMachineLimitExceeded` and `VZErrorDomain code 6`. `Create`
does not count. `MaxRunning` may only lower the cap.

## States

`Start` is legal from `Stopped` and moves `Stopped → Starting → Running`.
`Stop` is legal from `Running` or `Paused` and moves through `Stopping`.
`Pause`, `Resume`, and `Save` are on the concrete machine, not on
`runtime.Machine`:

| Call | From | Through | To | Slot |
| --- | --- | --- | --- | --- |
| `Start` | Stopped | Starting | Running | acquired |
| `Pause` | Running | Pausing | Paused | kept |
| `Resume` | Paused | Starting | Running | kept |
| `Save` | Paused | Saving | Stopped | released |
| `Stop` | Running or Paused | Stopping | Stopped | released |

Any other call returns `ErrInvalidState`. `Status` maps Pausing and Paused
onto `VMRunning`, and Saving and Stopping onto `VMStopping`, because
`types.VMState` has no names for those. `State()` returns the framework name.
`Status().Message` is that same name.

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

`Options.Clock` scales those waits. `NewScaledClock(8)` turns 30s into 3.75s
and 50s into 6.25s. The guest idle watchdog polls every 250ms and does not
see this clock, so the scale also shortens the idle timeout passed to the
agent. A scale that shrinks a 30s idle under 250ms will not reproduce the
stall. The tests use scale 8 and a 50s stall against the 30s idle so a late
watchdog tick still loses. A 35s stall is the same fault with a smaller gap.

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

## Not covered here

SimVZ does not boot a `VZVirtualMachine`, does not call
`hv_apple_isa_vm_quota`, and does not attach a virtio socket device. Those
checks stay on Apple hardware.
