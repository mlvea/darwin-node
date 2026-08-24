# Skills & Knowledge Gaps

Living document. Updated at the end of each phase.

What we knew, what we learned from the Agoda baseline and Apple docs, and
what remains hard. This is not a feature list; it is an honesty log for the
next engineer.

## 1. Virtualization.framework

**Learned**

- macOS guests require `VZMacOSBootLoader` + `VZMacPlatformConfiguration`
  (hardware model from the IPSW restore, auxiliary storage, machine identifier).
- Two concurrent macOS VMs with the *same* machine identifier misbehave;
  clonefile of aux is not enough — mint a new machine ID per overlay.
- Graphics device is mandatory for Metal; 1920×1200@80 is the common default.
- `VZVirtioFileSystemDeviceConfiguration` + `MacOSGuestAutomountTag` is the
  only first-class share path; guest path is `/Volumes/My Shared Files`.
- Socket device: host cannot `socket(AF_VSOCK)` (ENODEV). Host uses
  `VZVirtioSocketDevice` FDs. Guest *can* use `AF_VSOCK` to CID 2.
- Bridged attachments need `com.apple.vm.networking`, which Apple must
  approve on the App ID. NAT does not.
- `VZErrorVirtualMachineLimitExceeded` (code 6) is the 3rd-VM failure.
- `requestStop()` is cooperative; force `stop()` is available. There is no
  ACPI-from-host guarantee; guest shutdown is a guest-side action.

**Still hard**

- Undocumented performance cliffs (disk queue depth, virtio-fs vs large
  Xcode trees).
- Auxiliary storage format compatibility across host macOS versions.
- Whether `VZMacOSInstaller` can be driven fully unattended for every IPSW.
- Exact Metal `supportsFamily` the paravirtual GPU reports per host chip.

## 2. Production macOS daemon / agent design

**Learned**

- Host binary must be codesigned with `com.apple.security.virtualization`.
- Bridged: also `com.apple.vm.networking` + provisioning profile.
- launchd: `KeepAlive` + `RunAtLoad` + dedicated log paths. Do not daemonize
  inside the process.
- SIP prevents some hostPath tricks under `/System`. Guest is a separate
  kernel so SIP inside the VM is the guest's problem.
- Notarization is required for distribution outside the build machine;
  ad-hoc sign is enough for local NAT.

**Still hard**

- Shipping a privileged, entitled binary to third parties (Developer ID +
  notarization + vmnet entitlement approval).
- Keychain use from a launchd daemon (user vs system domain).
- Guest agent as root vs a dedicated user with a narrow sudoers — we currently
  run the agent as root for shutdown/mounts.

## 3. Virtual Kubelet provider semantics

**Learned (from Agoda + VK source)**

- `CreatePod` must return quickly; long work (image pull, boot) is async,
  reflected through `GetPodStatus`.
- Returning an error from `CreatePod` after the pod is bound is how we fail
  closed on capacity — VK will surface it; we also write a Failed phase so
  controllers do not churn forever.
- `UpdatePod` is unused in practice; recreate is the model.
- Node conditions must change on heartbeats (`LastHeartbeatTime`) or the
  node goes NotReady.
- `GetStatsSummary` is what metrics-server scrapes, not only `/metrics`.
- Force-deleting the Pod object from the provider (Agoda) hides failures
  from users; we will not do that.

**Still hard**

- Restart policies (`Always` / `OnFailure`) around a 30–60s VM boot.
- ReplicaSet vs Failed pods under a 2-slot node (churn).
- Projected token rotation without a real kubelet volume manager.

## 4. Volumes into a macOS guest

**Learned**

- virtio-fs automount is the right default.
- Silent skip of unknown volumes is a production footgun — we fail admission.
- Secrets-on-host-disk is unavoidable without vsock-only streaming, which
  still buffers.

**Still hard**

- APFS quotas for `emptyDir.sizeLimit`.
- `mount_null` reliability inside the guest across macOS versions.
- PVC/CSI design that does not fight Time Machine / local snapshots.

## 5. Privileged host binary distribution

**Learned**

- Two entitlement files: NAT (`darwin-node.entitlements`) and release
  (`darwin-node.release.entitlements` with vmnet).
- Ad-hoc sign is a one-liner; release sign needs a profile.

**Still hard**

- Apple's vmnet entitlement request process (forums folklore, no SLA).
- Homebrew cask + notarized binary CI.

## 6. Testing

**Learned**

- Unit tests cover capacity, protocol, volumes, status mapping, image
  config, fake runtime. They run on any arch.
- VZ and vsock require Apple Silicon hardware and entitlements.
- GitHub's hosted macOS runners may not allow Virtualization.framework
  nested in whatever they use. Self-hosted Macs are the e2e path.

**Still hard**

- Deterministic e2e without a 40 GB image.
- CI that is not "one Mac Mini under someone's desk".

## 7. macOS networking internals

**Learned**

- ARP polling works for NAT but is racy; guest-reported IP is authoritative.
- pcap on bridged needs additional privacy permissions on newer macOS.
- hostPort is userspace splice; TIME_WAIT and bind conflicts are ours.

**Still hard**

- vmnet APIs vs bridged device identifier stability across sleeps.
- IPv6 SLAAC timing vs Ready.
- Getting kube-proxy on a *Linux* control plane to route to a bridged Mac
  without extra routes.

## 8. Observability across host + guest

**Learned**

- Trace IDs must be passed in the agent Handshake/Exec frames or we lose
  correlation.
- Guest os_log is huge; we stream a predicate, not the whole archive.

**Still hard**

- Clock skew host/guest after restore.
- Mapping Metal GPU time to a Prometheus metric that is not a lie.

## 9. Resource accounting under 2 VMs

**Learned**

- `pods=2` is necessary but not sufficient (CPU overcommit).
- Queueing (Agoda semaphore) is hostile to Kubernetes. Fail closed.
- Reserved host CPU/memory is mandatory on laptops.

**Still hard**

- Fair GPU sharing (we don't try).
- Predicting clonefile disk usage (overlays are sparse until written).

## Phase log

| Phase | Date | What changed in this file |
|---|---|---|
| 0 | 2026-08-23 | Initial findings from Agoda + Apple docs + framework behaviour. |
| 1 | 2026-08-23 | Code-Hex/vz `SocketDevices()` already returns `*VirtioSocketDevice`; `RequestStop` is `(bool, error)`; graphics sizes are `int64`. Virtual Kubelet `nodeutil.Provider` now requires `GetStatsSummary`, `GetMetricsResource`, and `PortForward`. ResourceList index values are not addressable for `Quantity` pointer methods. Agoda OCI cache keys split on every `:`; overlay clones lived in `/tmp`; image `machineIdData` was reused across clonefile overlays (unsafe for two concurrent VMs). Pull is still tag-path, not a CAS — we will not copy that. |
| 2–5 | 2026-08-23 | `VirtioSocketDevice.Listen` is a `net.Listener` (guest-initiated). Guest `AF_VSOCK` + `SockaddrVM` exist on Darwin (`VMADDR_CID_HOST=2`). Code-Hex/vz deletes the listener cgo handle on the first connection — one agent session per VM, which matches our model. |
