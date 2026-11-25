# ADR 0002: Guest Agent language and communication

- Status: Accepted
- Date: 2026-08-23
- Phase: 0 / 2

## Context

A pure SSH control plane is not production-grade:

- Image must ship a working sshd, user, and credentials shared with the host.
- `kubectl logs` for the VM is impossible without a log shipper.
- Probes, secret rotation, and metrics require ad-hoc remote scripts.
- Graceful shutdown via `sudo shutdown -h now` over SSH is racy and password-sudo
  dependent (Agoda's current path).
- SSH readiness is a proxy for "the workload is ready", which it is not.

The Guest Agent is non-negotiable. It must start via launchd, survive reboot,
and be injectable into every base image.

## Language choice

| | Swift | Go |
|---|---|---|
| Native IOKit / os_log / launchd | Best | Good enough via syscalls + `log` CLI |
| Single-language repo | No | Yes (host is Go) |
| Protocol, streaming, tests | Extra toolchain | First-class |
| Static-ish `darwin/arm64` binary | Harder (dylibs, signing) | Easy |
| vsock (`AF_VSOCK`) | Native | `golang.org/x/sys/unix` |
| Metal/GPU introspection | Best | Can shell out / IORegistry |

**Decision: Go for the Guest Agent.** Same module, shared protocol types, one
test harness, one release pipeline. A small Swift helper can be added later
*only* if we need IOKit GPU counters that gopsutil cannot see. That helper
would be an optional sidecar inside the guest, not a second control plane.

## Communication strategy

Priority order, all behind `pkg/guest.Transport`:

1. **vsock (primary).** Host: `VZVirtioSocketDevice.Connect(port)`. Guest:
   `socket(AF_VSOCK)` to `VMADDR_CID_HOST` (2). Not visible on the IP network,
   independent of NAT/bridged, works before DHCP. Apple's host-side vsock is
   *not* `AF_VSOCK` (ENODEV); it is a file descriptor from Virtualization.framework.
   Code-Hex/vz exposes that FD as `net.Conn`.
2. **TCP fallback — removed 25 Nov 2026.** It listened on `1050/tcp` and
   sent the pod token in the clear. Over the shared NAT the other VM can
   sniff or stall that socket. The host does not dial it and the guest
   agent does not listen. See the amendment below.
3. **SSH (last resort).** Kept for images that have not yet been baked with the
   agent. Degraded: no log stream, probes are exec-over-SSH, no secret
   materialization.

The guest **dials the host** on vsock (guest-initiated) because Apple's
documented model is: guest `AF_VSOCK` works; host must use the framework. The
host installs a `VZVirtioSocketListener` on a well-known port; the agent
connects at launch and keeps a persistent session. If the listener is missing
the agent retries vsock. It does not listen on TCP.

Well-known vsock port: `50051`.

## Wire protocol

Length-prefixed JSON (4-byte big-endian length + UTF-8 JSON). Not protobuf in
Phase 2: zero codegen, easy to tcpdump, easy to test. Framing is versioned
(`v=1`). Streaming methods (exec, logs) use the same connection with
`id` + `type=stream` frames. A later ADR may switch the *payload* to protobuf
without changing the transport.

Methods:

| Method | Direction | Notes |
|---|---|---|
| `Handshake` | both | token, versions, hostname |
| `Health` | host->guest | liveness of the agent itself |
| `Ready` | host->guest | agent-reported workload ready |
| `Exec` | host->guest, stream | env, cwd, TTY, stdin/stdout/stderr, exit |
| `Logs` | host->guest, stream | follow, tail, timestamps, since |
| `Probe` | host->guest | exec / httpGet / tcpSocket |
| `Metrics` | host->guest | CPU, memory, disk, net, optional GPU |
| `NetInfo` | host->guest | interface IPs, primary pod IP |
| `Materialize` | host->guest | map shared-dir entries onto mountPaths |
| `Shutdown` | host->guest | sync, then halt; host then `VM.Stop` |
| `Workload` | host->guest | VM container command, args, env, directory |
| `Exit` | guest->host, notify | one-way exit code; no reply |

### Exit notify

An unexpected VM death does not call `Shutdown`. `Shutdown` is the host
asking the guest to stop, and its payload is a reason and a timeout. The
serial console is a raw byte stream and is off unless `--serial-console`
is set, so it is not this channel.

The guest writes one frame on the vsock connection it already has (port
50051) and then halts:

```json
{"v":1,"id":"exit","kind":"notify","method":"Exit","payload":{"exitCode":137,"reason":"OOMKilled"}}
```

`exitCode` is required. Zero is a real code. A missing or null code is
not a report, and the container stays at exit code 1 with the host reason
(`VMStopped` or `VMFailed`). `reason` is optional. The host keeps it when
it is 1–128 characters, starts with a letter, and continues with letters
or digits. Anything else is dropped and the host reason stays. The last
report that has a code wins. A later frame with no code does not clear it.
Exit 0 with `RestartPolicy` `Never` or `OnFailure` sets phase `Succeeded`
and reason `Completed` (or the accepted guest reason). `Always`, including
an empty policy, restarts the VM. A non-zero code restarts under `Always`
and `OnFailure`, and stays `Failed` under `Never`. A stop with no code
stays `Failed` with the host message ("macOS VM Stopped unexpectedly").

`ProtocolVersion` stays 1. An older host drops an unknown id, so this
frame is additive. Handshake may also carry `idleMs` and `stdinWindow`.
An older guest omits both. The host then heartbeats on a 30s idle and
sends stdin without waiting for credit. `KindWindow` is a credit grant
in either direction: host to guest for stdout, guest to host for stdin.
An older host treats a guest credit frame as an empty exec event and
continues. Delete, drain, and eviction of a pod whose workload
has not exited publish `Failed` with exit 137 when the grace period is 0
and 143 otherwise, reason `Error`. A workload that already exited 0 stays
`Succeeded`. A liveness restart clears the code so the new container does
not inherit it.

`Handler.ReportExit` is the in-process call that writes the frame.
`darwin-guest-agent` with no workload serves the connection and does not
send a code or halt. `darwin-guest-agent run -- <command>` runs that
command as the main workload. When the process exits, the agent writes
this frame with the real exit code and then halts. Exit 0 uses reason
`Completed`. Any other code, including a signal death mapped to `128+N`,
uses reason `Error`. The send waits at most 2s. A host that never reads
is disconnected so the halt is not stuck. Halt is `shutdown -h now` on
Darwin and a no-op elsewhere, so a Linux test can inject its own halt.
The stock launchd job still starts the agent with no workload. An image
that wants `terminated.exitCode` starts the workload with `run`. The fake
runtime's `ReportExit` is the Linux test seam for the same frame.

`Workload` is how the host applies the VM container's `command`, `args`,
`env`, and `workingDir` after the agent is ready. The process stdout and
stderr are appended to the agent log ring, one line at a time, and the
exit frame is sent when it exits. A second `Workload`, or `run` plus a
pod command, fails with `workload already started`. An older agent
answers `unknown_method`. The host then rejects the pod instead of
leaving the command unused. A pod with no command does not send
`Workload`. `Shutdown` syncs the filesystem and then halts. On Darwin
that is `sync(2)` and `shutdown -h now`. The halt runs even when sync
returns an error. Elsewhere both are no-ops.

## In-guest lifecycle

launchd job `io.darwin-node.guest-agent`:

- `KeepAlive` true, `RunAtLoad` true
- Starts as root (needed for shutdown, mounts, some probes)
- Writes logs to `/var/log/darwin-guest-agent.log` (and os_log)
- On vsock connect failure, retries with backoff. There is no TCP listen.
- Crash recovery is launchd's job; the host treats missing handshake as
  `ContainerStarting` then `Failed` after `AgentReadyTimeout`

## Security

- vsock is the trust boundary: only the hosting process can accept.
- The pod token is 32 random bytes, written mode 0600 into the virtio-fs
  control share, never into the Pod spec, and sent only on the vsock
  handshake.
- Exec does not imply a login shell; the host sends argv + env explicitly.
- Secrets are copied by the agent to the requested path with the requested
  mode, then the host may unshare the secret directory.

## Consequences

- Images without the agent still boot (SSH fallback) but are marked with
  condition `GuestAgent=False`.
- Bake tooling (`darwin-image inject-agent`) is part of the supported path.
- We must test vsock on real hardware. CI uses `net.Pipe` and the fake
  runtime, not a TCP guest transport.

## Amendment (25 Nov 2026)

The plaintext TCP fallback described above is removed. The guest agent
is reached only over vsock. `--agent-tcp-fallback`, `--listen`, and
`DARWIN_NODE_AGENT_TCP_FALLBACK` are gone; setting the variable does
not open a listener and does not make the host dial TCP. SSH fallback
is unchanged: opt-in, key-only, for images that have no agent.

Handshake is additive. The host sends `minProtocol`, `maxProtocol`, and
its capability list. The guest replies with the selected protocol, its
version, and its own list. The host keeps the intersection for that
session. An older agent that omits the list still handshakes; the
intersection is empty. Unknown JSON fields are ignored.

Control frames are capped at 64 KiB. Stream and materialize frames keep
the 16 MiB cap. An over-limit frame, or one nested deeper than 32,
closes the session. Workload exit codes are stored in 0..255. Guest
status and event text is truncated to 1024 printable UTF-8 characters.
`kubectl exec` returns the process exit code. Named probe ports are
resolved from the container's ports. Guest CPU and memory are measured
in the agent and are untrusted telemetry, not a scheduling input.
