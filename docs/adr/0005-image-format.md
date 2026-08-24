# ADR 0005 — Image format evolution

- Status: Accepted
- Date: 2026-08-23
- Phase: 0 / 4

## Context

Agoda stores a macOS VM as an OCI artifact:

- `application/vnd.agoda.macosvz.disk.image.v1` — compressed raw disk
- `application/vnd.agoda.macosvz.aux.image.v1` — auxiliary/NVRAM
- `application/vnd.agoda.macosvz.config.v1+json` — hardware model + machine id

Pull uses ORAS. Decompress (pgzip) on first pull. Integrity: sidecar
`.digest` files of the *uncompressed* content. Runtime clonefile's the disk
and aux into a per-pod overlay so the cached base is immutable.

This is the right shape. Pain points:

- Vendor media types (`vnd.agoda`) block a shared ecosystem.
- No guest-agent layer; SSH keys baked by hand.
- No IPSW→image path in-tree (operators use `macosvm` then a forked ORAS CLI).
- Overlays land in `os.TempDir()`, which macOS may purge.
- Config does not record guest-agent version, OS version, or GPU needs.

## Decision

### Media types

New types, Agoda types accepted on *read* for migration:

| Role | Canonical | Also accepted |
|---|---|---|
| Artifact | `application/vnd.darwin-node.macosvm` | `application/vnd.agoda.macosvz.artifact` |
| Disk | `application/vnd.darwin-node.disk.v1` | `application/vnd.agoda.macosvz.disk.image.v1` |
| Aux | `application/vnd.darwin-node.aux.v1` | `application/vnd.agoda.macosvz.aux.image.v1` |
| Config | `application/vnd.darwin-node.config.v1+json` | `application/vnd.agoda.macosvz.config.v1+json` |

Config v1 fields:

```json
{
  "os": "darwin",
  "osVersion": "15.4",
  "hardwareModelData": "<base64>",
  "machineIdData": "<base64>",
  "guestAgent": { "version": "0.1.0", "vsockPort": 50051 },
  "disk": { "file": "disk.img", "sizeBytes": 137438953472 },
  "aux":  { "file": "aux.img" },
  "graphics": { "width": 1920, "height": 1200, "ppi": 80 }
}
```

`machineIdData` in the *image* is a template. At clone time we generate a
**new** machine identifier so two concurrent VMs never share one (Apple
requirement). Hardware model stays as baked (tied to the IPSW).

### Cache and CoW

- Cache root: `~/Library/Caches/io.darwin-node.node/images/` (configurable).
- Image key: `CacheKey(ref)` — `:` → `--`, `/` → `_`. We do **not** copy
  Agoda’s `convertToPath` (it turns `127.0.0.1:5000/macos:latest` into
  `127.0.0.1/5000/macos/latest`).
- On start: `clonefile` disk + aux into
  `~/Library/Caches/io.darwin-node.node/pods/<uid>/overlay/`. **Not**
  `os.TempDir()`.
- On stop: delete overlay. Never mutate the cached base.
- Digest file next to each blob; mismatch → re-pull. Algorithm: sha256.
- Overlay machine identifier is **minted per pod**. The image’s
  `machineIdData` is a template only — two concurrent clones of the same
  image must not share a machine ID (Apple networking + 2-VM identity).
- Sparse decompress (64 KiB zero skip) is used on pull so a 128 GiB
  mostly-empty disk does not fill APFS with zeroes.

We keep parallel gzip for the large disk layer. Future: zstd (smaller, faster
decompress) as a *new* media type; old images keep working.

### Tooling (`darwin-image`)

```
darwin-image restore --ipsw UniversalMac.ipsw --out ./base
darwin-image inject-agent --image ./base --agent ./bin/darwin-guest-agent
darwin-image pack --image ./base --tag registry.example/macos:15
darwin-image pull  registry.example/macos:15
darwin-image verify --image ./base
```

`restore` wraps `VZMacOSRestoreImage` + `VZMacOSInstaller` (hardware only).
`inject-agent` boots (or mounts) the image, copies the agent, installs the
launchd plist and a first-boot ssh fallback key, shuts down.

### Image pull secrets

Same as kubelet: `kubernetes.io/dockerconfigjson` and `kubernetes.io/dockercfg`
from `imagePullSecrets` and the ServiceAccount. Missing/invalid → CreatePod
fails. Other secret types → Warning event, ignored.

## Consequences

- Operators can keep pulling Agoda-format images during migration.
- Two running VMs from the same image get independent machine IDs and overlays.
- Bake requires Apple Silicon; CI packs/verifies without booting.
