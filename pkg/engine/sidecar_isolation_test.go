package engine

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/image"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

func TestValidatePodAllowsPrivilegedVMContainer(t *testing.T) {
	priv := true
	pod := samplePod("p", "uid-vm-priv")
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &priv}
	if err := ValidatePod(pod, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRejectsSidecarIsolationBeforeSlot(t *testing.T) {
	priv := true
	cases := []struct {
		name string
		edit func(*corev1.Pod)
		want string
	}{
		{
			name: "privileged",
			edit: func(p *corev1.Pod) {
				p.Spec.Containers[1].SecurityContext = &corev1.SecurityContext{Privileged: &priv}
			},
			want: "privileged",
		},
		{
			name: "hostNetwork",
			edit: func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
			},
			want: "hostNetwork",
		},
		{
			name: "hostPID",
			edit: func(p *corev1.Pod) {
				p.Spec.HostPID = true
			},
			want: "hostPID",
		},
		{
			name: "docker.sock",
			edit: func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
					Name: "sock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}},
				})
				p.Spec.Containers[1].VolumeMounts = []corev1.VolumeMount{{Name: "sock", MountPath: "/var/run/docker.sock"}}
			},
			want: "docker.sock",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := testEngine(t)
			pod := hybridPod("p", "uid-"+tc.name, corev1.RestartPolicyAlways)
			tc.edit(pod)
			err := e.Create(context.Background(), pod, Credentials{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if e.slots.Used() != 0 {
				t.Fatalf("slots %d, rejected pod took a VM slot", e.slots.Used())
			}
		})
	}
}

type recordingSidecar struct {
	sidecar.None
	mu      sync.Mutex
	called  atomic.Int32
	allowed []string
	pull    image.RegistryCreds
}

func (s *recordingSidecar) Create(ctx context.Context, _ *corev1.Pod, _ corev1.Container, _ string) error {
	opts, _ := sidecar.CreateOptionsFrom(ctx)
	s.mu.Lock()
	s.allowed = append([]string(nil), opts.AllowedHostPaths...)
	s.pull = opts.Pull
	s.mu.Unlock()
	s.called.Add(1)
	return nil
}

func TestEnginePassesSidecarCreateOptions(t *testing.T) {
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.AllowedHostPaths = []string{"/data/share"}
	ring, err := image.NewPullCreds([]image.RegistryCreds{
		{Server: "quay.io", Username: "quser", Password: "qpass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc := &recordingSidecar{}
	e := New(cfg, slots, fake.New(), sc, event.Nop{}, "10.0.0.1")
	t.Cleanup(func() { _ = e.Close() })
	pod := hybridPod("side", "uid-opts", corev1.RestartPolicyAlways)
	if err := e.Create(context.Background(), pod, Credentials{Pull: ring}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sc.called.Load() == 0 {
		time.Sleep(15 * time.Millisecond)
	}
	if sc.called.Load() == 0 {
		t.Fatal("sidecar Create was not called")
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.allowed) != 1 || sc.allowed[0] != "/data/share" {
		t.Fatalf("allowlist %v", sc.allowed)
	}
	got := sc.pull.For("quay.io/team/side:1")
	if got.Username != "quser" || got.Password != "qpass" {
		t.Fatalf("pull user %q", got.Username)
	}
	if !sc.pull.For("busybox:1.36").Empty() {
		t.Fatal("docker hub received the quay credential")
	}
}
