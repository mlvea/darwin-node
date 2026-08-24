package sidecar

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

// Memory is an in-process sidecar runtime for tests and --runtime=fake.
type Memory struct {
	mu   sync.Mutex
	pods map[string][]Status
}

func NewMemory() *Memory {
	return &Memory{pods: map[string][]Status{}}
}

func key(ns, name string) string { return ns + "/" + name }

func (m *Memory) Create(_ context.Context, pod *corev1.Pod, c corev1.Container, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(pod.Namespace, pod.Name)
	st := Status{Name: c.Name, ID: "mem://" + c.Name, State: "running", StartedAt: time.Now()}
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == c.Name {
			st.State = "terminated"
			st.ExitCode = 0
			st.FinishedAt = time.Now()
			break
		}
	}
	m.pods[k] = append(m.pods[k], st)
	return nil
}

func (m *Memory) RemovePod(_ context.Context, ns, name string, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pods, key(ns, name))
	return nil
}

func (m *Memory) Get(_ context.Context, ns, name, container string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.pods[key(ns, name)] {
		if s.Name == container {
			return s, nil
		}
	}
	return Status{}, fmt.Errorf("sidecar %s not found", container)
}

func (m *Memory) List(_ context.Context, ns, name string) ([]Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Status(nil), m.pods[key(ns, name)]...)
	return out, nil
}

func (m *Memory) Logs(_ context.Context, _, _, container string, _ api.ContainerLogOpts) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte("sidecar " + container + " running\n"))), nil
}

func (m *Memory) Exec(_ context.Context, _, _, _ string, _ []string, attach api.AttachIO) error {
	if attach != nil && attach.Stdout() != nil {
		_, _ = attach.Stdout().Write([]byte("ok\n"))
	}
	return nil
}
