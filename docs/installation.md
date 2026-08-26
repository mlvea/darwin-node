# Installation

## Requirements

- Apple Silicon Mac (arm64). Host macOS that supports Virtualization.framework.
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

Binaries land in `./bin/`:

- `darwin-node`, host agent
- `darwin-guest-agent`, injected into images
- `darwin-image`, bake / pack / pull

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

Cluster mode refuses a plaintext kubelet HTTP server. Provide a cert
and key (and, in production, a client CA):

```bash
export KUBECONFIG="$HOME/.kube/config"
export APISERVER_CERT_LOCATION="$HOME/.kube/tls.crt"
export APISERVER_KEY_LOCATION="$HOME/.kube/tls.key"
# export APISERVER_CA_CERT_LOCATION="$HOME/.kube/ca.crt"

./bin/darwin-node \
  --nodename="$(hostname | tr '[:upper:]' '[:lower:]')" \
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

Capacity must show `pods: 2` and `darwin.node/vm: 2`.

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
2. Every running pod is deleted through the normal path: preStop hooks,
   the guest shutdown RPC, and cache-volume snapshots all run.
3. Each pod gets its `terminationGracePeriodSeconds`; the total budget is
   `--shutdown-grace-period` (default 60s). Pods that do not finish within
   the budget are left for the next start's crash recovery.
4. The warm pool is paused during drain; freed slots go to departing pods,
   not new warm boots.

A second signal skips the drain and exits immediately (exit code 130).
Setting `--shutdown-grace-period=0` disables draining entirely.

## Configuration

Precedence: flags > environment > config file > defaults.

Environment variables use the `DARWIN_NODE_` prefix (see `--help`). Notable:

| Variable | Default | Meaning |
|---|---|---|
| `DARWIN_NODE_MAX_VMS` | `2` | Must be 1 or 2 |
| `DARWIN_NODE_NETWORK_MODE` | `nat` | `nat` or `bridged` |
| `DARWIN_NODE_BRIDGE_INTERFACE` | empty | Required for bridged |
| `DARWIN_NODE_ALLOW_NAT_WORKLOADS` | `false` | Do not taint NAT nodes |
| `DARWIN_NODE_RESERVED_CPU` | `2` | Host keep-away |
| `DARWIN_NODE_RESERVED_MEMORY` | `4Gi` | Host keep-away |
| `DARWIN_NODE_CACHE_DIR` | `~/Library/Caches/io.darwin-node.node` | Images + overlays |
| `DARWIN_NODE_GRAPHICS` | `1920x1200@80` | `none` disables Metal |
| `DARWIN_NODE_SHUTDOWN_GRACE` | `60s` | Total drain budget on SIGTERM |
| `DARWIN_NODE_SERIAL_CONSOLE` | `false` | Attach break-glass serial ports |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | Enables traces |

Kubernetes connection (same as Virtual Kubelet):

| Variable | Meaning |
|---|---|
| `KUBECONFIG` | kubeconfig path |
| `APISERVER_CERT_LOCATION` / `APISERVER_KEY_LOCATION` | client cert |
| `KUBELET_PORT` | default `10250` |

## Helm (optional skeleton)

`deploy/helm/darwin-node` is a *skeleton* for operators who wrap the Mac
fleet. The agent itself still runs on the Mac via launchd, not as a Linux
pod. The chart deploys RBAC, a ConfigMap of node flags, and (optionally) a
DaemonSet of *documentation* ConfigMaps. It does not magically start VMs
on Linux nodes.

## Homebrew (sketch)

See `deploy/homebrew/darwin-node.rb`. Not published; use as a starting
formula once binaries are notarized.
