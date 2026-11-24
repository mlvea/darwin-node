package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/node"
	rtfake "github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestConfigureNodeCapacity(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = types.RuntimeFake
	cfg.NodeName = "mac-1"
	cfg.AllowNATWorkloads = true
	cfg.ReservedCPU = resource.MustParse("2")
	cfg.ReservedMemory = resource.MustParse("4Gi")
	cfg.ReservedEphemeral = resource.MustParse("20Gi")
	eng := engine.New(cfg, slots, rtfake.New(), sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 10, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Arch: "arm64", InternalIP: "10.0.0.8", HostID: "h"},
		Cfg:  cfg,
	}, event.Nop{})
	n := &corev1.Node{}
	if err := p.ConfigureNode(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	pods := n.Status.Capacity[corev1.ResourcePods]
	if pods.Value() != 2 {
		t.Fatalf("pods %s", pods.String())
	}
	vm := n.Status.Capacity[corev1.ResourceName(types.ResourceVM)]
	if vm.Value() != 2 {
		t.Fatal("missing darwin.node/vm")
	}
}

func TestProviderLifecycle(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	eng := engine.New(cfg, slots, rtfake.New(), sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30}, Cfg: cfg}, event.Nop{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default", UID: k8stypes.UID("u1")},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
	}
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.GetPodStatus(context.Background(), "default", "macos")
		if err == nil && st.Phase == corev1.PodRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := p.GetPodStatus(context.Background(), "default", "macos")
	t.Fatalf("pod never running: %+v", st)
}

func TestDeleteGraceDefault(t *testing.T) {
	if deleteGrace(nil) != 30 {
		t.Fatal("nil")
	}
	g := int64(7)
	if deleteGrace(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionGracePeriodSeconds: &g}}) != 7 {
		t.Fatal("deletion")
	}
	tg := int64(12)
	if deleteGrace(&corev1.Pod{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: &tg}}) != 12 {
		t.Fatal("spec")
	}
	if deleteGrace(&corev1.Pod{}) != 30 {
		t.Fatal("k8s default")
	}
}

func TestKubeResolverLoadsPullSecretAndConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "default"},
		Data:       map[string]string{"app.plist": "from-api"},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg", Namespace: "default"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"u","password":"p"}}}`),
		},
	}
	r := KubeResolver{Client: k8sfake.NewSimpleClientset(cm, sec)}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default"},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "reg"}},
			Volumes: []corev1.Volume{{
				Name: "cfg",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "cm"},
				}},
			}},
		},
	}
	creds, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatal(err)
	}
	if creds.ConfigMaps["cm"] == nil || creds.ConfigMaps["cm"].Data["app.plist"] != "from-api" {
		t.Fatalf("configmap %+v", creds.ConfigMaps)
	}
	got := creds.Pull.For("ghcr.io/darwin/macos:15")
	if got.Username != "u" || got.Password != "p" {
		t.Fatalf("pull %+v", got)
	}
}

func TestCreatePodResolvesConfigMapAndPullSecret(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	eng := engine.New(cfg, slots, rtfake.New(), sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30}, Cfg: cfg}, event.Nop{})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "default"},
		Data:       map[string]string{"app.plist": "from-api"},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg", Namespace: "default"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"u","password":"p"}}}`),
		},
	}
	p.SetCredentialResolver(KubeResolver{Client: k8sfake.NewSimpleClientset(cm, sec)})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default", UID: k8stypes.UID("u-cm")},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "reg"}},
			Volumes: []corev1.Volume{{
				Name: "cfg",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "cm"},
				}},
			}},
			Containers: []corev1.Container{{
				Name:         "macos",
				Image:        "local/macos:test",
				VolumeMounts: []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/cfg"}},
			}},
		},
	}
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.GetPodStatus(context.Background(), "default", "macos")
		if err == nil && st.Phase == corev1.PodRunning {
			b, err := os.ReadFile(filepath.Join(cfg.CacheDir, "pods", "u-cm", "volumes", "cfg", "app.plist"))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "from-api" {
				t.Fatalf("got %q", b)
			}
			_ = p.DeletePod(context.Background(), pod)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := p.GetPodStatus(context.Background(), "default", "macos")
	t.Fatalf("never running: %+v", st)
}

