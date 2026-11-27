# VM identity

A cold boot gets its own Mac machine identifier and its own NVRAM
file. Pods started from one image must not share an ECID: Apple's
Virtualization.framework leaves two concurrent VMs with the same
identifier undefined (the same constraint Tart hit in
[tart#311](https://github.com/cirruslabs/tart/issues/311)). The guest
then reports one `IOPlatformUUID` per VM.

The decision lives in `pkg/vmidentity`. It is plain Go, with no build
tag, so Linux tests can run it. `pkg/runtime/vz` calls it on every
cold boot.

## Cold boot

The image config field `machineIdData` is a template baked at image
build time. `vmidentity.ColdBoot` records that value and does not use
it. A minter supplies a new identifier. The default minter reads
`crypto/rand` and is what the Linux tests use. On the host,
`preparePlatform` calls `vz.NewMacMachineIdentifier` once and passes
that object to the platform config. The image bytes are not decoded.
If the minted value matches the image value, cold boot fails closed
and does not clone NVRAM.

`VMSpec` has no capacity-slot index. The runtime asks for slot 0.
The pod UID is already unique across the two concurrent VMs, and the
aux path includes that UID. Callers that know the slot pass 0 or 1
into `ColdBoot` directly; the helper keeps those paths apart.

## NVRAM

A blank `VZMacAuxiliaryStorage` (Code-Hex/vz
`WithCreatingMacAuxiliaryStorage`) has no iBoot LocalPolicy. The guest
does not boot. Cold boot never creates one.

`ColdBoot` clonefiles the caller's aux onto a pod-private path:

```text
<pod dir>/ephemeral/slot-<0|1>/<pod-uid>/aux.img
```

The pod dir is `cache/pods/<uid>` when the boot disk lives in the
engine's `overlay/` directory, and the disk's directory otherwise.
`clonefile(2)` is used on Darwin. Anywhere else the same call is a
byte copy. An empty file, a symlink, or a missing golden aux is
refused before that copy, so a failed boot does not leave an aux
behind.

The engine already clonefiles the image aux into
`cache/pods/<uid>/overlay/aux.img` and passes that path as
`VMSpec.AuxPath`. The runtime clones that path again and opens the
private file with `vz.NewMacAuxiliaryStorage(path)`, which loads an
existing file. A caller that passes the image's own `aux.img` still
gets a private copy. Writing one pod's aux does not change the file
it was cloned from, or any other pod's aux.

`Machine.Stop` removes the private aux. Removing it twice is a no-op.
The file that was cloned is left in place. The engine still deletes
`cache/pods/<uid>` after stop, which covers a clone `Stop` did not
reach. A `Create` that fails after the clone removes it before
returning.

## Entropy device

Every cold boot attaches `vz.NewVirtioEntropyDeviceConfiguration`.
The platform config is rejected when the identity plan does not ask
for it. Virtualization.framework does not publish a vmgenid, so a
later save/restore keeps the guest's saved RNG until something
reseeds it. The device is the entropy source that reseed can draw
from.

## Restore barrier

`vmidentity.NewReseedBarrier` is the fail-closed gate for handing a
restored guest to a pod. `Adopt` returns `ErrReseedRequired` until
`NoteGuestReseed` is called with at least 64 bytes. That is one write:
shorter writes return `ErrShortReseed`, do not add up, and do not
close a barrier that is already open. A nil barrier fails closed too.

The contract this is built for is XNU `write_random`: the kernel
CSPRNG reseeds synchronously when 64 bytes are written to
`/dev/random`. Processes exec'd after that write see the new entropy.
Processes that were already running keep the entropy they had at
save time.

Cold boot does not arm a barrier. A new boot has a new RNG. A
save/restore snapshot pool is not in this change. When it exists it
should create a barrier after restore and refuse the guest until the
guest has performed that write. The write itself is not implemented
here.

## Hardware-pending

These checks need a Mac Mini and a baked image. Linux CI does not
boot a VM.

- Two cold boots of one image, one after the other, show different
  `IOPlatformUUID` values (`ioreg -rd1 -c IOPlatformExpertDevice`
  inside the guest).
- Two VMs on the two concurrent slots, from that same image, show
  different `IOPlatformUUID` values.
- The guest still reaches the guest agent with the cloned aux. A
  blank aux would not.
