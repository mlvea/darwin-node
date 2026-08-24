package engine

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestProbeTiming(t *testing.T) {
	initial, period, th := probeTiming(&corev1.Probe{InitialDelaySeconds: 5, PeriodSeconds: 2, FailureThreshold: 4}, 10*time.Second)
	if initial != 5*time.Second || period != 2*time.Second || th != 4 {
		t.Fatalf("%v %v %d", initial, period, th)
	}
	_, period, th = probeTiming(nil, 10*time.Second)
	if period != 10*time.Second || th != 3 {
		t.Fatalf("defaults %v %d", period, th)
	}
}

func TestProbeContextIndependentTimeout(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx, stop := probeContext(parent, 50*time.Millisecond)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("probe context should time out independently")
	}
}

func TestK8sProbeExecHTTP(t *testing.T) {
	req, ok := k8sProbe(&corev1.Probe{
		ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
		TimeoutSeconds: 2,
	})
	if !ok || req.Type != "exec" || req.Argv[0] != "true" {
		t.Fatalf("%+v %v", req, ok)
	}
	req, ok = k8sProbe(&corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: "/ready", Port: intstr.FromInt(8080),
	}}})
	if !ok || req.Type != "httpGet" {
		t.Fatalf("%+v", req)
	}
	_, ok = k8sProbe(nil)
	if ok {
		t.Fatal("nil")
	}
}
