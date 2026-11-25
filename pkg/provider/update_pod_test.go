package provider

import (
	"context"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/node"
	rtfake "github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

func runningProvider(t *testing.T, pod *corev1.Pod) (*Provider, *engine.Engine, *rtfake.Runtime) {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	rt := rtfake.New()
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.8")
	t.Cleanup(func() { _ = eng.Close() })
	p := New(cfg, eng, node.Inventory{Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30}, Cfg: cfg}, event.Nop{})
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
		if err == nil && st.Phase == corev1.PodRunning {
			return p, eng, rt
		}
		time.Sleep(15 * time.Millisecond)
	}
	st, _ := p.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	t.Fatalf("pod never running: %+v", st)
	return nil, nil, nil
}

func neverPod(name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: k8stypes.UID(uid)},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "macos", Image: "local/macos:test"}},
		},
	}
}

// A label or annotation change is what virtual-kubelet sends to UpdatePod.
// Rejecting it makes the pod controller write reason ProviderFailed, and a
// RestartPolicy Never pod then becomes Failed while its VM keeps the slot.
func TestUpdatePodAcceptsLabelAndAnnotation(t *testing.T) {
	p, eng, rt := runningProvider(t, neverPod("job", "uid-meta"))
	starts := rt.Starts()
	running := rt.Running()
	slots := eng.Slots().Used()
	got, err := p.GetPod(context.Background(), "default", "job")
	if err != nil {
		t.Fatal(err)
	}
	updated := got.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	updated.Labels["batch"] = "c"
	updated.Annotations = map[string]string{"note": "metadata-only"}
	if err := p.UpdatePod(context.Background(), updated); err != nil {
		t.Fatalf("label and annotation update: %v", err)
	}
	again := updated.DeepCopy()
	again.Labels["batch"] = "c2"
	if err := p.UpdatePod(context.Background(), again); err != nil {
		t.Fatalf("second metadata update: %v", err)
	}
	got, err = p.GetPod(context.Background(), "default", "job")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase %s reason %q, a metadata update failed the running pod", got.Status.Phase, got.Status.Reason)
	}
	if got.Labels["batch"] != "c2" || got.Annotations["note"] != "metadata-only" {
		t.Fatalf("labels %v annotations %v were not stored", got.Labels, got.Annotations)
	}
	if rt.Starts() != starts || rt.Running() != running || eng.Slots().Used() != slots {
		t.Fatalf("starts %d->%d running %d->%d slots %d->%d, metadata update touched the VM", starts, rt.Starts(), running, rt.Running(), slots, eng.Slots().Used())
	}
}

// Virtual-kubelet rebuilds container env from a map, so the slice order can
// change when only a label changed. That is still a metadata update.
func TestUpdatePodAcceptsEnvReorderWithLabel(t *testing.T) {
	pod := neverPod("job", "uid-env-order")
	// Env is applied with command. Without command the pod is rejected,
	// because the host cannot inject variables into the image entrypoint.
	pod.Spec.Containers[0].Command = []string{"/bin/work"}
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}
	p, _, rt := runningProvider(t, pod)
	starts := rt.Starts()
	got, err := p.GetPod(context.Background(), "default", "job")
	if err != nil {
		t.Fatal(err)
	}
	updated := got.DeepCopy()
	updated.Labels = map[string]string{"batch": "c"}
	updated.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "B", Value: "2"}, {Name: "A", Value: "1"}}
	if err := p.UpdatePod(context.Background(), updated); err != nil {
		t.Fatalf("label update with reordered env: %v", err)
	}
	got, err = p.GetPod(context.Background(), "default", "job")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != corev1.PodRunning || got.Labels["batch"] != "c" {
		t.Fatalf("phase %s labels %v", got.Status.Phase, got.Labels)
	}
	if rt.Starts() != starts {
		t.Fatalf("starts %d -> %d, env order recreated the VM", starts, rt.Starts())
	}
}

func TestUpdatePodRejectsSpecChange(t *testing.T) {
	p, _, rt := runningProvider(t, neverPod("job", "uid-spec"))
	starts := rt.Starts()
	got, err := p.GetPod(context.Background(), "default", "job")
	if err != nil {
		t.Fatal(err)
	}
	updated := got.DeepCopy()
	updated.Labels = map[string]string{"batch": "c"}
	updated.Spec.Containers[0].Image = "local/macos:other"
	err = p.UpdatePod(context.Background(), updated)
	if err == nil || !errdefs.IsInvalidInput(err) {
		t.Fatalf("spec change error = %v, want InvalidInput", err)
	}
	got, err = p.GetPod(context.Background(), "default", "job")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != corev1.PodRunning || got.Labels["batch"] == "c" {
		t.Fatalf("phase %s labels %v, a rejected spec change was applied", got.Status.Phase, got.Labels)
	}
	if got.Spec.Containers[0].Image != "local/macos:test" {
		t.Fatalf("image %s, spec change was stored", got.Spec.Containers[0].Image)
	}
	if rt.Starts() != starts {
		t.Fatalf("starts %d -> %d, a rejected update recreated the VM", starts, rt.Starts())
	}
}
