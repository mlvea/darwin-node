package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/node"
	rtfake "github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

type warnLog struct {
	mu       sync.Mutex
	reasons  []string
	messages []string
}

func (w *warnLog) Normal(context.Context, string, string) {}

func (w *warnLog) Warn(_ context.Context, reason, message string) {
	w.mu.Lock()
	w.reasons = append(w.reasons, reason)
	w.messages = append(w.messages, message)
	w.mu.Unlock()
}

func (w *warnLog) saw(reason string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, got := range w.reasons {
		if got == reason {
			return true
		}
	}
	return false
}

type admitEnv struct {
	p     *Provider
	rt    *rtfake.Runtime
	slots *capacity.Slots
	warn  *warnLog
	kube  *k8sfake.Clientset
}

func newAdmitEnv(t *testing.T, sc sidecar.Runtime) *admitEnv {
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
	if sc == nil {
		sc = sidecar.None{}
	}
	rt := rtfake.New()
	warn := &warnLog{}
	eng := engine.New(cfg, slots, rt, sc, warn, "10.0.0.8")
	t.Cleanup(func() { _ = eng.Close() })
	kube := k8sfake.NewSimpleClientset()
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, Arch: "arm64"},
		Cfg:  cfg,
	}, warn)
	p.SetCredentialResolver(KubeResolver{Client: kube})
	return &admitEnv{p: p, rt: rt, slots: slots, warn: warn, kube: kube}
}

func reqs(cpu, mem, slot string) corev1.ResourceRequirements {
	out := corev1.ResourceList{}
	if cpu != "" {
		out[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		out[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	if slot != "" {
		out[corev1.ResourceName(types.ResourceVMSlot)] = resource.MustParse(slot)
	}
	return corev1.ResourceRequirements{Requests: out}
}

func testPod(name string, spec corev1.PodSpec) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: k8stypes.UID("uid-" + name)},
		Spec:       spec,
	}
}

func macosContainer(cpu, mem, slot string) corev1.Container {
	return corev1.Container{Name: "macos", Image: "local/macos:test", Resources: reqs(cpu, mem, slot)}
}

func assertRejected(t *testing.T, env *admitEnv, pod *corev1.Pod, reason string) {
	t.Helper()
	before := env.rt.Starts()
	err := env.p.CreatePod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("CreatePod err = %v, want %s", err, reason)
	}
	st, serr := env.p.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if serr != nil {
		t.Fatal(serr)
	}
	if st.Phase != corev1.PodFailed || st.Reason != reason || st.Message == "" {
		t.Fatalf("status phase=%s reason=%q message=%q", st.Phase, st.Reason, st.Message)
	}
	if env.slots.Used() != 0 {
		t.Fatalf("slots used %d, rejected pod took a slot", env.slots.Used())
	}
	if env.rt.Starts() != before {
		t.Fatalf("runtime starts %d -> %d, rejected pod booted a VM", before, env.rt.Starts())
	}
	if !env.warn.saw(reason) {
		t.Fatalf("events %+v, want reason %s", env.warn.reasons, reason)
	}
}

func TestAdmissionRejectsNonMacOSWorkloads(t *testing.T) {
	env := newAdmitEnv(t, sidecar.None{})
	linuxSel := testPod("linux-sel", corev1.PodSpec{
		NodeSelector: map[string]string{corev1.LabelOSStable: "linux"},
		Containers:   []corev1.Container{macosContainer("", "", "")},
	})
	assertRejected(t, env, linuxSel, "UnsupportedPlatform")

	affinity := testPod("linux-aff", corev1.PodSpec{
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"},
					}},
				}},
			},
		}},
		Containers: []corev1.Container{macosContainer("", "", "")},
	})
	assertRejected(t, env, affinity, "UnsupportedPlatform")

	preferred := testPod("linux-pref", corev1.PodSpec{
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
				Weight: 1,
				Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "beta.kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux", "darwin"},
				}}},
			}},
		}},
		Containers: []corev1.Container{macosContainer("", "", "")},
	})
	assertRejected(t, env, preferred, "UnsupportedPlatform")

	osName := corev1.Linux
	specOS := testPod("linux-os", corev1.PodSpec{
		OS:         &corev1.PodOS{Name: osName},
		Containers: []corev1.Container{macosContainer("", "", "")},
	})
	assertRejected(t, env, specOS, "UnsupportedPlatform")

	busy := testPod("busybox", corev1.PodSpec{
		Containers: []corev1.Container{{Name: "app", Image: "busybox:latest"}},
	})
	assertRejected(t, env, busy, "UnsupportedPlatform")

	evs, err := env.kube.CoreV1().Events("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs.Items {
		if ev.Reason == "UnsupportedPlatform" && ev.Type == corev1.EventTypeWarning && ev.InvolvedObject.Kind == "Pod" {
			found = true
		}
	}
	if !found {
		t.Fatalf("kubernetes events %+v, want Warning UnsupportedPlatform", evs.Items)
	}
}

