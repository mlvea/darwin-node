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
2. **Authenticated TCP (fallback).** Guest listens on a well-known port
   (`1050/tcp`). Host dials the guest IP once discovered. Shared token (injected
   via virtio-fs at boot) authenticates the first frame. Optional TLS.
3. **SSH (last resort).** Kept for images that have not yet been baked with the
   agent. Degraded: no log stream, probes are exec-over-SSH, no secret
   materialization.

The guest **dials the host** on vsock (guest-initiated) because Apple's
documented model is: guest `AF_VSOCK` works; host must use the framework. The
host installs a `VZVirtioSocketListener` on a well-known port; the agent
connects at launch and keeps a persistent session. If the listener is missing
(old host), the agent falls back to TCP listen.

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
| `Shutdown` | host->guest | graceful halt; host then `VM.Stop` |
| `Exit` | guest->host, notify | one-way exit code; no reply |

### Exit notify

An unexpected VM death does not call `Shutdown`. `Shutdown` is the host
asking the guest to stop, and its payload is a reason and a timeout. The
serial console is a raw byte stream and is off unless `--serial-console`
is set, so it is not this channel.

The guest writes one frame on the agent connection it already has (vsock
port 50051, or TCP when that fallback is on) and then halts:

```json
{"v":1,"id":"exit","kind":"notify","method":"Exit","payload":{"exitCode":137,"reason":"OOMKilled"}}
```

`exitCode` is required. Zero is a real code. A missing or null code is
not a report, and the container stays at exit code 1 with the host reason
(`VMStopped` or `VMFailed`). `reason` is optional. The host keeps it when
it is 1–128 characters, starts with a letter, and continues with letters
or digits. Anything else is dropped and the host reason stays. The last
report that has a code wins. A later frame with no code does not clear it.
The pod phase stays `Failed` even when the code is 0. The pod message
stays the host's ("macOS VM Stopped unexpectedly"). Only
`containerStatuses[].state.terminated.exitCode` and, when the reason is
accepted, `terminated.reason` change.

`ProtocolVersion` stays 1. An older host drops an unknown id, so this
frame is additive. A graceful `Delete` still publishes `Succeeded` and
exit 0 before the VM is torn down. A liveness restart clears the code so
the new container does not inherit it.

`Handler.ReportExit` is the in-process call that writes the frame.
`cmd/guest-agent` serves the connection and does not call it, and it does
not halt the VM. A guest image that wants a real `terminated.exitCode`
calls `ReportExit` on its live handler before it halts. The fake runtime's
`ReportExit` is the Linux test seam for the same frame.

## In-guest lifecycle

launchd job `io.darwin-node.guest-agent`:

- `KeepAlive` true, `RunAtLoad` true
- Starts as root (needed for shutdown, mounts, some probes)
- Writes logs to `/var/log/darwin-guest-agent.log` (and os_log)
- On vsock connect failure, retries with backoff and opens TCP fallback
- Crash recovery is launchd's job; the host treats missing handshake as
  `ContainerStarting` then `Failed` after `AgentReadyTimeout`

## Security

- vsock is the trust boundary: only the hosting process can accept.
- TCP fallback requires a per-pod random token, 32 bytes, written mode 0600
  into the virtio-fs control share, never into the Pod spec.
- Exec does not imply a login shell; the host sends argv + env explicitly.
- Secrets are copied by the agent to the requested path with the requested
  mode, then the host may unshare the secret directory.

## Consequences

- Images without the agent still boot (SSH fallback) but are marked with
  condition `GuestAgent=False`.
- Bake tooling (`darwin-image inject-agent`) is part of the supported path.
- We must test vsock on real hardware; CI uses the TCP loopback transport.
