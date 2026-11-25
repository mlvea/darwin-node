# VZ contract

`pkg/vzcontract` is the shared check that SimVZ and Virtualization.framework
are being held to the same post-conditions. A test passes a factory. The
suite asserts state transitions, NSError codes, callback order, and file
descriptors. It does not record socket bytes.

Linux CI runs the suite against SimVZ (`go test -race ./pkg/runtime/simvz/`).
Each subtest watches for a goroutine leak and for a rise in `/proc/self/fd`
(on Darwin, `/dev/fd`). The race detector is allowed two extra descriptors.
A manual clock replaces the 30s idle wait, so nothing in the suite sleeps
for half a minute.

## What the suite locks

- `Stop` and `RequestStop` may deliver the completion handler and the KVO
  `Stopped` notification in either order. `guestDidStop` is in that same
  set. `Stopping` is published first. `Options.Order` forces
  `kvo_before_stop_completion` or `stop_completion_before_kvo`, or `jitter`
  with a seed. A failing jitter run prints the seed.
- Callbacks for one VM run on a single actor. A callback that calls back
  into that VM synchronously panics (`synchronous re-entry`).
- `Connect` returns `ECONNREFUSED` when the agent has not registered. It
  does not block, and it does not accept on a listener created before the
  agent is ready. An injected completion delay of 50ms to 2s is honored.
  The 2s case uses the manual clock.
- A live vsock fd stays open across `Pause` and `Save`. Writes succeed
  until the buffer fills, then block. Reads block and do not see EOF.
  `Stop` and crash: reads return EOF, writes return `EPIPE` or
  `ECONNRESET`. After `Restore` the old connection is dead. A new
  `Connect` is required.
- Creating a third VM succeeds. `Start` fails asynchronously with
  `VZErrorDomain` code 6 `VZErrorVirtualMachineLimitExceeded` and ends in
  `Error`, which releases the slot. `Restore` of a third live VM fails
  with the same code and stays `Stopped`. Code 9 is
  `VZErrorOperationCancelled`. The second-opinion note that called the
  limit "code 9" does not match Code-Hex/vz or public NSError reports.
- The slot is held in `Starting`, `Running`, `Pausing`, `Paused`,
  `Saving`, `Resuming`, `Stopping`, and `Restoring`. Only `Stopped` and
  `Error` release it.
- `Save` requires `Paused`. Anything else is
  `VZErrorInvalidVirtualMachineState` (code 3). Success stays `Paused`.
  The file is a JSON descriptor of machine id, hardware model, MAC, CPU
  count, and memory size. `Restore` requires `Stopped` and the same
  values. A mismatch is `VZErrorRestore` (code 12) and does not change
  state. Success ends `Paused` and takes a slot.
- The file-handle network attachment raises `SO_SNDBUF` toward 256KiB and
  `SO_RCVBUF` toward 1MiB before the fd is used. A full nonblocking
  datagram returns `ENOBUFS` or `EAGAIN` rather than disappearing. The
  suite allows an injected `ENOBUFS` burst. The production raise lives in
  `pkg/runtime/vz` and is not modified here; SimVZ models the same goals.

`ArmBarrier(name)` / `WaitBarrier` / `ReleaseBarrier` park a transition
on the hooks in `pkg/runtime/simvz/barriers.go` (`before_kvo_stopped`,
`before_stop_completion`, and the other `before_*` names).

## Traces

`pkg/vzcontract/testdata/vz_traces/*.jsonl` is the Day-1 corpus. Every
file is labelled `source: derived` and `not hardware-recorded`. Linux
replay runs each op list against SimVZ and requires:

- the recorded event order, including both stop permutations
- no completion error code outside that file's envelope

The schema and the file list are in the directory README.

## Mac Mini

Execution against a real `VZVirtualMachine` is hardware-pending. The
darwin/arm64 test compiles in CI and skips unless
`DARWIN_NODE_VZ_HARDWARE=1`.

```bash
# Compiles on a Mac. Skips unless the variable is set.
go test ./pkg/vzcontract/ -count=1 -run 'TestVZErrorCodeConstants|TestRealVZContract'

# Does not boot a guest today. With the variable set, the factory returns
# a hardware-pending error until a signed helper and a restore bundle exist.
DARWIN_NODE_VZ_HARDWARE=1 go test ./pkg/vzcontract/ -count=1 -run TestRealVZContract -timeout 30m
```

`TestVZErrorCodeConstants` does run on the macOS CI job. It checks
Code-Hex/vz's limit, cancelled, invalid-state, and restore constants. It
does not start a VM.

`Record(ctx, vm, writer)` is the capture hook. It drains
`StateChangedNotify` and appends `state_change` lines. `dialPort` is the
`ConnectToPort` shape; Apple's documentation says a connect to a port the
guest is not listening on does nothing, so the ECONNREFUSED behaviour
above is a derived hypothesis until a trace shows otherwise.

## Promoting a hardware trace

1. On the Mac Mini, point `Record` at a live `*vz.VirtualMachine` and run
   the operation you want (stop, third start, save, connect).
2. Copy the JSONL into `pkg/vzcontract/testdata/vz_traces/`.
3. Edit it into the op schema: a `meta` line with `"source":"hardware"`,
   then `create` / `start` / `stop` / … lines whose `events` are what the
   framework actually emitted. Remove `not hardware-recorded` from the note.
4. Replay on Linux:

   ```bash
   go test ./pkg/runtime/simvz/ -count=1 -run TestVZTraceReplay
   ```

   SimVZ must produce that ordering and must not invent an error code the
   file does not contain. If it cannot, change SimVZ, not the trace.
5. Leave the derived files in place until the hardware file covers the
   same transition. A hardware file is allowed to disagree with a derived
   one; that disagreement is the point of recording it.