func TestGetStatsSummaryIsUsageNotCapacity(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = types.RuntimeFake
	cfg.NodeName = "mac-1"
	eng := engine.New(cfg, slots, rtfake.New(), sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 14, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, MemoryUsedFrac: 0.25},
		Cfg:  cfg,
	}, event.Nop{})
	sum, err := p.GetStatsSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cpu := *sum.Node.CPU.UsageNanoCores
	if cpu == 14 {
		t.Fatal("UsageNanoCores must not be logical CPU count")
	}
	mem := *sum.Node.Memory.UsageBytes
	if mem == 32<<30 {
		t.Fatal("UsageBytes must not be capacity")
	}
}

func waitPodRunning(t *testing.T, p *Provider, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.GetPodStatus(context.Background(), ns, name)
		if err == nil && st.Phase == corev1.PodRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := p.GetPodStatus(context.Background(), ns, name)
	t.Fatalf("pod never running: %+v", st)
}

func TestGetStatsSummaryUsesGuestMetrics(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	rt := rtfake.New()
	rt.MetricsFn = func() (guest.MetricsRes, error) {
		return guest.MetricsRes{CPUNanoCores: 123456, MemoryWorkingSet: 999}, nil
	}
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, MemoryUsedFrac: 0.25},
		Cfg:  cfg,
	}, event.Nop{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default", UID: k8stypes.UID("u-stats")},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
	}
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	waitPodRunning(t, p, "default", "macos")
	sum, err := p.GetStatsSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Pods) != 1 {
		t.Fatalf("pods %d", len(sum.Pods))
	}
	ps := sum.Pods[0]
	if ps.CPU == nil || ps.CPU.UsageNanoCores == nil || *ps.CPU.UsageNanoCores != 123456 {
		t.Fatalf("pod cpu %+v", ps.CPU)
	}
	if ps.Memory == nil || ps.Memory.WorkingSetBytes == nil || *ps.Memory.WorkingSetBytes != 999 {
		t.Fatalf("pod mem %+v", ps.Memory)
	}
	if ps.Memory.UsageBytes == nil || *ps.Memory.UsageBytes != 999 {
		t.Fatalf("pod usage %+v", ps.Memory)
	}
	if len(ps.Containers) != 1 || ps.Containers[0].Name != "macos" {
		t.Fatalf("containers %+v", ps.Containers)
	}
	cs := ps.Containers[0]
	if cs.CPU == nil || cs.CPU.UsageNanoCores == nil || *cs.CPU.UsageNanoCores != 123456 {
		t.Fatalf("container cpu %+v", cs.CPU)
	}
	if cs.Memory == nil || cs.Memory.WorkingSetBytes == nil || *cs.Memory.WorkingSetBytes != 999 {
		t.Fatalf("container mem %+v", cs.Memory)
	}
}

func TestGetStatsSummaryOmitsPodUsageWhenMetricsFail(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	rt := rtfake.New()
	rt.MetricsFn = func() (guest.MetricsRes, error) {
		return guest.MetricsRes{}, errors.New("no metrics")
	}
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, MemoryUsedFrac: 0.25},
		Cfg:  cfg,
	}, event.Nop{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default", UID: k8stypes.UID("u-nometrics")},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
	}
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	waitPodRunning(t, p, "default", "macos")
	sum, err := p.GetStatsSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Pods) != 1 {
		t.Fatalf("pods %d", len(sum.Pods))
	}
	ps := sum.Pods[0]
	if ps.CPU != nil || ps.Memory != nil || len(ps.Containers) != 0 {
		t.Fatalf("invented usage: %+v", ps)
	}
}

