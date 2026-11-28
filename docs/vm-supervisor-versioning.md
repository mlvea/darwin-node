# Supervisor version skew

A node-agent upgrade can leave a per-VM supervisor running from the
previous build. The two processes agree a wire version on the control
socket before the agent adopts the VM. This is separate from the frame
protocol major (`protocol_major`), which still has to match for a
request to be accepted.

## Handshake

The supervisor hello carries:

| Field | Meaning |
| --- | --- |
| `wire_major` | Contract generation. Equal majors re-adopt. |
| `wire_minor` | Additive field set. A new field bumps only this. |
| `build_sha` | Git sha of the supervisor. Not a compatibility check. |
| `capabilities` | Optional features the peer implements. |
| `slot` | Supervisor slot, 0 or 1. |
| `pod_uid` | Pod the VM belongs to. |
| `host_boot_id` | Host boot the supervisor was started in. |
| `workload_started` | The guest Workload RPC already ran. |
| `workload_exited` | That workload has finished. |
| `exit_code` | Guest exit code when `workload_exited` is set. |

Both sides ignore JSON fields they do not know. A minor that is ahead
or behind an equal major is re-adopted. Unknown capability names do not
turn features on. The features this agent understands are `heartbeat`
and `pause`. Pause is refused when the peer does not list it.

`wire_minor` is 0 for the field set in the table. Adding a field bumps
`wire_minor` and the key list checked by
`TestHandshakeMinorTracksFieldSet` in the same change.

## Equal major

The agent re-adopts the VM whatever `build_sha` or `wire_minor` is.
Optional work is gated by `capabilities`. The adopt still fences a
deleting pod and still stops a VM whose lease does not match.

`workload_started: true` means the agent does not call the guest
Workload RPC again. A `restartPolicy: Never` command must not run
twice. `workload_exited` with `exit_code` is copied into the pod
status: 0 is Succeeded, any other code is Failed with that code. The
VM is then stopped because the workload is finished.

`workload_started: false` on an equal-major re-adopt means the command
may be sent once. That is the crash before the note was recorded.

## Mismatched major

The agent does not call StopVM.

- The slot stays occupied and counts against the two-VM budget.
- The node is cordoned: `spec.unschedulable` is set, condition
  `SupervisorVersionSkew` / `WireMajorMismatch` is set, and a warning
  event uses the same reason. The message names both wire majors.
- The agent keeps the control connection (or heartbeats it) so the
  supervisor dead-man stays disarmed. It watches the supervisor pid
  recorded in the lease.
- When that supervisor exits on its own, the slot is released once.
- The node is made schedulable again only when this agent owns the
  cordon. Ownership is the annotation
  `darwin-node.io/cordoned-by=supervisor-skew`.
- Two mismatched supervisors keep the cordon until the last one exits.

Before the first hold, the agent reads the Node. A node that is
already unschedulable, and does not carry that annotation, stays
unschedulable when the last held supervisor exits. A node that already
carries the annotation was cordoned by a previous process of this
agent; the new process clears it once, after the last supervisor exits.

An operator who cordons during a hold, or who replaces the annotation,
keeps the cordon. The agent re-reads the Node before it clears
`spec.unschedulable`, and it clears the bit only while the annotation
is still `supervisor-skew`. An operator who clears `spec.unschedulable`
during a hold is not cordoned again. The agent logs a warning and
emits one `SupervisorSkewOperatorUncordon` event.

The cordon and the clear are JSON patches against the Node, not the
status subresource. When the Node has a resourceVersion, the patch
tests it and retries if the object changed. The client's RBAC needs
`patch` on `nodes` as well as `nodes/status`. With
`--rotate-client-certificates` the node client does not exist yet when
recovery runs; call `(*engine.Engine).SetSkewNodeClient` after it
does. Until that call, a hold does not assume the node was schedulable.

A supervisor that does not send `wire_major` is wire major 0. That is
the fail-closed path, same as an explicit 0 or any other mismatch.

A protocol-major mismatch is also a hold: the agent does not call
StopVM. The server rejects the hello before it counts the connection,
so this path cannot refresh the dead-man. The supervisor's own timer
can still stop the VM. The agent watches the pid and the socket.

## Hardware-pending

These need a Mac Mini and a real Virtualization.framework guest. The
tests in this change use the fake runtime and unix sockets.

1. Upgrade the node-agent while a real VZ supervisor from the previous
   build is running a VM. The node becomes unschedulable with
   `SupervisorVersionSkew`, the VM stays up, and the slot is not given
   to another pod.
2. Let that supervisor exit on its own (the VM stops; the new agent
   does not send SIGKILL). The slot frees once. The node becomes
   schedulable only if `darwin-node.io/cordoned-by=supervisor-skew`
   is still set. An operator cordon without that annotation stays.
3. Re-adopt a same-major supervisor with a different git sha. The pod
   reattaches, and a `restartPolicy: Never` command is not started
   twice.
