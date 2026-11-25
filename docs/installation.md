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
sudo cp deploy/launchd/io.darwin-node.node.plist /Library/LaunchDaemons/
# edit paths and environment
sudo launchctl load /Library/LaunchDaemons/io.darwin-node.node.plist
```

The guest-side plist is installed *inside images* by `darwin-image inject-agent`.

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
directories at startup. Drain the node before each upgrade so that
restart is not how a running pod is stopped. See [Upgrades](#upgrades).

## Upgrades

Drain the node before each upgrade. A process restart, a crash, or a
host reboot stops every pod VM. The next process deletes the leftover
pod disks and does not boot those pods from a clean image:

- `restartPolicy: Never` is marked `Failed` with reason `NodeRestarted`
  and is not started again. Its restart count is left as it was.
- `restartPolicy: Always` and `OnFailure`, and an empty policy, are
  started again. The container restart count increases by one.
- A pod that had already `Succeeded` or `Failed` keeps that phase and
  is not started again.

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

Precedence: `ApplyEnv` runs after flag parsing, so a set environment variable overrides the matching flag. Defaults apply when neither is set.

Environment variables use the `DARWIN_NODE_` prefix (see `--help`). `ApplyEnv` does not read `DARWIN_NODE_RESERVED_CPU` or `DARWIN_NODE_RESERVED_MEMORY`. Host keep-away stays at the `config.Default` values (2 CPUs and 4Gi). Notable:

| Variable | Default | Meaning |
|---|---|---|
| `DARWIN_NODE_MAX_VMS` | `2` | Must be 1 or 2 |
| `DARWIN_NODE_NETWORK_MODE` | `nat` | `nat` or `bridged` |
| `DARWIN_NODE_BRIDGE_INTERFACE` | empty | Required for bridged |
| `DARWIN_NODE_ALLOW_NAT_WORKLOADS` | `false` | Do not taint NAT nodes |
| `DARWIN_NODE_CACHE_DIR` | `~/Library/Caches/io.darwin-node.node` | Images + overlays |
| `DARWIN_NODE_CACHE_WRITERS` | empty (nobody) | Comma-separated namespaces or `namespace/serviceaccount` allowed to write named caches back. A pod must also set `cache-write.darwin.node/<name>: "true"`. Overrides `--cache-writers` |
| `DARWIN_NODE_GRAPHICS` | `1920x1200@80` | `none` disables Metal |
| `DARWIN_NODE_SHUTDOWN_GRACE` | `60s` | Total drain budget on SIGTERM. launchd `ExitTimeOut` must be larger; see Shutdown behavior |
| `DARWIN_NODE_AGENT_READY_TIMEOUT` | `2m` | Budget for each guest-agent dial and ready check |
| `DARWIN_NODE_SERIAL_CONSOLE` | `false` | Attach break-glass serial ports |
| `DARWIN_NODE_IMAGE_GC` | `true` | Delete unused images and cache stores under disk pressure from the thresholds below |
| `DARWIN_NODE_IMAGE_GC_HIGH_THRESHOLD` | `85` | Percent used at which GC starts |
| `DARWIN_NODE_IMAGE_GC_LOW_THRESHOLD` | `80` | Percent used at which GC stops. Must be below the high threshold |
| `DARWIN_NODE_IMAGE_GC_INTERVAL` | `5m` | How often GC evaluates the cache filesystem |
| `DARWIN_NODE_METRICS_ADDRESS` | `127.0.0.1:9102` | Prometheus bind. `off`, `disabled`, `-`, or empty disables it. Not node_exporter's 9100 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | Enables traces |

Kubernetes connection (same as Virtual Kubelet):

| Variable | Meaning |
|---|---|
| `KUBECONFIG` | kubeconfig path. User should be `system:node:<nodename>` in group `system:nodes` |
| `APISERVER_CERT_LOCATION` / `APISERVER_KEY_LOCATION` | kubelet server cert and key. Required in cluster mode |
| `APISERVER_CA_CERT_LOCATION` | kubelet client CA. Required in cluster mode |
| `KUBELET_PORT` | default `10250` |
| `DARWIN_NODE_LISTEN_ADDRESS` | kubelet HTTP bind address. Default `127.0.0.1`. When set, overrides `--listen-address` |

## Helm (optional skeleton)

`deploy/helm/darwin-node` (chart 0.3.0) installs RBAC for the Mac fleet.
The agent itself still runs on the Mac via launchd, not as a Linux pod.
The chart does not start VMs.

`runtimeClass.enabled` (default `false`) installs a RuntimeClass named
`darwin-vm`. Its scheduling block carries the static tolerations and
`kubernetes.io/os=darwin`. `admission.slotRequest` (default `false`,
Kubernetes 1.32+) adds `darwin.node/vm-slot: "1"` on container 0 when
that runtime class is set. See [scheduling](scheduling.md).

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