func TestGetStatsSummaryNodeCPUIndependentOfPods(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	rt := rtfake.New()
	rt.MetricsFn = func() (guest.MetricsRes, error) {
		return guest.MetricsRes{CPUNanoCores: 1_000_000_000, MemoryWorkingSet: 1}, nil
	}
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30},
		Cfg:  cfg,
	}, event.Nop{})
	p.setHostUsage(node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, MemoryUsedFrac: 0.5}, 0)
	for i, name := range []string{"a", "b"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: k8stypes.UID("u-ncpu-" + name)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
		}
		if err := p.CreatePod(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
		waitPodRunning(t, p, "default", name)
		_ = i
	}
	sum, err := p.GetStatsSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Node.CPU == nil || sum.Node.CPU.UsageNanoCores == nil {
		t.Fatal("missing node cpu")
	}
	if *sum.Node.CPU.UsageNanoCores != 0 {
		t.Fatalf("node cpu %d must equal host measurement (0 percent), not include pod CPU", *sum.Node.CPU.UsageNanoCores)
	}
	var podCPU uint64
	for _, ps := range sum.Pods {
		if ps.CPU != nil && ps.CPU.UsageNanoCores != nil {
			podCPU += *ps.CPU.UsageNanoCores
		}
	}
	if podCPU != 2_000_000_000 {
		t.Fatalf("pod cpu sum %d", podCPU)
	}
}

func TestGetStatsSummaryWedgeDoesNotBlock(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	rt := rtfake.New()
	var n atomic.Int32
	rt.MetricsFn = func() (guest.MetricsRes, error) {
		if n.Add(1) == 1 {
			time.Sleep(5 * time.Second)
		}
		return guest.MetricsRes{CPUNanoCores: 7}, nil
	}
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30},
		Cfg:  cfg,
	}, event.Nop{})
	p.setHostUsage(node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30}, 0)
	for _, name := range []string{"fast", "slow"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: k8stypes.UID("u-w-" + name)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
		}
		if err := p.CreatePod(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
		waitPodRunning(t, p, "default", name)
	}
	start := time.Now()
	sum, err := p.GetStatsSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("summary took %s", time.Since(start))
	}
	found := 0
	for _, ps := range sum.Pods {
		if ps.CPU != nil && ps.CPU.UsageNanoCores != nil && *ps.CPU.UsageNanoCores == 7 {
			found++
		}
	}
	if found < 1 {
		t.Fatalf("expected at least one healthy pod sample, pods=%+v", sum.Pods)
	}
}

func TestImagePullSecretIsSelectedByRegistry(t *testing.T) {
	raw := []byte(`{"auths":{
		"aaa.example":{"username":"attacker","password":"stolen"},
		"ghcr.io":{"username":"real","password":"secret"}
	}}`)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg", Namespace: "ns"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
	r := KubeResolver{Client: k8sfake.NewSimpleClientset(sec)}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "reg"}},
			Containers:       []corev1.Container{{Name: "macos", Image: "ghcr.io/team/macos:15"}},
		},
	}
	creds, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatal(err)
	}
	got := creds.Pull.For("ghcr.io/team/macos:15")
	if got.Server != "ghcr.io" || got.Username != "real" || got.Password != "secret" {
		t.Fatalf("credential for ghcr.io/team/macos:15 is server=%q user=%q", got.Server, got.Username)
	}
	if creds.Pull.For("aaa.example/other:1").Username != "attacker" {
		t.Fatal("aaa.example lost its own credential")
	}
	if !creds.Pull.For("quay.io/other:1").Empty() {
		t.Fatal("quay.io received a credential from a different registry")
	}
	base := creds.Pull.For("example.com:5000/team/base:1")
	if !base.Empty() {
		t.Fatalf("delta base registry received %+v", base)
	}
}

func TestLaterImagePullSecretMatchesItsRegistry(t *testing.T) {
	other := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"aaa.example":{"username":"attacker","password":"stolen"}}}`),
		},
	}
	gh := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "ns"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"real","password":"secret"}}}`),
		},
	}
	r := KubeResolver{Client: k8sfake.NewSimpleClientset(other, gh)}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "other"}, {Name: "gh"}},
		},
	}
	creds, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatal(err)
	}
	got := creds.Pull.For("ghcr.io/team/macos:15")
	if got.Username != "real" || got.Password != "secret" {
		t.Fatalf("later pull secret was ignored: %+v", got)
	}
	if creds.Pull.For("aaa.example/x:1").Username != "attacker" {
		t.Fatal("first secret's registry was dropped")
	}
}

