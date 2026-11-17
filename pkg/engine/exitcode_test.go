package engine

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

func TestPodFailedReportsRuntimeExitCode(t *testing.T) {
	seven := int32(7)
	zero := int32(0)
	cases := []struct {
		name string
		code *int32
		want int32
	}{
		{name: "reported", code: &seven, want: 7},
		{name: "zero", code: &zero, want: 0},
		{name: "omitted", code: nil, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slots, err := capacity.New(2)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Runtime = "fake"
			cfg.CacheDir = t.TempDir()
			cfg.AgentReadyTimeout = 2 * time.Second
			cfg.AllowNATWorkloads = true
			rt := fake.New()
			rt.ExitCode = tc.code
			rt.ExecFn = func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
				if len(req.Argv) > 0 && req.Argv[0] == "false" {
					return 9, nil
				}
				return 0, nil
			}
			e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
			t.Cleanup(func() { _ = e.Close() })
			pod := samplePod("ps", "uid-exit-"+tc.name)
			pod.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
				PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}},
			}
			if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
				t.Fatal(err)
			}
			p := waitPhase(t, e, "default", "ps", corev1.PodFailed)
			if len(p.Status.ContainerStatuses) == 0 || p.Status.ContainerStatuses[0].State.Terminated == nil {
				t.Fatalf("status %+v", p.Status.ContainerStatuses)
			}
			got := p.Status.ContainerStatuses[0].State.Terminated.ExitCode
			if got != tc.want {
				t.Fatalf("exit %d, runtime exit code was not published", got)
			}
		})
	}
}
