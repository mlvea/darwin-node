# Admission

darwin-node runs macOS guests only. One pod is one Virtualization.framework
VM and consumes exactly one `darwin.node/vm-slot`, however many init
containers and sidecars it has. Their CPU and memory requests are
charged to that one VM. This document covers the scheduler taint and
the `CreatePod` checks. It does not change the sidecar runtime that
starts init containers and sidecars today.

Admission runs in the provider `CreatePod` path, after the node-readiness
check and before credentials are resolved or the engine is asked to boot.
A rejected pod is stored `Failed` with a reason and message, and a
Warning event is recorded. No slot is taken and no VM is created.

Checks run in this order: pod profile, macOS platform and disk image,
then the one-VM budget. A pod the engine already has is left alone, so
a repeated create stays idempotent.

## Taint

Every node spec carries

```text
darwin.node/os=macos:NoSchedule
```

including when `--disable-taint` drops `virtual-kubelet.io/provider` and
`darwin.node/macos`. `darwin.node/vm-full` is still not published; a
stale one is removed on the next status update. `darwin.node/nat-only`
is unchanged.

A pod scheduled here has to tolerate the taint:

```yaml
tolerations:
  - key: darwin.node/os
    operator: Equal
    value: macos
    effect: NoSchedule
```

The node label stays `kubernetes.io/os=darwin`. The taint value is
`macos` so it is not the same key as that label.

## Platform

Reason `UnsupportedPlatform`.

The pod is rejected when it asks for an OS other than darwin:

- `spec.os.name` is set and is not `darwin` (`linux` and `windows`).
- `nodeSelector` `kubernetes.io/os` or `beta.kubernetes.io/os` is set and
  is not `darwin`.
- Node affinity, required or preferred, `matchExpressions` or
  `matchFields`, on those keys selects `linux`, excludes `darwin`
  (`NotIn`, `DoesNotExist`), or otherwise cannot match a node labeled
  `kubernetes.io/os=darwin`. An `In` list that contains `linux` is
  rejected even when it also contains `darwin`.

Container 0 is the VM disk. Sidecar and init images are not disks.

A host path (absolute, `./` relative, or an existing directory) must be
a baked image: suffix `.img` or `.ipsw`, or a directory that contains
`disk.img` or `config.json`. That is the existing `LooksLikeVMImage`
check.

A registry reference must parse with `github.com/distribution/reference`
(the parser `pkg/image` already uses for pull credentials). Docker Hub
library images (`docker.io/library/...`, including a bare `busybox:latest`
or `macos:15`) are Linux container images and are rejected. Otherwise
the repository's last path component must be `macos`, `macosvm`,
`macosvz`, or `darwin-node`, or start with `macos-`, `macosvm-`,
`macosvz-`, or `darwin-node-`. Examples that match:
`local/macos:test`, `registry.example.com/macos:15`,
`ghcr.io/acme/macos-sequoia:15`.

A pod that publishes a disk under another name sets

```yaml
metadata:
  annotations:
    darwin.node/disk-image: "true"
```

The annotation does not bypass the OS checks, and a host path still has
to be a baked image. Admission does not pull. The registry media type
(`application/vnd.darwin-node.macosvm`, or the Agoda type accepted on
read) is still checked when the image is pulled.

## One VM budget

Reason `VMBudgetExceeded`.

The effective CPU and memory request is the kubelet pod request:

```text
max(largest non-restartable init, sum(app containers) + sum(restartable init containers)) + pod overhead
```

Restartable init containers are init containers with
`restartPolicy: Always`. They are added to the app sum, not taken as a
separate max. Regular init containers are not summed with each other;
only the largest counts. If a non-restartable init is ordered after a
restartable one, that init's request is added to the restartable
requests already running, which is what kubelet does. CPU and memory
are compared independently.

`darwin.node/vm-slot` (and any other extended resource) is not part of
that sum. The engine still acquires one slot per pod.

The request is compared to what one VM can be given: node allocatable
CPU and memory (host capacity minus `--reserved-cpu` and
`--reserved-memory`). A request above that is rejected here. It is not
left to fail inside Virtualization.framework at boot.

An omitted request counts as zero, which is how kubelet schedules it.
The VM is still created from container 0, and an empty container 0
request still defaults to 2 vCPU and 4 GiB at boot. That default is not
added back into the admission sum.

## Pod profile

Two reasons. `UnsupportedPodField` names a field the guest cannot honor.
`UnsupportedVolume` names a volume this node does not materialize. The
message starts with the field path, for example
`spec.containers[macos].securityContext.privileged must be false` or
`spec.volumes[data].persistentVolumeClaim is not supported`.

The same rejection is stored `Failed`, recorded as a Warning event, and
returned before the engine runs.

