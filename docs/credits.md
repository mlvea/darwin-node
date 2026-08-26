# Credits

## Agoda macOS-vz-kubelet

darwin-node exists because
[macOS-vz-kubelet](https://github.com/agoda-com/macOS-vz-kubelet)
(Copyright 2024 Agoda Company Pte. Ltd., Apache License 2.0) showed
that an Apple Silicon Mac can be a Kubernetes node whose pods are
native macOS VMs.

That project established several ideas this tree still believes in:

- Virtual Kubelet as the Kubernetes surface, not a kubelet fork
- Virtualization.framework (via [Code-Hex/vz](https://github.com/Code-Hex/vz)) as the only hypervisor
- Hybrid pods: container 0 is the VM, the rest are host sidecars
- APFS `clonefile` for copy-on-write disk overlays
- An OCI artifact that carries a boot disk, auxiliary storage, and a
  hardware-model blob

darwin-node is a derivative rewrite of that problem, not a fork of
Agoda's tree. We rewrote the node agent, added an in-guest control
plane, and treated Apple's two-VM cap as a scheduling invariant. Where
we still speak Agoda's image media types, it is so operators can
migrate without a flag day. Source files that still follow Agoda
algorithms or formats carry a `Derived from Agoda macOS-vz-kubelet
(Apache-2.0)` comment.

The license obligation, and the thanks, are in [NOTICE](../NOTICE).
Go-module licenses for binaries are in
[THIRD_PARTY_NOTICES](../THIRD_PARTY_NOTICES) (`make licenses`).

## Other software

| Project | License | Role |
|---|---|---|
| [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) | Apache-2.0 | Node / pod API |
| [Code-Hex/vz](https://github.com/Code-Hex/vz) | MIT | Virtualization.framework bindings |
| [ORAS](https://github.com/oras-project/oras-go) | Apache-2.0 | Image pull |
| [gopsutil](https://github.com/shirou/gopsutil) | BSD-3-Clause | Host inventory and usage |
| [Kubernetes](https://github.com/kubernetes/kubernetes) | Apache-2.0 | API types, client-go, kubelet stats |
| [Docker / Moby](https://github.com/moby/moby) | Apache-2.0 | Hybrid sidecar runtime |
| [golang.org/x](https://pkg.go.dev/golang.org/x) | BSD-3-Clause | sys, crypto |

Apple, macOS, Metal, and Virtualization.framework are trademarks of
Apple Inc. This project is not affiliated with Apple.
