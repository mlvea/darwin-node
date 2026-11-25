package engine

import (
	"context"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime/fake"

	corev1 "k8s.io/api/core/v1"
)

// Kubelet does not evict a crash-looping container and does not free its
// resources. RestartPolicy Always and OnFailure keep restarting, so the pod
// stays Running and this engine keeps the VM slot. RestartPolicy Never does
// not restart a non-zero exit: the pod fails and the VM slot is released,
// because the VM is container[0] and a failed pod does not keep it running.
func TestCrashLoopSlotFollowsKubeletRestartPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy corev1.RestartPolicy
		fail   bool
	}{
		{name: "always", policy: corev1.RestartPolicyAlways},
		{name: "on-failure", policy: corev1.RestartPolicyOnFailure},
		{name: "never", policy: corev1.RestartPolicyNever, fail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New()
			sc := newRestartingSidecar()
			sc.exitAlways["log"] = 1
			e := sidecarEngine(t, rt, sc)
			if err := e.Create(context.Background(), hybridPod("hy", "uid-hy-"+tc.name, tc.policy), Credentials{}); err != nil {
				t.Fatal(err)
			}
			if tc.fail {
				p := waitPhase(t, e, "default", "hy", corev1.PodFailed)
				if p.Status.Phase != corev1.PodFailed {
					t.Fatalf("RestartPolicyNever left the pod %s", p.Status.Phase)
				}
				waitVMReaped(t, e, rt, "hy", "uid-hy-"+tc.name)
				if e.slots.Used() != 0 || rt.Running() != 0 {
					t.Fatalf("RestartPolicyNever kept the VM slot after the sidecar failed the pod: slots=%d running=%d", e.slots.Used(), rt.Running())
				}
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			var got *corev1.Pod
			for time.Now().Before(deadline) {
				p, err := e.Get("default", "hy")
				if err == nil && p.Status.Phase == corev1.PodFailed {
					t.Fatalf("crash loop failed the pod: %s", p.Status.Message)
				}
				side := (*corev1.ContainerStatus)(nil)
				if err == nil {
					side = containerStatus(p, "log")
				}
				if err == nil && p.Status.Phase == corev1.PodRunning && side != nil &&
					side.State.Waiting != nil && side.State.Waiting.Reason == "CrashLoopBackOff" &&
					sc.createsOf("log") >= 2 {
					got = p
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if got == nil {
				p, _ := e.Get("default", "hy")
				t.Fatalf("sidecar was not CrashLoopBackOff under %s: %+v creates=%d", tc.policy, containerStatus(p, "log"), sc.createsOf("log"))
			}
			if e.slots.Used() != 1 || rt.Running() != 1 {
				t.Fatalf("policy %s phase=%s slots=%d running=%d, kubelet keeps a crash-looping pod until it is deleted", tc.policy, got.Status.Phase, e.slots.Used(), rt.Running())
			}
		})
	}
}
