# Warm VM Pool

The warm pool keeps pre-booted, agent-ready macOS guests in slots that
pods do not currently need. A pod whose image matches adopts a running
VM instead of paying a full macOS boot.

```
flag            --warm-slots=N        N in 0..2
                --warm-image=REF      default: most recently booted image
env             DARWIN_NODE_WARM_SLOTS / DARWIN_NODE_WARM_IMAGE
slot identity   darwin-warm-<seq>     holds a real pkg/capacity slot
disk layout     <cache-dir>/warm/<seq>/{overlay,control}
events          WarmVMBooted, WarmVMAdopted, WarmVMEvicted
```

## Lifecycle

1. A replenisher goroutine ticks once per second. While
   `len(pool) < WarmSlots` and `slots.Free() > 0`, it boots another idle
   guest from the configured image.
2. Boot resolves the image through the normal path (local dir, cache,
   or OCI pull), creates a fresh per-slot overlay via APFS clonefile,
   writes an agent token into a control share, starts the VM, dials the
   guest agent over vsock, and waits for Ready before considering the
   entry warm. Adoption later adds no boot work at all.
3. On `CreatePod`, if the primary container mounts nothing and no cache
   annotations are declared, the engine looks for a warm entry whose
   image ref matches exactly. A match hands the machine to the pod; the
   pod record keeps the token the guest was booted with.
4. When both slots are held, admission reclaims warm capacity before it
   fails: a matching adoptable entry is taken alive for the pending
   pod; otherwise the oldest entry is stopped and its slot released.
   Only when nothing reclaimable remains does the third pod get
   `VMCapacityExhausted`.

## Invariants

- Warm VMs hold real slots from the same fail-closed table as pods.
  The node never advertises more than the hardware supports.
- The pool only fills capacity pods do not need right now; it never
  causes a rejection that would not have happened anyway.
- Adopted guests are torn down with their pod. They are never returned
  to the pool, because a used CI guest cannot be trusted as warm state.

## Adoption restrictions

Only pods whose `containers[0]` has no volumeMounts and no
`cache.darwin.node/*` annotations can adopt. virtio-fs shares are
configured at VM creation and cannot be attached to a running guest;
a pod that needs shares gets a cold boot even when a matching warm VM
is idle. Init containers and sidecars are unaffected: they run on the
host and are started normally after adoption.

## Shutdown

`Engine.Close()` cancels the replenisher, waits for any in-flight warm
boot to land, stops every pooled guest, releases its slot, and removes
its overlay directory. The node process therefore never leaks a VM that
outlives it beyond what Virtualization.framework itself cleans up.
