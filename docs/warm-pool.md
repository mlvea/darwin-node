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
   `len(pool) < WarmSlots`, a slot is free, and the boot backoff has
   elapsed, it boots another idle guest from the configured image.
   It also drops a pooled guest that has stopped or failed, including
   when `--warm-slots` is 0.
2. Boot resolves the image through the normal path (local dir, cache,
   or OCI pull), creates a fresh per-slot overlay via APFS clonefile,
   writes an agent token into a control share, starts the VM, dials the
   guest agent over vsock, and waits for Ready before considering the
   entry warm. Adoption later adds no boot work at all.
3. On start, if the primary container mounts nothing and no cache
   annotations are declared, the engine looks for a warm entry whose
   image ref matches exactly and whose vCPU and memory match the pod
   request. The pool boots 2 vCPU and 4 GiB, which is also the default
   request, so a pod that omits resources still matches. A different
   request cold-boots and leaves the warm VM in the pool when another
   slot is free.
4. Before that adoption, a committed pull cache for the image is
   checked with the pod's own pull credentials. The same rule as a
   cache hit: anonymous success means the image is public, and any
   failure, including an unreachable registry, refuses the warm VM.
   The pod then cold-boots. A warm image with no committed cache is
   not sent to the registry.
5. The adopted guest gets a new agent token. The file on the control
   share is replaced (temp file, mode 0600, rename) and the pod dials
   with that token. The warm-boot token is not reused.
6. A real pod that cannot adopt cancels one in-flight warm boot, or
   stops the oldest pooled VM, and then takes the slot. A warm VM
   whose guest has stopped or failed is removed from the pool and its
   slot is released. A failed warm boot waits 1s, doubling to 30s.
   Cancelling a boot, or finding the slot table full, does not start
   that wait. A successful boot resets it.
7. A create is rejected with `VMCapacityExhausted` only when every
   slot is held, the pool is empty, and no warm boot is in flight.
   The pod is not queued.

## Invariants

- Warm VMs hold real slots from the same fail-closed table as pods.
  The node never advertises more than the hardware supports.
- The pool only fills capacity pods do not need right now. A real pod
  preempts a warm boot and evicts a pooled guest before it is rejected.
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

`Engine.Close()` marks the engine draining, cancels the replenisher,
waits up to 30 seconds for any in-flight warm boot, stops every pooled
guest, and then force-stops any pod machine still registered. A boot or
stop that is still running at that deadline is logged and its slot stays
held until the call returns. On the normal path, slots and overlay
directories are released, and a pod's cache volumes are snapshotted.
A second SIGINT/SIGTERM cancels the drain and still runs that stop
before the process exits. A crash skips it.
