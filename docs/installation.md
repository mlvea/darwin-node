# Installation

## Requirements

- Apple Silicon Mac (arm64). Host macOS **13** or newer, and a guest
  image of macOS **13** or newer. The README Requirements section names
  the Virtualization.framework APIs that set that floor.
- A Kubernetes cluster (the Mac joins as a node).
- A macOS VM image in Darwin-Node (or Agoda-compatible) OCI format, **or**
  an IPSW to bake one with `darwin-image`.
- For sidecars: a Docker daemon (Colima, Docker Desktop, or equivalent).
- Codesigning: ad-hoc is enough for NAT. Bridged networking needs Apple-approved
  `com.apple.vm.networking`.

## Build

```bash
make build
make sign          # ad-hoc sign with Virtualization entitlement
```

`make sign` is an ad-hoc signature. It is enough to boot a VM on the
Mac that built it. A binary other people run needs a Developer ID
Application signature and notarization. That is a release step, not a
CI secret: sign with the Developer ID certificate, staple the
notarization ticket, and only then point the Homebrew formula at the
archive. This repository does not store the certificate or the
notary key. The formula in `deploy/homebrew/darwin-node.rb` stays a
sketch until those notarized archives exist.

Binaries land in `./bin/`:

- `darwin-node`, host agent
- `darwin-guest-agent`, injected into images
- `darwin-image`, bake / pack / pull / verify
- `darwin-hwtest`, hardware release gate
- `darwin-soak`, adopt/delete soak harness

To stage a redistributable directory (binaries plus `LICENSE`,
`NOTICE`, and `THIRD_PARTY_NOTICES`):

```bash
make dist          # writes ./dist/
```

Do not publish `bin/` or a GitHub release tarball of binaries without
those three files. MIT and BSD-3-Clause dependencies (Code-Hex/vz,
gopsutil, golang.org/x) require their copyright notices in the
distribution. `make licenses` regenerates `THIRD_PARTY_NOTICES` from
the Go module graph after dependency changes.

## Run (dev)

Cluster mode refuses a plaintext kubelet HTTP server and refuses to start
without a client CA. The kubeconfig user should be `system:node:<nodename>`
in group `system:nodes` (see [security notes](security.md)). The listen
address defaults to `127.0.0.1`. Set it to the address the API server
dials before expecting `kubectl logs` or `kubectl exec`.

```bash
export KUBECONFIG="$HOME/.kube/config"
export APISERVER_CERT_LOCATION="$HOME/.kube/tls.crt"
export APISERVER_KEY_LOCATION="$HOME/.kube/tls.key"
export APISERVER_CA_CERT_LOCATION="$HOME/.kube/ca.crt"

./bin/darwin-node \
  --nodename="$(hostname | tr '[:upper:]' '[:lower:]')" \
  --listen-address="$(ipconfig getifaddr en0)" \
  --allow-nat-workloads \
  --log-level=info
```

Without a cluster, `--runtime=fake --standalone` still prints capacity
(`pods=2`, `vm=2`) and needs no certs.

You should then see:

```bash
kubectl get nodes
# NAME             STATUS   OS-IMAGE          KERNEL-VERSION   CONTAINER-RUNTIME
# madushan-mac     Ready    macOS 15.x        Darwin 24.x      vz://15.x
```

Capacity must show `pods: 2`, `darwin.node/vm-slot: 2`, and `darwin.node/vm: 2`.
`darwin.node/vm-slot` is what macOS pods should request. See
[scheduling](scheduling.md).

## launchd (production)

```bash
sudo mkdir -p /var/log/darwin-node /etc/newsyslog.d
sudo cp deploy/launchd/io.darwin-node.node.plist /Library/LaunchDaemons/
sudo cp deploy/launchd/darwin-node.conf /etc/newsyslog.d/darwin-node.conf
# edit paths and environment
sudo launchctl load /Library/LaunchDaemons/io.darwin-node.node.plist
```

The plist writes stdout and stderr to `/var/log/darwin-node/stdout.log` and
`stderr.log`. `darwin-node.conf` is a newsyslog rule for those two files:
rotate at 1024 KB, keep 5 bzip2 archives, owner `root:admin`, mode `640`.
The `N` flag tells newsyslog not to signal syslogd. `newsyslog` runs on
its own schedule. launchd keeps the log descriptors open, so bytes written
after a rotation stay in the renamed file until the job starts again.

