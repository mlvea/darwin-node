# VM supervisor recovery

With `--vm-supervisor` on, a restart of the node-agent process adopts a VM
whose supervisor is still alive. The flag stays off unless the process is
started with it. This note is the engine half of that adopt. The supervisor
process, the lease, and the control socket are described in
`docs/vm-supervisor.md`.

## Startup

`Engine.Recover` asks the runtime for `Recover` before it deletes unheld
pod directories. The supervised adapter implements that method. The metrics
wrapper in front of the runtime forwards it, and forwards `SetAgentToken`,
because production wraps the runtime before the engine is built. A runtime
that does not implement it (the default fake and vz paths, including when
they are wrapped) returns no machines. Those pods stay on the existing
node-restart settlement.

For each supervisor slot:

- The lease matches a pod directory left under `cache/pods/<uid>`. The VM
  is adopted on a fresh guest connection. The engine holds that pod's slot,
  so the directory, the disk, and `control/agent-token` are kept. No new VM
  is booted.
- No pod directory matches the lease. The supervisor is told to stop the
  VM and the slot is released.
- The supervisor is not listening, or the boot fence fails. Nothing is
  adopted. `RestartPolicy` `Never` stays `Failed` with reason
  `NodeRestarted`, and the slot stays free. `Always` and `OnFailure` still
  receive one restart credit and boot again. That is the same settlement
  the engine already used when every VM died with the process.

When virtual-kubelet submits the pod, the engine claims the adopted VM
before that settlement. It reads the agent token back from
`cache/pods/<uid>/control/agent-token`, redials the guest through the
descriptor this `Recover` attached, and marks the pod `Running`. The
restart count already on the pod is kept, and so is `status.startTime`
when the pod has one. Init containers are not run again, and the container
workload is not applied again. Host volume files and emptyDir limits are
rebuilt from the pod. The guest is not asked to materialize them again.
Sidecars are created again, because they exited with the agent process.

A pod that is already `Succeeded` or `Failed`, a pod with a deletion
timestamp, or a namespace and name that do not match the machine is not
marked `Running`. That VM is stopped, the slot is released, and the
node-restart settlement runs.

`Close` stops an adopted VM that virtual-kubelet never submitted. A `Stop`
that does not return within the close deadline leaves the slot held, which
is the same rule as a pod `Stop` that does not return.

The engine slot table and the supervisor slot table both cap at two. A
third pod is rejected while both adopted VMs are held. After drain, both
tables are empty and both VMs are stopped.

## Supervisor loss while the pod is running

The adapter marks the machine `Failed` with message `supervisor lost` and
releases its slot once. A later `Stop` does not free a slot a new VM has
taken. The engine copies that text onto the pod: reason and message are
both `supervisor lost`. Restart policy does not run, so `Always` does not
boot a replacement. Any other unexpected stop still uses the generic text,
reason `VMFailed` and message `macOS VM Failed unexpectedly`.

## What this does not change

`--vm-supervisor` stays default off. No flag was added. With the flag off,
startup recovery only sweeps caches and settles interrupted pods, as
before.

`Connect` during `Start` on real Virtualization.framework is
hardware-pending. Adopt calls `Connect` immediately. A guest that is not
listening yet can stall that call. SimVZ registers its agent during
`Start`, so the Linux tests do not show the stall. SimVZ's channel peer is
a drain of the virtio-vsock stand-in, not the guest-agent stream, so the
engine redial tests use the fake runtime with the guest bridged onto the
adopted descriptor. Re-adopt of a SimVZ VM with a fresh `Connect` is
covered by `pkg/runtime/supervised`.
