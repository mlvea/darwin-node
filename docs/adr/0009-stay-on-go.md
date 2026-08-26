# ADR 0009: Stay on Go (do not port to Rust)

- Status: Accepted
- Date: 2026-08-23
- Phase: 1 (language revisit)

## Context

The host agent, guest agent, and image tool are Go. A full Rust port was
considered after Phase 0-1. Rust is a reasonable language for a privileged
macOS daemon. It is the wrong language for *this* architecture.

## Decision

**Do not port.** darwin-node stays Go. A Rust guest-agent can be reconsidered
later as an optional rewrite of `cmd/guest-agent` only, not the node.

## Why this is not a taste call

### 1. Virtual Kubelet is the Kubernetes surface, and it is Go

ADR 0001 chose an in-process Virtual Kubelet provider so we get node
leases, pod sync, kubelet HTTP (exec / logs / attach / stats), webhook
auth, and TLS cert rotation for free. That interface (`nodeutil.Provider`)
is Go. There is no production Rust Virtual Kubelet.

A Rust port would mean one of:

- Reimplement the kubelet-compatible node controller on `kube-rs`
  (leases, SPDY/WebSocket exec, stats summary, token review). That is
  not a port; it is a second kubelet. High semantic risk, years of VK
  edge cases discarded.
- Speak VK's HTTP "web provider" protocol. Extra hop, weaker auth
  story, still not the in-process provider we designed.
- FFI: Rust runtime behind a Go VK. Two languages, two memory
  managers, cgo *and* the objc runtime. Worse than either language
  alone.

The first is the only "real" port. It contradicts the architecture we
just accepted.

### 2. Virtualization.framework production bindings are Go

The runtime we actually call is Code-Hex/vz (Lima, vfkit, Agoda). It
already solved vsock-as-`net.Conn`, start/stop, Mac platform, graphics.
`objc2-virtualization` exists as generated bindings; it is not a
lifecycle library. Porting is re-validating every API we just mapped
(`RequestStop`'s two return values, graphics `int64`, typed socket
devices, unique machine IDs per overlay).

### 3. One module, one protocol, one test binary

The guest protocol is length-prefixed JSON in this repo. Host tests
drive an in-process agent over `net.Pipe`. A Rust rewrite splits that
unless we also rewrite the test harness and keep a Go shim for VK.

## What Rust would buy (not enough)

- Memory safety in the guest agent (nice; the agent is small and
  mostly `exec` + HTTP).
- No GC in the VZ event path (we run at most two VMs; this is not a
  packet switch).
- A smaller signed guest binary (real, not decisive).

## Revisit trigger

Reopen this ADR only if:

- Virtual Kubelet is abandoned and we move to a CRI/kubelet we own, or
- Apple ships a supported Swift/Rust VZ sample that is clearly ahead of
  Code-Hex/vz for macOS guests, or
- We extract the guest agent as its own security-sensitive binary and
  want a memory-safe implementation *without* touching the node.

Until then, language churn delays the actual production work (VZ boot
e2e, vsock listener, ORAS pull, sidecars).
