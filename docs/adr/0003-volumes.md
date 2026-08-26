# ADR 0003: Volume strategy for a macOS guest

- Status: Accepted
- Date: 2026-08-23
- Phase: 0 / 3

## Context

A Linux kubelet bind-mounts host paths into a container mount namespace.
A macOS Virtualization.framework guest has no such namespace. The available
primitives are:

1. **Virtio-fs shared directories** (`VZVirtioFileSystemDeviceConfiguration` +
   `MacOSGuestAutomountTag`), automounted in the guest at
   `/Volumes/My Shared Files/<share-name>`.
2. **Block devices**, extra virtio-blk disks (good for PVC/CSI later).
3. **Agent-side copy / link**, host materializes files, agent places them at
   the pod's `mountPath`.

Agoda implements (1) for hostPath, emptyDir, and a subset of projected
(configMap, serviceAccountToken, downwardAPI.namespace). Unknown volume types
are silently skipped. Secrets are unsupported. `emptyDir.sizeLimit` is ignored.
Service account tokens are not rotated.

## Decision

Two-stage mount:

**Stage A, host materialization** (`pkg/volume`)

| Volume | Host action |
|---|---|
| `emptyDir` | Directory under `cache/pods/<uid>/volumes/<name>`. `sizeLimit` is tracked (best-effort quota via directory size; hard APFS quota later). `medium: Memory` is *not* tmpfs; documented limitation. |
| `hostPath` | Use the path as-is. Create only if `type` requests it. Never `MkdirAll` a hostPath that the user did not ask to create. |
| `configMap` | Write each key as a file, honor `items`, `defaultMode`, optional. |
| `secret` | Same as configMap, mode 0600 default, directory 0700. |
| `projected` | Union of the above + serviceAccountToken + downwardAPI. Token rotation: rewrite file when the projected volume is refreshed (VK does not rotate; we re-materialize on the provider resync period). |
| `downwardAPI` | Support `metadata.{name,namespace,uid,labels,annotations}` and resource requests/limits of container[0]. |
| PVC / CSI | Not in Phase 1-5. Extension: extra virtio-blk + local CSI using APFS clonefile/snapshots (Phase 6+). |

Unknown volume types **fail pod admission** (CreatePod returns InvalidInput).
No silent skip.

**Stage B, guest placement** (`pkg/guest` Materialize)

Shares are attached as a single `VZMultipleDirectoryShare` keyed by volume
name. After the agent handshakes, the host sends:

```
{ "volumes": [ { "name": "cfg", "guestPath": "/etc/config", "readOnly": true, "mode": "copy" } ] }
```

Modes:

- `link`, symlink `/Volumes/My Shared Files/<name>` -> `guestPath` (emptyDir, hostPath).
- `copy`, recursive copy (secrets, configMaps) so the guest path survives
  share teardown and is not world-readable via the automount folder.
- `bind`, `mount_null` when available (Phase 4 hardening).

The control share `.darwin-node` (token, downward files) is always attached
read-only.

## Why not NFS / 9p / sshfs

Extra moving parts, worse than virtio-fs on Apple Silicon, and they require
guest IP. virtio-fs is in-framework, automounted, and works before networking.

## Path toward PVC

1. Local CSI driver using APFS (`clonefile`, `fs_snapshot`) exposing a
   `darwin-node.local` StorageClass.
2. CSI NodePublish creates a directory or sparse image on the host.
3. Runtime attaches it as virtio-blk (block) or virtio-fs (filesystem).
4. The CRD/controller path can reuse the same attach code.

That work is explicitly out of Phase 1-5; the volume manager's `Attacher`
interface is the hook.

## Consequences

- Secret bytes exist on the host disk for the life of the pod (mitigated by
  0700 dirs under the node cache, deleted on pod teardown).
- virtio-fs does not provide Linux bind-mount semantics (no mount
  propagation). Documented.
- Windows-style drive letters are irrelevant; we always use POSIX paths.