func TestGetMetricsResourceReportsPodCPUAndMemory(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	rt := rtfake.New()
	rt.MetricsFn = func() (guest.MetricsRes, error) {
		return guest.MetricsRes{CPUNanoCores: 500_000_000, MemoryWorkingSet: 4096}, nil
	}
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, MemoryUsedFrac: 0.25},
		Cfg:  cfg,
	}, event.Nop{})
	p.setHostUsage(node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, MemoryUsedFrac: 0.25}, 12.5)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default", UID: k8stypes.UID("u-res")},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
	}
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	waitPodRunning(t, p, "default", "macos")

	now := time.Unix(1_700_000_000, 0)
	p.metricsNow = func() time.Time { return now }
	first, err := p.GetMetricsResource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mem := metricValue(t, first, "container_memory_working_set_bytes", map[string]string{
		"namespace": "default", "pod": "macos", "container": "macos",
	})
	if mem != 4096 {
		t.Fatalf("container memory %v, want the guest working set 4096", mem)
	}
	cpu := metricValue(t, first, "container_cpu_usage_seconds_total", map[string]string{
		"namespace": "default", "pod": "macos", "container": "macos",
	})
	if cpu != 0 {
		t.Fatalf("first cpu sample %v, a counter starts at 0", cpu)
	}
	p.metricsNow = func() time.Time { return now.Add(2 * time.Second) }
	second, err := p.GetMetricsResource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cpu = metricValue(t, second, "container_cpu_usage_seconds_total", map[string]string{
		"namespace": "default", "pod": "macos", "container": "macos",
	})
	if cpu < 0.9 || cpu > 1.1 {
		t.Fatalf("cpu counter %v, want about 1 core-second from 0.5 cores over 2s", cpu)
	}
	podMem := metricValue(t, second, "pod_memory_working_set_bytes", map[string]string{
		"namespace": "default", "pod": "macos",
	})
	if podMem != 4096 {
		t.Fatalf("pod memory %v", podMem)
	}
}

func TestGetMetricsResourceUsesAllocationWhenGuestIsSilent(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	rt := rtfake.New()
	rt.MetricsFn = func() (guest.MetricsRes, error) {
		return guest.MetricsRes{}, errors.New("no metrics")
	}
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, MemoryUsedFrac: 0.5},
		Cfg:  cfg,
	}, event.Nop{})
	p.setHostUsage(node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, MemoryUsedFrac: 0.5}, 0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default", UID: k8stypes.UID("u-alloc")},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "macos",
			Image: "local/macos:test",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3"),
				corev1.ResourceMemory: resource.MustParse("6Gi"),
			}},
		}}},
	}
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	waitPodRunning(t, p, "default", "macos")
	now := time.Unix(1_700_000_000, 0)
	p.metricsNow = func() time.Time { return now }
	fams, err := p.GetMetricsResource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mem := metricValue(t, fams, "container_memory_working_set_bytes", map[string]string{
		"namespace": "default", "pod": "macos", "container": "macos", "source": "allocation",
	})
	if mem != float64(6<<30) {
		t.Fatalf("memory %v, want the configured 6Gi allocation", mem)
	}
	p.metricsNow = func() time.Time { return now.Add(time.Second) }
	fams, err = p.GetMetricsResource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cpu := metricValue(t, fams, "container_cpu_usage_seconds_total", map[string]string{
		"namespace": "default", "pod": "macos", "container": "macos", "source": "allocation",
	})
	if cpu < 2.9 || cpu > 3.1 {
		t.Fatalf("cpu %v, want about 3 core-seconds (the configured vCPUs, not a measured sample)", cpu)
	}
}

func metricValue(t *testing.T, fams []*dto.MetricFamily, name string, want map[string]string) float64 {
	t.Helper()
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m, want) {
				continue
			}
			if g := m.GetGauge(); g != nil && m.Gauge != nil {
				return g.GetValue()
			}
			if c := m.GetCounter(); c != nil && m.Counter != nil {
				return c.GetValue()
			}
		}
	}
	t.Fatalf("metric %s with %v not found in %d families", name, want, len(fams))
	return 0
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
