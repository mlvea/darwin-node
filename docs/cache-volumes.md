# Cache Volumes

Cache volumes persist guest directories across pods with no PVC, no
CSI, and no registry round-trip. Everything rides two primitives the
node already ships: APFS `clonefile` for copy-on-write state transfer,
and virtio-fs for read-write guest access.

## Declaration

Pod annotations, one per cache:

```yaml
metadata:
  annotations:
    cache.darwin.node/derived-data: "/Users/mac/Library/Developer/Xcode/DerivedData"
    cache.darwin.node/spm: "/Users/mac/Library/Caches/org.swift.swiftpm"
```

Rules enforced at admission:

- The annotation name after the prefix must match `[A-Za-z0-9._-]+`.
- The value must be an absolute guest path below `/`; any `..` element
  is rejected, so a cache can never escape its mount point.
- Two caches may not declare the same guest path.

## Lifecycle

1. **Restore (before VM boot).** For each declared cache the host
   checks `<cache-dir>/cache-store/<namespace>/<name>/`. If present it
   is clonefile'd into `<cache-dir>/pods/<uid>/caches/<name>/`; if not,
   an empty directory is created.
2. **Expose.** The directory becomes a read-write virtio-fs share and
   the agent links it at the annotated guest path. Guest writes are
   host writes; there is no sync step and no copy-out protocol.
3. **Snapshot (graceful delete only).** Before the pod directory is
   removed, each cache is cloned to `<store>.tmp`, then atomically
   renamed over the previous store entry. A pod whose start failed is
   never snapshotted, so corrupt build state cannot poison the store.

The store key includes the namespace; cross-namespace sharing is a
deliberate future extension.

## Properties

- Restore and snapshot cost one APFS clone each: O(metadata), not
  O(bytes). A multi-gigabyte DerivedData restores in milliseconds.
- Snapshot granularity is per graceful delete. A node crash loses at
  most the last successful run's output, which matches CI semantics:
  the next run rebuilds what it needs.
- Cache contents live under the node's cache dir with the same
  permissions as pod overlays (0700). Nothing crosses a trust boundary
  that virtio-fs shares had not already crossed.

Example manifest: [examples/pod-cache.yaml](../examples/pod-cache.yaml).