func TestAdmissionRejectsOverVMBudget(t *testing.T) {
	env := newAdmitEnv(t, sidecar.None{})
	// Allocatable is host 8 CPU / 16Gi minus the default reservation (2 CPU / 4Gi).
	both := testPod("sum", corev1.PodSpec{
		Containers: []corev1.Container{
			macosContainer("4", "1Gi", "1"),
			{Name: "log", Image: "busybox:1.36", Resources: reqs("4", "1Gi", "1")},
		},
	})
	assertRejected(t, env, both, "VMBudgetExceeded")

	initBig := testPod("init-big", corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "prep", Image: "busybox:1.36", Resources: reqs("7", "", "")}},
		Containers:     []corev1.Container{macosContainer("100m", "1Gi", "1")},
	})
	assertRejected(t, env, initBig, "VMBudgetExceeded")

	always := corev1.ContainerRestartPolicyAlways
	sideInit := testPod("side-init", corev1.PodSpec{
		InitContainers: []corev1.Container{{
			Name: "mesh", Image: "busybox:1.36", RestartPolicy: &always, Resources: reqs("4", "", ""),
		}},
		Containers: []corev1.Container{macosContainer("3", "1Gi", "1")},
	})
	assertRejected(t, env, sideInit, "VMBudgetExceeded")

	mem := testPod("mem", corev1.PodSpec{
		Containers: []corev1.Container{
			macosContainer("1", "8Gi", "1"),
			{Name: "log", Image: "busybox:1.36", Resources: reqs("100m", "8Gi", "1")},
		},
	})
	assertRejected(t, env, mem, "VMBudgetExceeded")
}

func TestAdmissionOnePodOneSlotWithInitAndSidecar(t *testing.T) {
	env := newAdmitEnv(t, sidecar.NewMemory())
	// Two regular inits must not be summed: max(3, 4) is 4, and the app is 1,
	// so the pod fits in 6 allocatable CPUs. Summing the inits would not.
	// Each container requests a slot; the pod still consumes one.
	pod := testPod("hybrid", corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: "a", Image: "busybox:1.36", Resources: reqs("3", "1Gi", "1")},
			{Name: "b", Image: "busybox:1.36", Resources: reqs("4", "1Gi", "1")},
		},
		Containers: []corev1.Container{
			macosContainer("1", "1Gi", "1"),
			{Name: "log", Image: "busybox:1.36", Resources: reqs("100m", "256Mi", "1")},
		},
	})
	if err := env.p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var st *corev1.PodStatus
	for time.Now().Before(deadline) {
		got, err := env.p.GetPodStatus(context.Background(), "default", "hybrid")
		if err == nil && got.Phase == corev1.PodRunning {
			st = got
			break
		}
		st = got
		time.Sleep(20 * time.Millisecond)
	}
	if st == nil || st.Phase != corev1.PodRunning {
		t.Fatalf("phase %v, want Running", st)
	}
	if env.slots.Used() != 1 {
		t.Fatalf("slots used %d, want exactly 1 for a multi-container pod", env.slots.Used())
	}
	if env.rt.Starts() != 1 {
		t.Fatalf("runtime starts %d, want 1 VM", env.rt.Starts())
	}
}

