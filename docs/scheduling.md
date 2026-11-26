# Scheduling macOS pods

A darwin-node host runs at most two macOS VMs (`--max-vms` may lower
that to 1). The scheduler learns that number from an extended resource,
not from a taint that appears after the node is already full.

## Extended resource

The node status sets both capacity and allocatable:

| Resource | Quantity |
|---|---|
| `darwin.node/vm-slot` | configured maximum (1 or 2) |
| `darwin.node/vm` | the same count |
| `darwin.node/metal` | the same count |
| `pods` | the same count |

`darwin.node/vm` remains so a manifest written before 25 Nov 2026 still
binds. The scheduler accounts each extended resource on its own.
Requesting `darwin.node/vm: 1` does not consume `darwin.node/vm-slot`,
and the reverse is also true. Request `darwin.node/vm-slot: 1` on the
macOS container. The examples do that and still request `darwin.node/vm`
so one pod consumes both names.

The provider is the backstop. A create is rejected, and not queued,
when every slot is held by a running VM, the warm pool is empty, and
no warm boot is in flight. The error wraps `ErrVMCapacityExhausted`,
names `darwin.node/vm-slot`, and is recorded as `VMCapacityExhausted`.
A pod that was already accepted and then loses the race stays
`Pending` with reason `BackOff` and retries the acquire. It does not
hold a slot during that sleep.

Image pull and VM-boot backoff do not hold a slot. The slot is acquired
immediately before the VM is created. A boot failure releases it before
the sleep. A guest-agent dial retry keeps it, because the VM is running.

## Taints that stay

These are static. They do not track how many VMs are running.

- `darwin.node/os=macos:NoSchedule` on every node, including when
  `--disable-taint` drops the provider and `darwin.node/macos` taints
- `darwin.node/macos=true:NoSchedule`, unless `--disable-taint`
- `virtual-kubelet.io/provider=darwin-node:NoSchedule`, unless
  `--disable-taint`
- `darwin.node/nat-only=true:NoSchedule`, unless the process was started
  with `--allow-nat-workloads`

The node label stays `kubernetes.io/os=darwin`. A pod scheduled here
tolerates the OS taint:

```yaml
tolerations:
  - key: darwin.node/os
    operator: Equal
    value: macos
    effect: NoSchedule
```

Which images and resource sums `CreatePod` accepts are in
[admission.md](admission.md).

`darwin.node/vm-full` is not published. A node object that still has it
loses it on the next status update.

`VMCapacity` is still set. It is `True` with reason `SlotsFull` when the
slot table is full, and `False` otherwise. It does not stop the
scheduler. The extended resource does.

## RuntimeClass

The Helm chart can install a RuntimeClass named `darwin-vm`
(`runtimeClass.enabled`, default `false`). Its scheduling block carries
the static tolerations, including `darwin.node/os=macos:NoSchedule`, and
`nodeSelector: kubernetes.io/os=darwin`.
`runtimeClass.tolerateNAT` (default `true`) includes the NAT taint.
The API server's RuntimeClass admission plugin copies that scheduling
onto a pod that sets `runtimeClassName: darwin-vm`.

The RuntimeClass does not add the extended resource. Turn on
`admission.slotRequest` (default `false`, needs Kubernetes 1.32 or
later; this tree tracks 1.35) to install a `MutatingAdmissionPolicy`
and binding. On pod create, when `runtimeClassName` is the configured
class, the policy JSON-patches container 0 so
`darwin.node/vm-slot` is `"1"`. A request that is already set is left
alone. Missing `resources` or `requests` maps are created.

## LimitRange

A namespace LimitRange can default the request without admission-policy
CRDs:

```yaml
apiVersion: v1
kind: LimitRange
metadata:
  name: darwin-vm-slot
spec:
  limits:
    - type: Container
      defaultRequest:
        darwin.node/vm-slot: "1"
```

`defaultRequest` applies to every container in the pod. A hybrid pod
would then request one slot for the VM and one for the sidecar, and
the scheduler would require two slots for one VM. Prefer the
MutatingAdmissionPolicy, which patches container 0 only.
