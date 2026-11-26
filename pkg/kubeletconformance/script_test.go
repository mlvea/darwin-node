package kubeletconformance

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/sidecar"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

// scriptSidecar terminates every container as soon as it is created.
// Init containers and hybrid sidecars both use the sidecar runtime, and
// the exit code is chosen by container name. A missing name exits 0.
type scriptSidecar struct {
	mu    sync.Mutex
	exits map[string]int
	order []string
	pods  map[string]map[string]sidecar.Status
}

func newScript(exits map[string]int) *scriptSidecar {
	cp := make(map[string]int, len(exits))
	for k, v := range exits {
		cp[k] = v
	}
	return &scriptSidecar{exits: cp, pods: map[string]map[string]sidecar.Status{}}
}

func (s *scriptSidecar) Create(_ context.Context, pod *corev1.Pod, c corev1.Container, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, c.Name)
	now := time.Now()
	code := s.exits[c.Name]
	st := sidecar.Status{
		Name:       c.Name,
		ID:         c.Name,
		State:      "terminated",
		StartedAt:  now,
		FinishedAt: now,
		ExitCode:   code,
	}
	if code != 0 {
		st.Error = "scripted failure"
	}
	key := pod.Namespace + "/" + pod.Name
	if s.pods[key] == nil {
		s.pods[key] = map[string]sidecar.Status{}
	}
	s.pods[key][c.Name] = st
	return nil
}

func (s *scriptSidecar) RemovePod(_ context.Context, ns, name string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pods, ns+"/"+name)
	return nil
}

func (s *scriptSidecar) Get(_ context.Context, ns, name, container string) (sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.pods[ns+"/"+name][container]
	if !ok {
		return sidecar.Status{}, errNoContainer
	}
	return st, nil
}

func (s *scriptSidecar) List(_ context.Context, ns, name string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sidecar.Status
	for _, st := range s.pods[ns+"/"+name] {
		out = append(out, st)
	}
	return out, nil
}

func (s *scriptSidecar) Logs(context.Context, string, string, string, api.ContainerLogOpts) (io.ReadCloser, error) {
	return nil, errNoContainer
}

func (s *scriptSidecar) Exec(context.Context, string, string, string, []string, api.AttachIO) error {
	return errNoContainer
}

func (s *scriptSidecar) Order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *scriptSidecar) Creates(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, got := range s.order {
		if got == name {
			n++
		}
	}
	return n
}

type scriptErr string

func (e scriptErr) Error() string { return string(e) }

const errNoContainer scriptErr = "script sidecar: container not found"
