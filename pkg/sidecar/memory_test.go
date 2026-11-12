package sidecar

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMemoryCreateReplacesExitedContainer(t *testing.T) {
	m := NewMemory()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"}}
	c := corev1.Container{Name: "log", Image: "busybox"}
	ctx := context.Background()
	if err := m.Create(ctx, pod, c, ""); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.pods["ns/p"][0].State = "terminated"
	m.pods["ns/p"][0].ExitCode = 1
	m.mu.Unlock()
	if err := m.Create(ctx, pod, c, ""); err != nil {
		t.Fatal(err)
	}
	list, err := m.List(ctx, "ns", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != "running" || list[0].Name != "log" || list[0].ExitCode != 0 {
		t.Fatalf("second Create left %+v, want one running container", list)
	}
}
