// Package sidecar is the host-side container runtime for hybrid pods.
package sidecar

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

// Runtime manages containers[1..].
type Runtime interface {
	Create(ctx context.Context, pod *corev1.Pod, c corev1.Container, volRoot string) error
	RemovePod(ctx context.Context, namespace, name string, grace int64) error
	Get(ctx context.Context, namespace, name, container string) (Status, error)
	List(ctx context.Context, namespace, name string) ([]Status, error)
	Logs(ctx context.Context, namespace, name, container string, opts api.ContainerLogOpts) (io.ReadCloser, error)
	Exec(ctx context.Context, namespace, name, container string, cmd []string, attach api.AttachIO) error
}

// Status is a sidecar container snapshot.
type Status struct {
	Name       string
	ID         string
	State      string // waiting|running|terminated
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	Error      string
}

// None rejects sidecars. Used when Docker is unavailable.
type None struct{}

func (None) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	return fmt.Errorf("sidecar %q: no host container runtime (docker) configured", c.Name)
}
func (None) RemovePod(context.Context, string, string, int64) error { return nil }
func (None) Get(context.Context, string, string, string) (Status, error) {
	return Status{}, fmt.Errorf("no sidecar runtime")
}
func (None) List(context.Context, string, string) ([]Status, error) { return nil, nil }
func (None) Logs(context.Context, string, string, string, api.ContainerLogOpts) (io.ReadCloser, error) {
	return nil, fmt.Errorf("no sidecar runtime")
}
func (None) Exec(context.Context, string, string, string, []string, api.AttachIO) error {
	return fmt.Errorf("no sidecar runtime")
}
