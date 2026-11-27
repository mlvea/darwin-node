# Guest Adopt

Adopt is the fail-closed reseed barrier between a booted guest and any
volume placement or workload. The host sends it after the agent dials and
answers Ready, and before Materialize or Workload. A guest that cannot
prove the reseed does not run a workload. The VM is stopped and that start
fails.

Virtualization.framework has no vmgenid. A restored guest keeps the kernel
CSPRNG and the address space of processes that were already running. XNU
reseeds the kernel CSPRNG synchronously when 64 bytes are written to
`/dev/random` (`write_random`). Processes exec'd after that read fresh
entropy. The engine calls Adopt on every cold start and every VM restart.
A later save/restore pool would call it on restore as well. This tree
does not restore snapshots.

Per-pod `MacMachineIdentifier` and NVRAM are chosen on cold boot. See
[vm-identity.md](vm-identity.md). Pods started from one image do not
share that identifier or that NVRAM file. Adopt reports
`io_platform_uuid` and does not rotate the ECID.

## Wire

Protocol version stays 1. Adopt is a new method, `Adopt`, and a new
capability, `adopt`. An older agent omits the capability and answers
`unknown_method`. JSON names are snake_case. `host_entropy_64b` is
standard base64. The sample and the proof are lowercase hex. There is no
session-token field. A token stuffed into the object is ignored.

Request, host to guest:

| Field | Meaning |
| --- | --- |
| `pod_uid` | Pod UID. Non-empty, at most 1024 bytes, no control characters. |
| `pod_hostname` | `spec.hostname` when set, otherwise the pod name. DNS hostname, at most 253 characters. |
| `host_entropy_64b` | 64 bytes from `crypto/rand`, fresh for this boot. All-zero is rejected. Not the agent token. |
| `host_wall_clock_unix_nano` | Host wall clock. Accepted range is 2001-01-01 inclusive through 2100-01-01 exclusive. |

Response:

| Field | Meaning |
| --- | --- |
| `reseed_ok` | True only when every guest step succeeded. |
| `guest_rng_sample_hex` | 32 bytes read after the reseed, hex encoded. |
| `reseed_proof_sha256` | `SHA256(host_entropy_64b \|\| guest_sample)`, hex encoded. The raw bytes, not the hex text. |
| `io_platform_uuid` | `IOPlatformUUID` from the guest. |
| `clock_unix_nano` | Guest clock after it was set. |

The seed written to the kernel is `SHA512(host_entropy_64b \|\| pod_uid \|\| mach_continuous_time)` as 8 big-endian bytes for the counter. The host does not need that seed. The proof binds the sample to the entropy the host sent.

## Guest steps

Any error stops the sequence. The guest does not return `reseed_ok` and
does not invent a proof.

1. Validate the request. Nothing touches the OS before this returns.
2. Mix the seed and write all 64 bytes to `/dev/random`.
3. On `runtime.NumCPU()` threads (capped at 256), `LockOSThread` and read 64 bytes with `getentropy`, so each CPU's generator is refreshed.
4. Set the wall clock from `host_wall_clock_unix_nano`.
5. `scutil --set` HostName and ComputerName to the pod hostname. LocalHostName is the same name with dots turned into hyphens, one label, at most 63 characters.
6. Delete `/etc/ssh/ssh_host_*`. A directory with that prefix fails the reseed. `sshd_config` is left in place. An empty glob is success.
7. `SIGHUP` `mDNSResponder`.
8. Read 32 bytes for the sample, on a locked thread. An all-zero sample is an error.
9. Read `IOPlatformUUID` and the clock, and return the proof.

The OS steps sit behind `AdoptPlatform`. Linux tests, SimVZ, and the fake
runtime use an in-memory platform that does not write `/dev/random`, set
the clock, run `scutil`, or delete host keys. `cmd/guest-agent` on darwin
installs the real platform. `cmd/guest-agent` on any other GOOS installs
the in-memory one.

`DARWIN_NODE_GUEST_SOFTWARE_PLATFORM=1` (or `true`) forces the in-memory
platform even on darwin. SimVZ sets it when it execs `darwin-guest-agent`,
because that child is the test Mac, not a guest. The launchd job inside a
VM image does not set it. Default off.

Inside a VM the agent has to be root. Writing `/dev/random`,
`settimeofday`, `scutil`, deleting `/etc/ssh/ssh_host_*`, and signalling
`mDNSResponder` all fail closed without it.

A per-job ephemeral keychain, installed as the default keychain, is not
implemented.

## Host checks

The host draws the entropy. It does not trust the guest to have done the
OS steps, so it checks the response before Materialize or Workload:

- `reseed_ok` is true.
- The sample decodes to exactly 32 bytes, is not empty, and is not all zeros.
- The proof matches `SHA256(host_entropy_64b \|\| sample)` in constant time.
- The guest clock is within 500ms of the host clock at check time.
- `io_platform_uuid` matches a UUID.
- The sample is not equal to any sample this process has already accepted, up to 1024. Past that, the oldest is dropped and may be reused. The set is per engine, not process-global.

Entropy and samples are not written into the pod status text.

On failure the host closes the agent, stops the VM, and marks the pod
Failed. Workload is not called. Materialize is not called. This is the
same permanent failure as a guest that refuses volume placement, not the
dial BackOff loop. Retrying the same image will not produce a reseed it
cannot prove. A ReplicaSet replaces a Failed pod. A dial error before
Adopt is still retried.

The barrier runs on:

- cold start, after dial and before Materialize
- warm-pool handoff, which uses that same path
- every VM restart (liveness or an unexpected stop), before the workload and before a copy-mode rematerialize

A redial of a guest that already passed the barrier does not Adopt again.
That session is the same boot.

## Older guests

Missing capability `adopt`, or an `unknown_method` response, fails the pod.
`DARWIN_NODE_ALLOW_LEGACY_GUEST=1` or `true` skips only that case and logs
a warning. Default off. A bad proof, a zero sample, a duplicate sample, or
any other Adopt error still fails the pod. The variable is read in the
engine. It is not a field on the node config.

## Hardware-pending

These need a Mac Mini and two pods of one image. Linux CI covers the
protocol, the proof, the ordering, and the fail-closed host with SimVZ.

- `/dev/random`, `getentropy`, `arc4random_buf`, and `SecRandomCopyBytes` samples taken from a workload differ across the two pods.
- The guest clock is within 500ms of the host.
- No `/etc/ssh/ssh_host_*` file exists at workload start.
- HostName, LocalHostName, and ComputerName are the pod hostname (LocalHostName with dots turned into hyphens).
