# GPU / Metal

darwin-node exposes the host Apple GPU to each macOS VM through
**paravirtualized graphics**, not PCIe passthrough.

## Model

```
Guest Metal  ->  VZMacGraphicsDevice  ->  host Apple GPU
```

Both VMs and the host UI share one GPU. There is no exclusive assignment,
no MIG, no time-slice API we can set.

## Configuration

Always on by default (required for Metal, simulators, VideoToolbox).

```
--graphics=1920x1200@80          # default
--graphics=2560x1600@110
--graphics=none                  # debug; Metal will not work
```

Per-pod override (annotation):

```
darwin.node/graphics: "1920x1080@72"
```

## Kubernetes surface

Labels on the node:

- `darwin.node/gpu=shared`
- `darwin.node/gpu.model=apple-paravirtual`
- `darwin.node/gpu.metal=true`
- `darwin.node/gpu.passthrough=false`

Resource: `darwin.node/metal` capacity = max VMs (1 or 2). This is a
**scheduling token**. It does not isolate the GPU.

## What to expect in the guest

- `MTLCreateSystemDefaultDevice()` returns a device.
- `supportsFamily` may report an **older** Apple GPU family than the host.
  Modern ML kernels (SIMD-group matrix, 64 KB threadgroups, bfloat16) may
  be disabled by the stack even though the hardware could run them.
- We do **not** spoof family bits.

Validate:

```
kubectl exec macos -c macos -- darwin-guest-agent selftest metal
kubectl exec macos -c macos -- system_profiler SPDisplaysDataType
```

## Workloads

| Workload | Expected |
|---|---|
| Xcode Simulator | Works (needs graphics device) |
| Metal UI / games | Works, shared performance |
| VideoToolbox encode/decode | Generally works |
| ML inference (llama.cpp, Core ML) | Works, often on a slower kernel path |
| CUDA / NVIDIA | Impossible |
