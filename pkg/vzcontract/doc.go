// Package vzcontract is the shared Virtualization.framework contract.
//
// A test passes a factory. The suite drives that runtime through the
// transitions SimVZ and the real framework both have to get right: stop
// notification order, a serial VM queue, fail-fast vsock connect, fd
// behaviour across pause and stop, the two-VM quota, save and restore
// identity, and datagram buffer pressure. Linux CI runs it against SimVZ.
// The darwin/arm64 entrypoint compiles the same calls against Code-Hex/vz
// and skips execution until a Mac Mini is available.
//
// Traces in testdata/vz_traces are derived from those behaviours. They are
// not hardware recordings. docs/vzcontract.md explains how a Mac Mini run
// promotes one.
package vzcontract
