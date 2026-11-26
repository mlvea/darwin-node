// Package kubeletconformance is the SimVZ pod-lifecycle conformance suite.
//
// It is test-only. Production code does not import it. The cases run on
// every PR through the provider and engine public APIs against
// pkg/runtime/simvz, and they pin kubelet state-machine behavior that
// already shipped. See docs/kubelet-conformance.md.
package kubeletconformance
