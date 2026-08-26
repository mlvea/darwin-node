# Delta Images

Monthly Xcode bumps should not re-download tens of gigabytes. Delta
images turn an image update into a small verifiable patch against a
base the host already has cached.

```
darwin-image delta-create --base macos-15 --target macos-15-xcode16 --out delta/
darwin-image delta-apply  --base macos-15 --delta delta/  --out macos-15-xcode16-host
```

Measured on a smoke fixture: a 48 MB disk with one 4 KB changed region
produced a 3,852 byte patch, and the applied output was byte-identical
to the target.

## Layout

A delta directory contains `delta.json` and one `<name>.patch` per
patched file:

```json
{
  "schema": 1,
  "patches": [
    {
      "name":     "disk.img",
      "baseSha":  "sha256-...",
      "baseSize": 107374182400,
      "destSha":  "sha256-...",
      "destSize": 107583897600
    }
  ]
}
```

`name` is the file inside the image directory the patch targets (the
boot disk is `disk.img`). `baseSha` pins exactly which base this patch
applies to; `destSha` is what the result must hash to.

## Patch format

Each `.patch` file is a flat sequence of records:

```
offset  uint64 big-endian    absolute offset in the target file
length  uint32 big-endian    bytes of data that follow
data    length bytes         exact target content at that offset
```

Records are emitted per 4 MiB chunk (`DefaultDeltaChunkSize`) whenever
the chunk differs from the base. Applying ends with an implicit
truncate to `destSize`, so growth and shrinkage are both handled.

## Apply algorithm

1. Read `delta.json`; refuse unknown schema versions.
2. Load the base image dir and run its optional digest verification.
3. Refuse if the destination already exists.
4. Clone the base tree into `dest.delta-tmp` with APFS clonefile
   (`clonefile.Dir`: every file is a CoW clone).
5. For each patch: re-verify the base file's SHA-256 and size against
   the manifest, apply records with `WriteAt`, truncate to `destSize`,
   fsync, then re-hash and compare against `destSha`.
6. Rename the temporary directory into place. Any failure removes it,
   so a destination is never partially patched.

Correctness never depends on the diff being right: the recorded digest
of the whole target file is checked after every apply.

## OCI pull integration

A registry artifact can carry the patch directly. The image manifest has
its usual config layer (the final image's config.json) plus one disk
patch layer with media type `application/vnd.darwin-node.disk.delta.v1`,
annotated:

| Annotation | Meaning |
|---|---|
| `dev.darwin-node.delta.base-ref` | image ref of the base (pulled on demand through the same cache) |
| `dev.darwin-node.delta.base-disk-sha256` | hex sha256 of the base disk.img this patch applies to |
| `dev.darwin-node.content.uncompressed-digest` | sha256 of the patched disk (the final content) |
| `dev.darwin-node.content.uncompressed-size` | size of the patched disk |

Pulling such a reference:

1. The manifest and blobs are copied into the cache dir as usual.
2. The base image is pulled recursively if not already cached
   (single-flight, same as any other pull).
3. The cached base disk is digest-checked against the pinned value.
4. The patch is applied copy-on-write and the result verified against
   the final digest.
5. Only then does the cache entry become visible; any failure removes
   the partial directory.

Delta artifacts must not also carry an `aux` layer; aux state comes from
the base clone. Ship a full artifact when aux changes.

## Operational model

- The base must be present on every applying host, pinned by content:
  hosts keep their existing cache entry and the delta rides on top.
- Deltas compose by replacement: re-running `delta-create` for the same
  target name updates that entry in `delta.json`. Chained deltas
  (patch-on-patch) are intentionally not supported; bake the new full
  image, cut one delta from whatever base your fleet holds.
- The output of `delta-apply` is an ordinary image dir. It feeds the
  engine's normal resolve path, OCI packing, and verification tools
  without special cases.