### Admitted

| Field | Admitted values |
|---|---|
| `spec.restartPolicy` | unset, `Never`, `OnFailure`, `Always` |
| init container `restartPolicy` | unset, `Always` |
| `spec.hostNetwork`, `spec.hostPID`, `spec.hostIPC` | unset or false |
| `securityContext.privileged` on containers, init containers, and ephemeral containers | unset or false |
| `securityContext.capabilities.add` | unset or empty. `capabilities.drop` is still admitted |
| `seccompProfile` on the pod or on a container | unset, or `type: RuntimeDefault` |
| `securityContext.procMount` | unset or `Default` |
| volume sources | `emptyDir`, `secret`, `configMap`, `downwardAPI`, `hostPath`, `projected` |
| `projected` sources | `serviceAccountToken`, `secret`, `configMap`, `downwardAPI` |

`restartPolicy: Always` restarts the guest process inside the same VM.
The pod still takes one `darwin.node/vm-slot`. `OnFailure` and `Never`
stay on the existing workload-exit path.

`hostPath` is a supported type. The node prefix allowlist still applies
in the engine, before a slot is taken. An empty allowlist
(`--allowed-host-paths` / `DARWIN_NODE_ALLOWED_HOST_PATHS` unset) denies
every hostPath. `/` is never allowed. The allowlist is unchanged.

These fields are left to the existing sidecar isolation check, which
still runs in the engine before a slot is taken: `allowPrivilegeEscalation`,
`runAsNonRoot` without an explicit uid, and `docker.sock` mounts.
`readOnlyRootFilesystem`, `runAsUser`, `runAsGroup`, and dropped
capabilities stay admitted here. `supplementalGroups`, `fsGroup`, and
`sysctls` are unchanged: this profile does not reject them, and the
guest does not apply them.

`pkg/volume` materializes `emptyDir`, `secret`, `configMap`,
`downwardAPI`, `hostPath`, and `projected` limited to the four sources
above. Those stay admitted.

### Rejected

| Field | Reason |
|---|---|
| `spec.hostNetwork`, `spec.hostPID`, or `spec.hostIPC` true | `UnsupportedPodField` |
| `securityContext.privileged: true` on a container, init container, or ephemeral container | `UnsupportedPodField` |
| `securityContext.capabilities.add` non-empty, including on init and ephemeral containers | `UnsupportedPodField` |
| `seccompProfile` at pod or container level set to anything other than `RuntimeDefault`, including `Unconfined`, `Localhost`, and an empty type | `UnsupportedPodField` |
| `appArmorProfile` set at pod or container level, including `RuntimeDefault` | `UnsupportedPodField` |
| `seLinuxOptions` set at pod or container level | `UnsupportedPodField` |
| `securityContext.procMount: Unmasked` | `UnsupportedPodField` |
| `volumeDevices` on a container, init container, or ephemeral container | `UnsupportedVolume` |
| a volume whose source is `persistentVolumeClaim`, `csi`, `ephemeral`, `image`, or any other source outside the admitted set | `UnsupportedVolume` |
| a volume with no source set | `UnsupportedVolume` |
| a `projected` source of `clusterTrustBundle`, `podCertificate`, or an empty source | `UnsupportedVolume` |

`persistentVolumeClaim`, inline `ephemeral` volumes, and CSI are not
materialized (`pkg/volume` returns an unsupported-volume error, and the
volume ADR leaves PVC and CSI for a later phase). `image` volumes are
in the same default. `clusterTrustBundle` and `podCertificate` are not
written by the projected-volume publisher; rejecting them keeps a pod
from looking admitted while those files are skipped.

When several fields are set, the first match is the one recorded:
host namespaces, then the pod security context, then each container,
init container, and ephemeral container (security context, then
`volumeDevices`), then volumes in spec order.

### Cluster policy

`deploy/admission/validatingadmissionpolicy.yaml` is a
`ValidatingAdmissionPolicy` and binding that mirror this profile for
pods which:

- set `runtimeClassName: darwin-vm` (the Helm chart default; a renamed
  class has to be edited into the match condition), or
- set `nodeSelector` `kubernetes.io/os` or `beta.kubernetes.io/os` to
  `darwin`, or
- require that OS with `In` in required node affinity
  (`matchExpressions` or `matchFields`).

`validationActions` is `Deny`. The CEL `reason` is `Invalid` because
that is the API's enum; the message starts with `UnsupportedPodField`
or `UnsupportedVolume`. The Helm chart does not install this policy.
Apply the manifest on the cluster. The policy leaves the hostPath
allowlist to the node. A pod that reaches the node without the
RuntimeClass, the node selector, or the required affinity is still
checked by the provider.
