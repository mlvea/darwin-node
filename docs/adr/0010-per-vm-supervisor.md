# ADR 0010: Per-VM supervisor processes

- Status: Accepted
- Date: 2026-11-25

## Context

A `VZVirtualMachine` cannot outlive the process that created it. darwin-node
creates every VM in the virtual-kubelet process, so a crash, panic, or
upgrade kills both running CI pods. Those pods often run 30 to 90 minutes.
The host cap is still two VMs.

Moving the VM into another process requires a binary that carries the
virtualization entitlement. Entitlements do not survive `execve`, so a
separate helper signed independently is the wrong shape.

## Decision

One signed binary, two roles:

- `darwin-node --role=agent` is today's process. It is the default.
- `darwin-node --role=supervisor --slot=N` owns at most one VM and a Unix
  control socket. It does not pull images, watch Kubernetes, or serve
  :10250.

There are two fixed slots. Production runs them as LaunchAgents
`io.darwin-node.slot-0` and `io.darwin-node.slot-1` in the `gui/<uid>`
domain, with `KeepAlive` `{SuccessfulExit=false}`. A clean VM stop exits
0 and launchd does not restart it. Development can spawn the same binary
with `Setsid` and no `Pdeathsig`.

The control protocol is versioned. The handshake carries protocol major,
protocol minor, and binary version. The agent refuses an unknown major
and accepts any supervisor of the same major, so a running supervisor
keeps its old binary until its VM ends. The calls are `StartVM`,
`InspectVM`, `PauseAndSaveVM`, `StopVM`, and `AttachChannels`.
`AttachChannels` passes two descriptors: the datagram socket for
`VZFileHandleNetworkDeviceAttachment`, and a connected vsock stream to
the guest agent. The guest protocol is not parsed in the supervisor.

The agent writes an atomic boot lease (`boot_id`, HMAC token, pid, start
time, pod identity, image digest). Reattach requires `InspectVM` to echo
that lease and the pod to still exist without `DeletionTimestamp`.
Otherwise the agent stops the VM. If no agent is connected for five
minutes while a VM is running, the supervisor stops the VM and exits.

The feature is `--vm-supervisor=off|on`, default off. The engine keeps
talking to `runtime.Runtime`. A supervised adapter implements that
interface by speaking the control socket. The supervisor serves the same
interface, so tests drive it with the fake runtime.

## Consequences

- An agent crash no longer destroys a VM whose supervisor is still
  running, once the engine calls the adapter's reattach path.
- Upgrades are deferred: old supervisors finish their VM, new ones start
  on the new binary. Mixed binaries are allowed inside one protocol major.
- The run directory (mode 0700, owned by the service uid) and
  `LOCAL_PEERCRED` / `SO_PEERCRED` are the local trust boundary.
- File-handle networking replaces NAT only inside the supervisor. The
  in-process path is unchanged while the flag is off.
- macOS 13 remains the floor. Saving a paused VM needs macOS 14 and
  returns the framework error on 13.
- `github.com/Code-Hex/vz` stays on v3. The connected vsock descriptor is
  taken from the unexported connection field because v3 does not export it.

The operator notes, the threat model, and the hardware-pending list are
in `docs/vm-supervisor.md`. Wire-major skew, which holds the VM and
cordons the node instead of stopping it, is in
`docs/vm-supervisor-versioning.md`.