func TestAdmissionDeleteAllowsACorrectedPod(t *testing.T) {
	env := newAdmitEnv(t, sidecar.None{})
	bad := testPod("fix", corev1.PodSpec{
		NodeSelector: map[string]string{corev1.LabelOSStable: "linux"},
		Containers:   []corev1.Container{macosContainer("", "", "")},
	})
	assertRejected(t, env, bad, "UnsupportedPlatform")
	if err := env.p.DeletePod(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	if _, err := env.p.GetPodStatus(context.Background(), "default", "fix"); err == nil {
		t.Fatal("deleted rejection is still visible")
	}
	good := testPod("fix", corev1.PodSpec{Containers: []corev1.Container{macosContainer("", "", "")}})
	good.UID = "uid-fix-2"
	if err := env.p.CreatePod(context.Background(), good); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveResourcesKubeletWay(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	slot := corev1.ResourceName(types.ResourceVMSlot)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: "a", Resources: reqs("3", "2Gi", "1")},
			{Name: "b", Resources: reqs("1", "8Gi", "1")},
			{Name: "mesh", RestartPolicy: &always, Resources: reqs("500m", "1Gi", "1")},
		},
		Containers: []corev1.Container{
			{Name: "vm", Resources: reqs("2", "1Gi", "1")},
			{Name: "log", Resources: reqs("1", "256Mi", "1")},
		},
		Overhead: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}}
	got := effectiveResources(pod)
	// max(largest regular init, apps + restartable init) + overhead.
	// CPU: max(3, 2+1+0.5)+0.1 = 3.6. Memory: max(8Gi, 1Gi+256Mi+1Gi)+128Mi = 8Gi+128Mi.
	if got.CPU.Cmp(resource.MustParse("3600m")) != 0 {
		t.Fatalf("cpu %s, want 3600m", got.CPU.String())
	}
	wantMem := resource.MustParse("8Gi")
	wantMem.Add(resource.MustParse("128Mi"))
	if got.Memory.Cmp(wantMem) != 0 {
		t.Fatalf("memory %s, want %s", got.Memory.String(), wantMem.String())
	}
	// A non-restartable init ordered after a restartable one counts both.
	overlap := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: "early", Resources: reqs("1", "", "")},
			{Name: "side", RestartPolicy: &always, Resources: reqs("3", "", "")},
			{Name: "late", Resources: reqs("2", "", "")},
		},
		Containers: []corev1.Container{{Name: "vm", Resources: reqs("1", "", "")}},
	}}
	if got := effectiveResources(overlap); got.CPU.Cmp(resource.MustParse("5")) != 0 {
		t.Fatalf("overlap cpu %s, want 5 (late init plus the restartable sidecar already running)", got.CPU.String())
	}
	if q := pod.Spec.Containers[0].Resources.Requests[slot]; q.IsZero() {
		t.Fatal("fixture should request a slot so the assertion below is about ignoring it")
	}
}

func TestDiskImageAndOSRules(t *testing.T) {
	budget := vmBudget{CPU: resource.MustParse("8"), Memory: resource.MustParse("16Gi"), Known: true}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "guest.img")
	if err := os.WriteFile(img, []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(t.TempDir(), "rootfs")
	if err := os.WriteFile(plain, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		pod    *corev1.Pod
		reason string
	}{
		{name: "local-macos", pod: testPod("a", corev1.PodSpec{Containers: []corev1.Container{macosContainer("1", "1Gi", "1")}})},
		{name: "registry-macos", pod: testPod("b", corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "registry.example.com/macos:15"}}})},
		{name: "macos-prefix", pod: testPod("c", corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "ghcr.io/acme/macos-sequoia:15"}}})},
		{name: "baked-dir", pod: testPod("d", corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: dir}}})},
		{name: "img-file", pod: testPod("e", corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: img}}})},
		{name: "annotation", pod: annPod("f", "ghcr.io/acme/xcode:16")},
		{name: "os-darwin", pod: testPod("g", corev1.PodSpec{
			NodeSelector: map[string]string{corev1.LabelOSStable: "darwin"},
			Containers:   []corev1.Container{macosContainer("", "", "")},
		})},
		{name: "busybox", pod: testPod("h", corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox:latest"}}}), reason: ReasonUnsupportedPlatform},
		{name: "library-macos", pod: testPod("i", corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "macos:15"}}}), reason: ReasonUnsupportedPlatform},
		{name: "xcode", pod: testPod("j", corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "ghcr.io/acme/xcode:16"}}}), reason: ReasonUnsupportedPlatform},
		{name: "host-file", pod: testPod("k", corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: plain}}}), reason: ReasonUnsupportedPlatform},
		{name: "windows", pod: testPod("l", corev1.PodSpec{
			NodeSelector: map[string]string{"beta.kubernetes.io/os": "windows"},
			Containers:   []corev1.Container{macosContainer("", "", "")},
		}), reason: ReasonUnsupportedPlatform},
		{name: "notin-darwin", pod: testPod("m", corev1.PodSpec{
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{
						Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"darwin"},
					}},
				}}},
			}},
			Containers: []corev1.Container{macosContainer("", "", "")},
		}), reason: ReasonUnsupportedPlatform},
		{name: "annotation-still-linux", pod: func() *corev1.Pod {
			p := annPod("n", "ghcr.io/acme/xcode:16")
			p.Spec.NodeSelector = map[string]string{corev1.LabelOSStable: "linux"}
			return p
		}(), reason: ReasonUnsupportedPlatform},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := decideAdmission(tc.pod, budget)
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Reason != tc.reason {
				t.Fatalf("err=%v, want %s", err, tc.reason)
			}
		})
	}
}

func annPod(name, image string) *corev1.Pod {
	pod := testPod(name, corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: image}}})
	pod.Annotations = map[string]string{AnnotationDiskImage: "true"}
	return pod
}
