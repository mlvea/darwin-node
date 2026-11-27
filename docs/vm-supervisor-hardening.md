# Per-VM supervisor hardening

These checks are required before `--vm-supervisor` can default to on.
The wire protocol and the version handshake are unchanged. Lease JSON
gains two fields the agent writes and reads. They are not part of the
socket handshake.

## File descriptors

Every descriptor received with `SCM_RIGHTS` is marked close-on-exec
before it is wrapped in an `*os.File`. `MSG_CTRUNC` is an error. Any
descriptor the truncated message did install is closed. Descriptors the
supervisor sends, including the ones `OpenChannels` returns, are
close-on-exec as well.

Both the node-agent (`supervised.New`) and the supervisor
(`Server.prepare`) raise `RLIMIT_NOFILE` to 65536, or to the hard limit
when that is lower. A soft limit that is already higher is left alone.

## Death watch

Adopt watches the supervisor pid with `EVFILT_PROC`/`NOTE_EXIT` on
Darwin and with a pidfd on Linux, and it watches the control socket and
the guest fd for EOF. The two signals share one `sync.Once`. `ESRCH`
while registering the pid means the process is already dead, and the
lost path runs before the watch returns. Closing the session stops the
watch before it closes those descriptors.

A lease pid is trusted only when `host_boot_id` matches
`kern.bootsessionuuid` (Darwin) or `/proc/sys/kernel/random/boot_id`
(Linux) and `supervisor_proc_start` matches the OS start time of that
pid (`kinfo_proc` start time in nanoseconds, or `/proc/<pid>/stat`
field 22). An empty boot id, a mismatch, or a missing process fails
closed. The pid is not signalled. This is separate from the VM boot
UUID and from the supervisor's self-reported start time on the socket.

## Stop deadlines

`Machine.Stop` can spend the pod's termination grace in a clean guest
shutdown and then hang inside `VZVirtualMachine.Stop`. The hang cap is
added to that grace. It does not replace it.

| Process | Wait |
| --- | --- |
| supervisor | grace + 10s, then `os.Exit(1)` |
| node-agent | grace + 12s, then `SIGKILL` the supervisor and wait for `NOTE_EXIT` / pidfd before releasing the slot |

The two second gap lets the supervisor exit itself before the agent
signals it. A pid whose boot id or OS start time does not match the
lease is not killed. A process that is already gone is not killed.

The agent reaps supervisors it spawned with `cmd.Wait`. Spawn uses
`Setsid`. Stdin is `/dev/null`. Stdout and stderr append to
`<run dir>/slot-N.supervisor.log` (mode 0600). The parent closes its
copies after `Start`, so the agent exiting cannot deliver `SIGPIPE`.

## launchd

`deploy/launchd/io.darwin-node.node.plist` sets `AbandonProcessGroup`
true and `ProcessType` Interactive. The first leaves supervisor
sessions running when launchd restarts the agent. The second disables
App Nap so heartbeats are not deferred into the dead-man window.

## Hardware-pending

These need a Mac Mini with Virtualization.framework. They are not
covered by Linux or SimVZ tests.

1. File-descriptor flatness. With `--vm-supervisor=on`, adopt one VM
   500 times (close the previous session each time). Sample the
   node-agent with `lsof -p <agent-pid> | wc -l` before and after.
   The count stays flat. It must not grow by about 500.

2. App Nap. `launchctl print gui/$(id -u)/io.darwin-node.node` shows
   `processType = Interactive`. While a guest is running, the VM host
   process is not App-Napped.

3. Agent restart. Start a VM under the node agent, note the supervisor
   pid (`pgrep -f 'darwin-node --role=supervisor'`), then
   `launchctl kickstart -k gui/$(id -u)/io.darwin-node.node`.
   The same supervisor pid is still alive and
   `<run dir>/slot-N.sock` still accepts a connection.
