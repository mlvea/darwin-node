# ADR 0008 — Remaining technical debt and next steps

- Status: Living
- Date: 2026-08-23
- Phase: 0–1 (updated as later phases land)

## Done in the foundation (Phase 0–1)

- ADRs 0001–0007 accepted.
- Independently buildable Go module with tests for capacity, protocol,
  volumes, image config, fake VM lifecycle, node capacity, hostPort,
  provider Create/status.
- Fail-closed 2-VM slot table.
- Guest agent protocol + TCP server + fake in-process transport.
- VZ wrapper skeleton (graphics, NAT/bridged, vsock, virtio-fs) behind
  `darwin && arm64`.
- Honest GPU labels and `darwin.node/metal` tokens.

## Debt (ordered)

1. **Hardware e2e** of a signed VZ boot against a real IPSW/disk. Code
   path exists (platform, disk, vsock listener, clonefile). Not run here.
2. **IPSW restore** is a CLI stub pointing at `VZMacOSInstaller` (30–60m,
   hardware only).
3. **ORAS pull** copies via `oras.Copy` into a file store then sparsely
   decompresses gzip. Needs a live registry test; CAS/GC still absent.
4. **SSH fallback** exec-only; host keys still ignored (documented).
5. **CRDs / CSI / token rotation / APFS quotas** — Phase 6.
6. **Codesign/notarization CI** and Homebrew publish.

## Recommended next implementation slices

1. Signed `make sign` + local image boot on a Mac (the real Phase 1
   acceptance).
2. Live ORAS pull of a compressed disk layer.
3. Docker sidecar e2e with Colima.
4. Guest-initiated vsock on a running VM (listener is installed at Start).
5. Notarized release bits.
