package engine

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

func TestUnresolvedValueFromRejectsThePod(t *testing.T) {
	e := testEngine(t)
	pod := samplePod("env-from", "uid-env-from")
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", pod.Name, 0) })
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{
		Name: "TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "s"},
				Key:                  "token",
			},
		},
	}}
	err := e.Create(context.Background(), pod, Credentials{})
	if err == nil || !strings.Contains(err.Error(), "valueFrom") {
		t.Fatalf("unresolved valueFrom was ignored: %v", err)
	}
}

func TestArgsWithoutCommandRejectsThePod(t *testing.T) {
	e := testEngine(t)
	pod := samplePod("args-only", "uid-args-only")
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", pod.Name, 0) })
	pod.Spec.Containers[0].Args = []string{"--only-args"}
	err := e.Create(context.Background(), pod, Credentials{})
	if err == nil || !strings.Contains(err.Error(), "args") {
		t.Fatalf("args without command were ignored: %v", err)
	}
}

func TestEnvWithoutCommandRejectsThePod(t *testing.T) {
	e := testEngine(t)
	pod := samplePod("env-only", "uid-env-only")
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", pod.Name, 0) })
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "FOO", Value: "bar"}}
	err := e.Create(context.Background(), pod, Credentials{})
	if err == nil || !strings.Contains(err.Error(), "env") {
		t.Fatalf("env without command was ignored: %v", err)
	}
}

func TestCommandArgsAndEnvAreApplied(t *testing.T) {
	leakcheck.Check(t)
	rt := fake.New()
	var mu sync.Mutex
	var got guest.WorkloadReq
	var seen bool
	rt.WorkloadFn = func(ctx context.Context, req guest.WorkloadReq) error {
		mu.Lock()
		got = req
		seen = true
		mu.Unlock()
		return nil
	}
	e := testEngineRuntime(t, rt)
	pod := samplePod("work", "uid-work")
	pod.Spec.Containers[0].Command = []string{"/bin/work"}
	pod.Spec.Containers[0].Args = []string{"--flag"}
	pod.Spec.Containers[0].WorkingDir = "/work"
	pod.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "FOO", Value: "bar"},
		{
			Name:  "TOKEN",
			Value: "resolved",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "s"},
					Key:                  "token",
				},
			},
		},
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", pod.Name, 0) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := seen
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen {
		t.Fatal("container command was not sent to the guest")
	}
	if strings.Join(got.Argv, " ") != "/bin/work --flag" {
		t.Fatalf("argv %q", got.Argv)
	}
	if got.Dir != "/work" || got.Env["FOO"] != "bar" || got.Env["TOKEN"] != "resolved" {
		t.Fatalf("workload %+v", got)
	}
}

func TestVMLogsTimestamps(t *testing.T) {
	e := testEngine(t)
	pod := samplePod("logs", "uid-logs")
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", pod.Name, 0) })
	waitPhase(t, e, "default", pod.Name, corev1.PodRunning)

	rc, err := e.LogsVM(context.Background(), "default", pod.Name, api.ContainerLogOpts{Timestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("fake vm started")) {
		t.Fatalf("logs %q", body)
	}
	if !bytes.Contains(body, []byte("T")) || !bytes.Contains(body, []byte("Z ")) {
		t.Fatalf("kubectl logs --timestamps returned %q", body)
	}
}
