package engine

import (
	"context"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

// A guest Exit of 0 is the workload finishing. RestartPolicy Never and
// OnFailure must publish Succeeded so a Job counts the run. Always (and an
// empty policy, which Kubernetes treats as Always) starts the VM again and
// leaves the pod Running. A non-zero code follows the same policy: Never
// stays Failed with that code, and OnFailure and Always restart.
func TestWorkloadExitFollowsRestartPolicy(t *testing.T) {
	cases := []struct {
		name    string
		policy  corev1.RestartPolicy
		code    int32
		reason  string
		restart bool
		succeed bool
	}{
		{name: "never-zero", policy: corev1.RestartPolicyNever, code: 0, reason: "Completed", succeed: true},
		{name: "on-failure-zero", policy: corev1.RestartPolicyOnFailure, code: 0, reason: "Completed", succeed: true},
		{name: "always-zero", policy: corev1.RestartPolicyAlways, code: 0, reason: "Completed", restart: true},
		{name: "empty-zero", policy: "", code: 0, reason: "Completed", restart: true},
		{name: "never-nonzero", policy: corev1.RestartPolicyNever, code: 7, reason: "Error"},
		{name: "on-failure-nonzero", policy: corev1.RestartPolicyOnFailure, code: 7, reason: "Error", restart: true},
		{name: "always-nonzero", policy: corev1.RestartPolicyAlways, code: 7, reason: "Error", restart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New()
			e := sidecarEngine(t, rt, sidecar.None{})
			pod := samplePod("job", "uid-workload-"+tc.name)
			pod.Spec.RestartPolicy = tc.policy
			if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
				t.Fatal(err)
			}
			waitPhase(t, e, "default", "job", corev1.PodRunning)
			before := rt.Starts()
			code := tc.code
			rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: tc.reason})
			waitGuestExit(t, e, "default/job", tc.code)
			if err := podMachine(t, e, "default/job").Stop(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			if tc.restart {
				waitWorkloadRestarted(t, e, rt, "default", "job", before)
				if got := rt.Starts(); got != before+1 {
					t.Fatalf("starts=%d, want one restart (%d)", got, before+1)
				}
				time.Sleep(100 * time.Millisecond)
				p, err := e.Get("default", "job")
				if err != nil {
					t.Fatal(err)
				}
				if p.Status.Phase != corev1.PodRunning {
					t.Fatalf("phase %s after exit %d, restart policy %q did not keep the pod up", p.Status.Phase, tc.code, tc.policy)
				}
				if got := rt.Starts(); got != before+1 {
					t.Fatalf("starts=%d, the workload restarted in a loop", got)
				}
				if e.slots.Used() != 1 {
					t.Fatalf("slots=%d, a restarted workload released its VM", e.slots.Used())
				}
				if got := p.Status.ContainerStatuses[0].RestartCount; got != 1 {
					t.Fatalf("restartCount=%d, the new container was not counted once", got)
				}
				return
			}
			if tc.succeed {
				p := waitPhase(t, e, "default", "job", corev1.PodSucceeded)
				term := terminated(t, p)
				if term.ExitCode != 0 || term.Reason != "Completed" {
					t.Fatalf("exit %d reason %q, a zero exit was not Completed", term.ExitCode, term.Reason)
				}
				if rt.Starts() != before {
					t.Fatalf("starts %d -> %d, a finished workload was restarted", before, rt.Starts())
				}
				waitSlotReleased(t, e, rt)
				return
			}
			p := waitPhase(t, e, "default", "job", corev1.PodFailed)
			term := terminated(t, p)
			if term.ExitCode != tc.code || term.Reason != tc.reason {
				t.Fatalf("exit %d reason %q, the guest code was not published", term.ExitCode, term.Reason)
			}
			if rt.Starts() != before {
				t.Fatalf("starts %d -> %d, RestartPolicyNever restarted a failed workload", before, rt.Starts())
			}
			waitSlotReleased(t, e, rt)
		})
	}
}

func waitWorkloadRestarted(t *testing.T, e *Engine, rt *fake.Runtime, ns, name string, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := e.Get(ns, name)
		if err == nil {
			last = p
			if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
				t.Fatalf("phase %s (%s), exit %d ended the pod instead of restarting it (starts=%d)", p.Status.Phase, p.Status.Message, exitOf(p), rt.Starts())
			}
			if p.Status.Phase == corev1.PodRunning && rt.Starts() > before && p.Status.ContainerStatuses[0].RestartCount > 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	phase := corev1.PodPhase("missing")
	if last != nil {
		phase = last.Status.Phase
	}
	t.Fatalf("workload was not restarted: phase %s starts=%d (was %d)", phase, rt.Starts(), before)
}

func exitOf(p *corev1.Pod) int32 {
	if p == nil || len(p.Status.ContainerStatuses) == 0 || p.Status.ContainerStatuses[0].State.Terminated == nil {
		return -1
	}
	return p.Status.ContainerStatuses[0].State.Terminated.ExitCode
}