The plist passes `--nodename`, `--log-level`, and `--listen-address`.
Those flags win over the matching environment variables. Change the
listen address by editing `--listen-address`. Log level has no
environment variable.

The guest-side plist is installed *inside images* by `darwin-image inject-agent`.

## Per-VM supervisor

The mode stays off unless the process is started with `--vm-supervisor=on`.
Off keeps each `VZVirtualMachine` inside the agent. On runs each VM in a
second process of the same signed binary. The protocol, threat model, and
upgrade flow are in [vm-supervisor.md](vm-supervisor.md). Flags and
environment variables are in
[Per-VM supervisor configuration](#per-vm-supervisor-configuration).

| Role | How it starts | What it does |
|---|---|---|
| agent | `--role=agent` (default) | Virtual kubelet: image pull, pod watch, `:10250` |
| supervisor | `--role=supervisor --slot=0` or `--slot=1` | One VM and one control socket. It does not pull images, watch the API server, or listen on `:10250` |

`--slot` defaults to `-1` (unset). A supervisor process requires `0` or `1`.

### launchd slot plists

Examples live in `deploy/launchd/io.darwin-node.slot-0.plist` and
`io.darwin-node.slot-1.plist`. They belong in the `gui/<uid>` domain, not
beside the node LaunchDaemon. Each one runs the same signed binary:

```text
/usr/local/bin/darwin-node --role=supervisor --slot=N --runtime=vz --vm-supervisor-run-dir=/var/lib/darwin-node/run
```

`KeepAlive` is `{SuccessfulExit=false}`. launchd restarts a supervisor
that crashes. A supervisor that exits 0, which it does after the VM stops
and after the dead-man switch, stays stopped. The next pod needs
`launchctl kickstart gui/$UID/io.darwin-node.slot-N`, or an agent started
with `--vm-supervisor-spawn=subprocess` (the default), which starts the
slot itself with `Setsid` and does not ask launchd.

`--vm-supervisor-spawn=launchd` is the agent setting that runs
`launchctl kickstart gui/<uid>/io.darwin-node.slot-N` when the socket is
not already listening.

Create `/var/lib/darwin-node/run` mode `0700`, owned by the GUI user,
before the first boot. launchd does not create it. The plist has no
`Sockets` key. The process binds `<run dir>/slot-N.sock` itself. The
agent default, when the flag is on and the run directory is left unset,
is `~/Library/Application Support/io.darwin-node.node/run`. The boot
lease is `<run dir>/slots/slot-N/lease.json` (mode `0600`).

Real LaunchAgent install, `launchctl kickstart`, codesign of the running
binary, and Virtualization.framework behaviour are hardware-pending. CI
checks the plist text. It does not boot launchd or a VM. The pending
framework paths are a file-handle NIC that actually moves frames (the
agent does not bridge those frames to host NAT yet),
`VirtioSocketDevice.Connect` on the Virtualization queue (today's guest
dials the host, so `Connect` completes when the guest listens), and
`Pause` plus `SaveMachineStateToPath` (the save API is macOS 14; on
macOS 13 the framework returns an error, and the minimum stays macOS 13).

### Socket path limit

macOS `sockaddr_un.sun_path` holds 103 bytes plus a trailing NUL. Linux
holds 107. The path that has to fit is `<run dir>/slot-N.sock`, not the
run directory alone. `Serve` rejects a longer path before it creates the
directory, and the error reports the length and the OS limit.

`/var/lib/darwin-node/run/slot-0.sock` is 36 bytes. The default
`$HOME/Library/Application Support/io.darwin-node.node/run/slot-0.sock`
fits when `$HOME` is at most 39 bytes: `/Users/` plus an account name of
32 characters is exactly 103, and one byte more is rejected. A longer
home directory needs a shorter `--vm-supervisor-run-dir` (or
`DARWIN_NODE_VM_SUPERVISOR_RUN_DIR`).

### Slot logs

The slot plists write:

- `/var/log/darwin-node/slot-0.stdout.log` and `slot-0.stderr.log`
- `/var/log/darwin-node/slot-1.stdout.log` and `slot-1.stderr.log`

`deploy/launchd/darwin-node.conf` rotates the node agent's
`/var/log/darwin-node/stdout.log` and `stderr.log` only. The four slot
paths are absent from that file, so newsyslog does not bound them.
`cmd/darwin-node/launchd_plist_test.go` requires the rule count in
`darwin-node.conf` to equal the node plist's two log files, so slot
rules stay out of that file. Add a separate newsyslog configuration when
those slot logs should rotate.

### Current limits of the flag

These are the operator-visible gaps. The feature write-up records the
same list.

- The supervised adapter adopts on first attach and on `Recover`
  through `supervisor.Adopt` (a fresh Connect over `SCM_RIGHTS`).
  `pkg/engine` still does not call that `Recover`, so a restarted
  agent with the flag on leaves a live VM unadopted. Startup still
  deletes unheld pod directories. Supervisor death is `SlotLost` /
  `VMFailed` `supervisor lost` within about 2 seconds and does not wait
  for the dead-man switch. The pod text stays the engine's
  `macOS VM Failed unexpectedly`. `Connect` during `Start` on real
  Virtualization.framework is hardware-pending. See
  [vm-supervisor.md](vm-supervisor.md).
- The guest agent still dials the host. `AttachChannels` dials the
  guest. Those meet when the guest listens.
- The datagram descriptor is handed to the agent and is not bridged to
  host NAT. A guest booted with the flag on has no host network until
  that bridge exists.
- The supervised machine does not expose the serial console.
  `--serial-console` on the in-process runtime is unchanged.
- A recovered machine does not carry the guest-agent token. The lease
  does not store it. The engine has to supply the token from the pod
  before `DialAgent` on a reattached VM.
- macOS 13 cannot save a paused VM. The save API is macOS 14. The
  minimum stays macOS 13, so `PauseAndSaveVM` returns the framework
  error there.

## Shutdown behavior

SIGTERM and SIGINT trigger a graceful drain before the process exits:

1. New pod creates are rejected immediately (the scheduler routes work
   elsewhere).
2. Every running pod is deleted through the normal path: preStop hooks
   and the guest shutdown RPC run. A named cache is written back only
   when that workload already exited 0 and the pod is allowed to write
   it. Drain of a pod that is still running does not replace the store.
3. Each pod gets its `terminationGracePeriodSeconds`; the total budget is
   `--shutdown-grace-period` (default 60s).
4. When that budget ends, or when draining is disabled
   (`--shutdown-grace-period=0`), `Engine.Close` force-stops every pod VM
   still running and releases its slot. That close does not replace a
   cache whose workload has not exited 0. The warm pool is paused
   during the drain and stopped as part of that close.
   A VM call that is still running after 30 seconds does not hold the
   close. The error is logged and that slot stays held until the call
   returns.

A second signal cancels the rest of the drain and still force-stops
every remaining pod VM (`Engine.Close`: slot release; a cache write-back
only if that workload already exited 0) before exiting 130. It does not
wait out `--shutdown-grace-period`.
A crash skips that stop. The next process reclaims leftover pod
directories at startup. That is the in-process runtime. With
`--vm-supervisor=on` the supervisor can keep the VM, and the engine
does not adopt it. Drain the node before each upgrade so that
restart is not how a running pod is stopped. See [Upgrades](#upgrades).

## Upgrades

Drain the node before each upgrade. With `--vm-supervisor` left off
(the default), a process restart, a crash, or a host reboot stops every
pod VM. The next process deletes the leftover pod disks and does not
resume those disks as live machines:

- `restartPolicy: Never` is marked `Failed` with reason `NodeRestarted`
  and is not started again. Its restart count is left as it was.
- `restartPolicy: Always` and `OnFailure`, and an empty policy, are
  started again. The container restart count increases by one.
- A pod that had already `Succeeded` or `Failed` keeps that phase and
  is not started again.

With `--vm-supervisor=on`, the slot supervisor can keep the VM across an
agent restart, and the engine does not adopt it. Drain before the
upgrade in that case too. See [Per-VM supervisor](#per-vm-supervisor).

Draining first lets the pods shut down through the normal path, so a
Job is not failed by the upgrade and an Always pod is not killed mid-run.

Image pull, VM boot, and the guest-agent dial are retried until they
succeed, the error is permanent, or the pod is deleted. The wait starts
at 1s, doubles, and is capped at 30s, with jitter. Image pull and VM
boot do not hold a slot during that wait: the slot is acquired
immediately before the VM is created, and a boot failure releases it
before the sleep. The guest-agent dial keeps the slot, because the VM
is already running. The pod stays `Pending` with reason `BackOff`, so a
ReplicaSet does not create a replacement. Auth denied, an invalid
image, a size or cycle limit, and a delta whose base is itself a delta
fail the pod and release the slot. A pull deadline or a registry read
stall is retried. Each dial attempt uses `--agent-ready-timeout`
(default 2m), which covers a boot of about 48s.

`Stop` polls the VM and returns when it has halted. It does not sit
out the rest of `terminationGracePeriodSeconds` after the guest is
already stopped. launchd's `ExitTimeOut` is the SIGTERM-to-SIGKILL
gap and has to be longer than `--shutdown-grace-period` (default 60s,
`DARWIN_NODE_SHUTDOWN_GRACE`) plus the 30s close and the 5s final
reap. A cache whose workload already exited 0 is committed before the
pod directory is removed. The supplied plist sets `ExitTimeOut` to 120,
which covers the defaults. Raise that integer when you raise the grace
period, or launchd kills the process before that commit runs.

## Configuration

`config.Config` and `config.Supervisor` are separate values. Precedence for both is defaults, then environment variables, then explicit flags. `buildCommand` in `cmd/darwin-node/main.go` calls `Config.MergeEnv` (`pkg/config/env.go`) and `(*Supervisor).MergeEnv` (`pkg/config/supervisor.go`) before the flags are registered, so an omitted flag keeps the environment value and a flag on the command line replaces it. `--help` prints those flag defaults after the environment has been applied. Node defaults come from `config.Default()` in `pkg/config/config.go`. Supervisor defaults come from `config.DefaultSupervisor()`.

A value that `Config.MergeEnv` or `(*Supervisor).MergeEnv` cannot parse is an error before flags are parsed, including before `--help`. For the node config that is a bad integer, duration, Kubernetes quantity, or graphics string, and `DARWIN_NODE_IMAGE_GC` set to anything other than `true`, `1`, `false`, or `0`. For the supervisor config it is a non-integer `DARWIN_NODE_SLOT` or a `DARWIN_NODE_VM_SUPERVISOR_DEADMAN` that is not a Go duration. A value that parses and then fails `Validate` (`pkg/config/validate.go` for the node, `(*Supervisor).Validate` for the supervisor) is an error when the command runs. The flag is already applied, so it can stand in for an environment value that would not validate. Node `Validate` rejects max VMs outside 1 or 2, warm slots outside 0–2, an unknown runtime or network mode, bridged mode with no interface, a negative duration, image GC thresholds out of range, a metrics address that is not `host:port` while metrics are on, a cache-writer entry that is not a namespace or `namespace/serviceaccount`, and any non-empty `DARWIN_NODE_SSH_PASSWORD` or `VZ_SSH_PASSWORD`. Supervisor `Validate` is described with the supervisor table below.

The first table is every variable `Config.MergeEnv` reads. The flag column is the matching `buildCommand` flag. Where that column is empty, there is no flag.

`DARWIN_NODE_ALLOW_NAT_WORKLOADS`, `DARWIN_NODE_SERIAL_CONSOLE`, and `DARWIN_NODE_ENABLE_SSH_FALLBACK` turn on only for `true` or `1`. Any other non-empty value leaves them off and is not a parse error. An empty variable leaves the current value, except `DARWIN_NODE_METRICS_ADDRESS`: setting that variable to empty turns metrics off. `DARWIN_NODE_AGENT_TCP_FALLBACK` is read and ignored.

Host keep-away is subtracted from Allocatable. The defaults are 2 CPUs, 4Gi of memory, and 20Gi of ephemeral storage. Set them with `DARWIN_NODE_RESERVED_CPU`, `DARWIN_NODE_RESERVED_MEMORY`, and `DARWIN_NODE_RESERVED_EPHEMERAL`, or with `--reserved-cpu`, `--reserved-memory`, and `--reserved-ephemeral`. Values are Kubernetes quantities (`500m`, `6Gi`).

| Variable | Default | Flag | Meaning |
|---|---|---|---|
| `DARWIN_NODE_NAME` | lower-cased hostname | `--nodename` | Kubernetes node name |
| `DARWIN_NODE_RUNTIME` | `vz` | `--runtime` | `vz` (hardware) or `fake` (tests/CI) |
| `DARWIN_NODE_MAX_VMS` | `2` | `--max-vms` | Concurrent VMs. Must be 1 or 2 |
| `DARWIN_NODE_RESERVED_CPU` | `2` | `--reserved-cpu` | CPUs withheld from Allocatable |
| `DARWIN_NODE_RESERVED_MEMORY` | `4Gi` | `--reserved-memory` | Memory withheld from Allocatable |
| `DARWIN_NODE_RESERVED_EPHEMERAL` | `20Gi` | `--reserved-ephemeral` | Ephemeral storage withheld from Allocatable |
| `DARWIN_NODE_WARM_SLOTS` | `0` | `--warm-slots` | Pre-booted idle VMs in free slots. Must be 0, 1, or 2 |
| `DARWIN_NODE_WARM_IMAGE` | empty | `--warm-image` | Image pre-booted into warm slots. Empty uses the most recently used image |
| `DARWIN_NODE_NETWORK_MODE` | `nat` | `--network-mode` | `nat` or `bridged`. `disabled` passes `Validate` and the VM still gets a NAT device unless the mode is `bridged` with an interface |
| `DARWIN_NODE_BRIDGE_INTERFACE` | empty | `--bridge-interface` | Required when the mode is bridged. Setting this while the mode is `nat` selects bridged. `VZ_BRIDGE_INTERFACE` is used only when this variable is empty, and it also selects bridged |
| `DARWIN_NODE_ALLOW_NAT_WORKLOADS` | `false` | `--allow-nat-workloads` | `true` or `1` skips the `nat-only` taint. The default still applies that taint |
| `DARWIN_NODE_CACHE_DIR` | `~/Library/Caches/io.darwin-node.node` | `--cache-dir` | Images, overlays, and named-cache stores |
| `DARWIN_NODE_CACHE_WRITERS` | empty (nobody) | `--cache-writers` | Comma-separated namespaces or `namespace/serviceaccount` allowed to write named caches back after exit 0. A pod must also set `cache-write.darwin.node/<name>: "true"`. See [cache-volumes.md](cache-volumes.md) |
| `DARWIN_NODE_ALLOWED_HOST_PATHS` | empty (deny all) | `--allowed-host-paths` | Comma-separated hostPath prefixes guests may use |
| `DARWIN_NODE_GRAPHICS` | `1920x1200@80` |  | `WxH@PPI`. `none` disables Metal |
| `DARWIN_NODE_LISTEN_ADDRESS` | `127.0.0.1` | `--listen-address` | Kubelet HTTP bind address. The flag wins. An empty address binds every interface |
| `KUBELET_PORT` | `10250` |  | Kubelet HTTP port. There is no flag |
| `DARWIN_NODE_SHUTDOWN_GRACE` | `60s` | `--shutdown-grace-period` | Total drain budget on SIGTERM. launchd `ExitTimeOut` must be larger; see Shutdown behavior |
| `DARWIN_NODE_AGENT_READY_TIMEOUT` | `2m` | `--agent-ready-timeout` | Budget for each guest-agent dial and ready check |
| `DARWIN_NODE_SERIAL_CONSOLE` | `false` | `--serial-console` | `true` or `1` attaches break-glass serial ports |
| `DARWIN_NODE_IMAGE_GC` | `true` | `--image-gc` | Delete unused images and cache stores under disk pressure. Only `true`, `1`, `false`, and `0` parse |
| `DARWIN_NODE_IMAGE_GC_HIGH_THRESHOLD` | `85` | `--image-gc-high-threshold` | Percent used at which GC starts. Must be 1–100 while GC is on |
| `DARWIN_NODE_IMAGE_GC_LOW_THRESHOLD` | `80` | `--image-gc-low-threshold` | Percent used at which GC stops. Must be below the high threshold |
| `DARWIN_NODE_IMAGE_GC_INTERVAL` | `5m` | `--image-gc-interval` | How often GC evaluates the cache filesystem |
| `DARWIN_NODE_METRICS_ADDRESS` | `127.0.0.1:9102` | `--metrics-address` | Prometheus bind. `off`, `disabled`, `-`, or empty disables it. Unset keeps the default. Not node_exporter's 9100 |
| `DARWIN_NODE_INSECURE_REGISTRIES` | empty | `--insecure-registry` | Comma-separated registries allowed over plain HTTP. Loopback is always allowed |
| `DARWIN_NODE_MAX_IMAGE_BYTES` | 256GiB | `--max-image-bytes` | Maximum declared or decompressed image size (`256 << 30` bytes) |
| `DARWIN_NODE_MAX_IMAGE_COMPRESSED_BYTES` | 256GiB | `--max-image-compressed-bytes` | Maximum compressed blob size (`256 << 30` bytes) |
| `DARWIN_NODE_IMAGE_DISK_HEADROOM` | 10GiB | `--image-disk-headroom` | Free bytes that must remain on the cache filesystem after an image write (`10 << 30` bytes) |
| `DARWIN_NODE_IMAGE_PULL_TIMEOUT` | `30m` | `--image-pull-timeout` | Deadline for one image pull |
| `DARWIN_NODE_IMAGE_READ_STALL` | `2m` | `--image-read-stall` | Fail a pull when a registry read makes no progress for this long |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty |  | Turns on OpenTelemetry trace export |
| `DARWIN_NODE_ENABLE_SSH_FALLBACK` | `false` |  | `true` or `1` turns on SSH for images with no agent |
| `DARWIN_NODE_SSH_USER` | empty |  | SSH user. `VZ_SSH_USER` is used when this is empty |
| `DARWIN_NODE_SSH_PRIVATE_KEY_PATH` | empty |  | Key path. `VZ_SSH_PRIVATE_KEY_PATH` is used when this is empty |
| `DARWIN_NODE_SSH_PRIVATE_KEY_BASE64` | empty |  | Key bytes. `VZ_SSH_PRIVATE_KEY_BASE64` is used when this is empty |
| `DARWIN_NODE_SSH_KNOWN_HOSTS` | empty |  | known_hosts file for the SSH fallback |
| `DARWIN_NODE_SSH_HOST_KEY` | empty |  | OpenSSH authorized_keys line that pins the guest host key |
| `DARWIN_NODE_SSH_PASSWORD` | empty |  | Rejected at `Validate`, as is `VZ_SSH_PASSWORD`. Password auth is not a fallback |
| `DARWIN_NODE_AGENT_TCP_FALLBACK` | ignored |  | Setting it does nothing. The guest agent is reached only over vsock |

Kubernetes connection (same names Virtual Kubelet reads, plus the listen address above):

| Variable | Default | Meaning |
|---|---|---|
| `KUBECONFIG` | `~/.kube/config` | Kubeconfig path. Same as `--kubeconfig`. User should be `system:node:<nodename>` in group `system:nodes` |
| `APISERVER_CERT_LOCATION` / `APISERVER_KEY_LOCATION` | empty | Kubelet server cert and key. Required in cluster mode. No flag |
| `APISERVER_CA_CERT_LOCATION` | empty | Kubelet client CA. Required in cluster mode. No flag |

### Flags with no environment variable

These are registered in `buildCommand` and are not read from the environment. `--startup-timeout` is the zero duration: `config.Default()` does not set it.

| Flag | Default | Meaning |
|---|---|---|
| `--provider-id` | empty | Provider ID reported to the API server |
| `--disable-taint` | `false` | Drop `virtual-kubelet.io/provider` and `darwin.node/macos`. `darwin.node/os=macos:NoSchedule` stays on the node |
| `--log-level` | `info` | `debug`, `info`, `warn`, or `error`. The stock plist passes this flag |
| `--standalone` | `false` | Run the engine without joining a cluster |
| `--startup-timeout` | `0` | Passed to virtual-kubelet `Node.WaitReady`. `0` adds no deadline: the call waits until the node is Ready, the controller exits, or the process context ends |
| `--init-timeout` | `2m` | Maximum wait for each init container |

### Per-VM supervisor configuration

`(*Supervisor).MergeEnv` reads the variables below. An empty variable leaves the current value. Strings are lowercased before `Validate`. Role must be `agent` or `supervisor`. Mode must be `off` or `on`. Spawn must be `subprocess` or `launchd`. The dead-man duration must be positive. The slot must be `-1`, `0`, or `1`. Role `supervisor` requires slot `0` or `1` and a non-empty run directory. Mode `on` also requires a non-empty run directory. When the home directory cannot be resolved, the default run directory is empty and those two cases fail `Validate`.

The socket path limit for `--vm-supervisor-run-dir` is in [Socket path limit](#socket-path-limit).

| Variable | Default | Flag | Meaning |
|---|---|---|---|
| `DARWIN_NODE_VM_SUPERVISOR` | `off` | `--vm-supervisor` | `on` moves each VM into a slot supervisor. Any other value fails `Validate` |
| `DARWIN_NODE_ROLE` | `agent` | `--role` | `agent` or `supervisor` |
| `DARWIN_NODE_SLOT` | `-1` | `--slot` | Slot `0` or `1`. `-1` means unset. A non-integer is a parse error. Role `supervisor` requires `0` or `1` |
| `DARWIN_NODE_VM_SUPERVISOR_RUN_DIR` | `~/Library/Application Support/io.darwin-node.node/run` | `--vm-supervisor-run-dir` | Directory for `<run dir>/slot-N.sock` and the boot lease. Mode `0700`. On macOS the socket path must be at most 103 bytes |
| `DARWIN_NODE_VM_SUPERVISOR_DEADMAN` | `5m` | `--vm-supervisor-deadman` | After a disconnect while a VM is running, stop it once no agent has been connected for this long. Must be a positive Go duration |
| `DARWIN_NODE_VM_SUPERVISOR_SPAWN` | `subprocess` | `--vm-supervisor-spawn` | `subprocess` starts the slot with `Setsid`. `launchd` runs `launchctl kickstart` |

## Helm (optional skeleton)

`deploy/helm/darwin-node` (chart 0.3.0) installs RBAC for the Mac fleet.
The agent itself still runs on the Mac via launchd, not as a Linux pod.
The chart does not start VMs.

`runtimeClass.enabled` (default `false`) installs a RuntimeClass named
`darwin-vm`. Its scheduling block carries the static tolerations,
including `darwin.node/os=macos:NoSchedule`, and
`kubernetes.io/os=darwin`. The node publishes that taint on every
spec, including with `--disable-taint`. A pod that does not use the
RuntimeClass needs the same toleration or it will not schedule.
`admission.slotRequest` (default `false`,
Kubernetes 1.32+) adds `darwin.node/vm-slot: "1"` on container 0 when
that runtime class is set. `admission.podProfilePolicy.enabled`
(default `false`) renders
`deploy/admission/validatingadmissionpolicy.yaml`. The runtimeClassName
comparison uses `runtimeClass.name`. `failurePolicy` is `Fail`. Image,
budget, and pod-profile rules are in [admission.md](admission.md). See
[scheduling](scheduling.md).

The default `rbac.identity` is `node`. Install the chart for the notes,
and authenticate each Mac as `system:node:<nodename>` in group
`system:nodes`. That identity is what the Node authorizer and
NodeRestriction apply to, including `serviceaccounts/token` create for
pods on the node.

```bash
helm upgrade --install darwin-node deploy/helm/darwin-node \
  --set rbac.identity=serviceaccount \
  --set rbac.namespaces='{default,ci}'
```

`serviceaccount` is the fallback when a node client certificate cannot be
issued. Secret get, ConfigMap get, pod delete, pod status update, and
`serviceaccounts/token` create are Roles in the listed namespaces only.
A pod in any other namespace cannot receive a projected service account
token from this node.

## Homebrew (sketch)

See `deploy/homebrew/darwin-node.rb`. Not published; use as a starting
formula once binaries are notarized.
