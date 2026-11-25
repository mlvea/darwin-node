// Package simvz is a fault-injecting stand-in for the Virtualization.framework
// runtime. It implements runtime.Runtime with real OS resources so lifecycle
// bugs that a synchronous in-process stub cannot see (vsock and fd leaks,
// goroutines left behind a closed session, VM state races) fail a Linux test.
//
// What is real:
//
//   - An AF_UNIX stream socket carries guest-agent frames. That is the
//     virtio-vsock stand-in. Apple's guest dials the host; the simulator's
//     guest listens and the host dials, because the agent test mode is a
//     listener. The bytes after connect are the same pkg/guest frames.
//   - A SOCK_DGRAM socketpair stands in for VZFileHandleNetworkDeviceAttachment.
//   - When Options.GuestBinary is set, Start execs that darwin-guest-agent.
//     Otherwise the protocol server runs in-process on the same socket.
//
// What is not real: there is no VZVirtualMachine, no hv_apple_isa_vm_quota
// check inside XNU, and no virtio device. The 2-VM cap and the state machine
// are enforced here so a third Start fails the way VZErrorVirtualMachineLimitExceeded
// fails on hardware. pkg/vzcontract runs that behaviour as a shared suite.
// Confirming that the framework agrees is hardware-pending.
package simvz
