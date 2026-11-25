# Cache Volumes

Cache volumes persist guest directories across pods with no PVC, no
CSI, and no registry round-trip. Everything rides two primitives the
node already ships: APFS `clonefile` for copy-on-write state transfer,
and virtio-fs for the guest mount.

Consumers see a read-only share. Writing the shared store back is an
explicit opt-in, and only a workload that exits 0 may replace the
previous tree.

## Declaration

Pod annotations, one per cache:

```yaml
metadata:
  annotations:
    cache.darwin.node/derived-data: "/Users/mac/Library/Developer/Xcode/DerivedData"
    cache.darwin.node/spm: "/Users/mac/Library/Caches/org.swift.swiftpm"
    cache-write.darwin.node/derived-data: "true"
```

Rules enforced at admission:

- The annotation name after `cache.darwin.node/` must match
  `[A-Za-z0-9._-]+` and must not end in `.tmp` or `.old`. Those
  suffixes are the crash staging directories next to the store.
- The value must be an absolute guest path below `/`; any `..` element
  is rejected, so a cache can never escape its mount point.
- Two caches may not declare the same guest path.
- `cache-write.darwin.node/<name>` must be `true` or `false`. `true`
  must name a cache declared on the same pod. `false`, or no write
  annotation, is a consumer.

## Who may write

The share is read-only unless both of these are true:

1. The pod sets `cache-write.darwin.node/<name>: "true"`.
2. `--cache-writers` (env `DARWIN_NODE_CACHE_WRITERS`) lists that pod's
   namespace, or `namespace/serviceaccount`. An empty
   `serviceAccountName` is `default`. The flag default is empty, so no
   pod writes a cache until an operator names who may.

A write annotation on a pod outside the list leaves the share
read-only and emits a `CacheReadOnly` warning. One allowlist entry can
cover a whole namespace; `namespace/serviceaccount` covers one account.
The two forms are a comma-separated list. An omitted `--cache-writers`
keeps `DARWIN_NODE_CACHE_WRITERS`. A flag on the command line replaces it.

## Lifecycle

1. **Restore (before VM boot).** For each declared cache the host
   checks `<cache-dir>/cache-store/<namespace>/<name>/`. If present it
   is clonefile'd into `<cache-dir>/pods/<uid>/caches/<name>/`; if not,
   an empty directory is created.
2. **Expose.** The directory becomes a virtio-fs share and the agent
   links it at the annotated guest path. Consumers, and writers that
   are not on the allowlist, get `ReadOnly`. An allowed writer gets a
   read-write share. Guest writes on that share are host writes; there
   is no sync step and no copy-out protocol.
3. **Snapshot (exit 0 only).** When the workload reports exit 0, and
   the pod was not killed and is not already `Failed`, each cache
   named by a write annotation is cloned to `<store>.tmp` while the
   pod directory still exists. The live store is renamed to
   `<store>.old`, then `<store>.tmp` is renamed onto the store.
   `<store>.old` is deleted only after that rename. This runs when the
   guest reports the exit (including `restartPolicy: Always`, before
   the next container starts), again when a `Never` or `OnFailure` pod
   becomes `Succeeded`, and again on `Delete` if that exit 0 is still
   the recorded result. A self-exit therefore commits before the pod
   directory is removed, including when the process restarts and
   reaps that directory. A failed exit, a delete or drain of a pod
   that is still running, and `Close` of a pod that has not exited 0
   leave the previous store in place. A pod whose start failed is
   never snapshotted.

A crash at any point in the rename sequence leaves the last committed
tree at the store path or at `<store>.old`. The next start's
`Recover`, and the next restore, republish that tree and delete
uncommitted `.tmp` clones (a half-written clone is not distinguishable
from a finished one, so `.tmp` is never published).

The store key includes the namespace; cross-namespace sharing is a
deliberate future extension.

## Properties

- Restore and snapshot cost one APFS clone each: O(metadata), not
  O(bytes). A multi-gigabyte DerivedData restores in milliseconds.
  On Linux the same steps walk and copy; the rename is still the
  commit.
- A node crash loses at most the uncommitted output of a run that had
  not yet exited 0. The last successful run stays in the store.
- The snapshot runs on the guest read loop when the exit frame
  arrives. On APFS that clone is metadata. On a copy it holds that
  read until the tree is committed.
- Cache contents live under the node's cache dir with the same
  permissions as pod overlays (0700). A pod that is not an allowed
  writer cannot replace the store. The virtio-fs read-only bit is
  what the guest mount is given; confirming that a booted macOS guest
  cannot write through it still needs Apple hardware.

Example manifest: [examples/pod-cache.yaml](../examples/pod-cache.yaml).
