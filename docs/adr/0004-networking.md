# ADR 0004 — Networking

- Status: Accepted
- Date: 2026-08-23
- Phase: 0 / 4

## Context

Agoda supports NAT (default) and bridged (`VZ_BRIDGE_INTERFACE`). IP discovery
is:

- Bridged: libpcap sniff of the VM's MAC.
- Else: parse `arp -an` for that MAC, 60s timeout, then kill the VM.

There is no hostPort, no CNI, no Service dataplane other than "the VM has an
IP, hope kube-proxy / something can reach it". NAT IPs are host-local and
**cannot** be ClusterIP backends from other nodes.

Apple constraints:

- NAT (`VZNATNetworkDeviceAttachment`) is entitlement-light.
- Bridged (`VZBridgedNetworkDeviceAttachment`) needs `com.apple.vm.networking`
  (Apple-approved) in addition to `com.apple.security.virtualization`.
- No virtio-net multi-queue tuning we control.
- vmnet host objects exist but require the same entitlement family.

## Decision

### Modes

| Mode | When | Pod IP routable from cluster? | Entitlements |
|---|---|---|---|
| `nat` | default | No (host-local only) | virtualization |
| `bridged` | `network.mode=bridged` + interface | Yes, if the bridge is on the cluster L2 | virtualization + vm.networking |
| `disabled` | tests / air-gapped bake | n/a | n/a |

A node that is in `nat` mode is labelled
`darwin.node/network-mode=nat` and tainted
`darwin.node/nat-only=true:NoSchedule` *unless* `--allow-nat-workloads` is set.
This prevents operators from accidentally scheduling Service backends onto
unroutable IPs. CI nodes that only `kubectl exec` stay on NAT.

### IP reporting (authoritative path)

1. Guest Agent `NetInfo` (preferred): the guest lists `en0` IPv4/IPv6 after
   DHCP. This is correct for both NAT and bridged and does not race ARP.
2. vsock is up even without an IP, so the VM can be Running/Ready for exec
   before PodIP is set. `PodIP` is filled when NetInfo succeeds.
3. Fallback: ARP table poll (rewritten, no shell-out loop busy-wait).
4. Last resort (bridged only): pcap sniff, optional dependency.

If no IP is found within `IPTimeout` the pod is **not** killed solely for
that (Agoda killed it). It stays Running with `PodScheduled`/`Initialized`/
`ContainersReady` and `PodReady=False` reason `NoPodIP` when Services need an
IP. Operators can still exec via vsock.

### hostPort

Userspace proxy in `pkg/hostport`: for each `containerPort` with `hostPort`,
bind `0.0.0.0:hostPort` (or `hostIP`) on the host and splice to
`podIP:containerPort`. Requires a PodIP (NAT is fine — host-local). Conflicts
fail the pod with an event. This is *not* kube-proxy; it is the kubelet
hostPort equivalent.

### Services

- **ClusterIP / NodePort / LoadBalancer** work when the PodIP is reachable
  from the proxy node (bridged, or a custom routing hack). Documented.
- **NAT mode + Services from other nodes = does not work.** Fail closed via
  the taint above; do not pretend.

We do not implement CNI. A future `darwin-bridge` plugin could exist; the
runtime already accepts a network attachment interface name.

### MAC addresses

Locally-administered unicast MAC, unique per VM, persisted for the pod UID
so DHCP leases stay stable across recreate-in-place (when we add it). Two VMs
must never share a MAC or Machine Identifier (Apple networking + 2-VM identity).

## Consequences

- Bridged is the production Service path. NAT is the laptop/CI path.
- hostPort works in both modes once PodIP exists.
- We drop the hard "no IP → kill VM" behaviour.
