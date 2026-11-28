package engine

import (
	"bytes"
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/runtime/supervised"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

var skewNodeSeq atomic.Int64

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// countingNodes records patches the skew cordon sends and forwards them
// to the fake clientset, which is the node the assertions read back.
type countingNodes struct {
	mu      sync.Mutex
	inner   node.NodePatcher
	patches [][]byte
}

func (c *countingNodes) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error) {
	return c.inner.Get(ctx, name, opts)
}

func (c *countingNodes) Patch(ctx context.Context, name string, pt k8stypes.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*corev1.Node, error) {
	c.mu.Lock()
	c.patches = append(c.patches, append([]byte(nil), data...))
	c.mu.Unlock()
	return c.inner.Patch(ctx, name, pt, data, opts, subresources...)
}

// setsUnschedulable counts JSON patches that write spec.unschedulable.
func (c *countingNodes) setsUnschedulable(v bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	path := []byte(`"/spec/unschedulable"`)
	val := []byte(`"value":false`)
	if v {
		val = []byte(`"value":true`)
	}
	for _, p := range c.patches {
		if bytes.Contains(p, path) && bytes.Contains(p, val) {
			n++
		}
	}
	return n
}

func registerSkewNode(t *testing.T, n *corev1.Node) (*k8sfake.Clientset, *countingNodes) {
	t.Helper()
	cs := k8sfake.NewSimpleClientset(n)
	wrap := &countingNodes{inner: cs.CoreV1().Nodes()}
	RegisterSkewNodeAPI(n.Name, wrap)
	t.Cleanup(func() { RegisterSkewNodeAPI(n.Name, nil) })
	return cs, wrap
}

func apiNode(name string, unschedulable bool, owner string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"}}
	n.Spec.Unschedulable = unschedulable
	if owner != "" {
		n.Annotations = map[string]string{node.AnnotationCordonedBy: owner}
	}
	return n
}

func getNode(t *testing.T, cs kubernetes.Interface, name string) *corev1.Node {
	t.Helper()
	got, err := cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func supEngineNamed(t *testing.T, cache, runDir, nodeName string, rec event.Recorder) (*Engine, *supervised.Runtime) {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = cache
	cfg.NodeName = nodeName
	cfg.AllowNATWorkloads = true
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.ProbeInterval = 20 * time.Millisecond
	inner := supervised.New(supervised.Options{
		RunDir:        runDir,
		Slots:         2,
		Deadman:       time.Hour,
		VMRuntime:     types.RuntimeFake,
		BinaryVersion: "agent-test",
	})
	var rt runtime.Runtime = inner
	if rec == nil {
		rec = event.Nop{}
	}
	e := New(cfg, slots, rt, sidecar.None{}, rec, "10.0.0.1")
	return e, inner
}

func startSupMajorSlot(t *testing.T, dir string, slot, major int) *runningSup {
	t.Helper()
	rt := fake.New()
	rt.SetBridgeGuest(true)
	ctx, cancel := context.WithCancel(context.Background())
	srv := &supervisor.Server{
		Slot:               slot,
		RunDir:             dir,
		RT:                 rt,
		BinaryVersion:      "sv-test",
		Deadman:            time.Hour,
		AdvertiseWireMajor: major,
	}
	s := &runningSup{rt: rt, srv: srv, cancel: cancel, done: make(chan struct{})}
	var serveErr error
	go func() {
		serveErr = srv.Serve(ctx)
		close(s.done)
	}()
	path := supervisor.SocketPath(dir, slot)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			t.Cleanup(func() {
				cancel()
				select {
				case <-s.done:
				case <-time.After(5 * time.Second):
					t.Errorf("supervisor slot %d did not exit: %v", slot, serveErr)
				}
			})
			return s
		}
		select {
		case <-s.done:
			t.Fatalf("supervisor slot %d exited: %v", slot, serveErr)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	t.Fatalf("socket %s did not appear", path)
	return nil
}

func bootOneMismatch(t *testing.T) (cache, dir string, sup *runningSup) {
	t.Helper()
	dir = shortRunDir(t)
	sup = startSupMajor(t, dir, 2)
	cache = t.TempDir()
	e1, _ := supEngine(t, cache, dir, false)
	if err := e1.Create(context.Background(), supPod("job", "uid-job"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e1, "default", "job", corev1.PodRunning)
	crashEngine(t, e1)
	return cache, dir, sup
}

func exitSupervisor(t *testing.T, sup *runningSup, rt *supervised.Runtime) {
	t.Helper()
	sup.cancel()
	select {
	case <-sup.done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not exit")
	}
	rt.WaitSkew(3 * time.Second)
}

func yieldCount(s *node.SkewCordon) int {
	n := 0
	for _, ev := range s.Events() {
		if ev.Kind == "yield" {
			n++
		}
	}
	return n
}

// An operator cordon that predates the hold must still be unschedulable
// after the mismatched supervisor exits. The published node is the object
// Apply writes; the fake clientset is the API object.
func TestOperatorPrecordonSurvivesSkewRelease(t *testing.T) {
	leakcheck.Check(t)
	cache, dir, sup := bootOneMismatch(t)
	name := "skew-pre-" + itoa(skewNodeSeq.Add(1))
	cs, patches := registerSkewNode(t, apiNode(name, true, ""))

	e2, inner := supEngineNamed(t, cache, dir, name, nil)
	t.Cleanup(func() { _ = e2.Close() })
	got := getNode(t, cs, name)
	if !got.Spec.Unschedulable {
		t.Fatal("hold cleared an operator cordon")
	}
	if got.Annotations[node.AnnotationCordonedBy] == node.CordonOwnerSupervisorSkew {
		t.Fatal("hold claimed an operator cordon")
	}

	exitSupervisor(t, sup, inner)
	got = getNode(t, cs, name)
	if !got.Spec.Unschedulable {
		t.Fatal("supervisor exit cleared an operator cordon")
	}
	if got.Annotations[node.AnnotationCordonedBy] == node.CordonOwnerSupervisorSkew {
		t.Fatal("exit wrote the skew cordon annotation")
	}
	if patches.setsUnschedulable(false) != 0 {
		t.Fatal("exit patched spec.unschedulable to false")
	}
	published := &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	e2.SupervisorSkew().Apply(published)
	if !published.Spec.Unschedulable {
		t.Fatal("release cleared an operator cordon on the published node")
	}
}

// The operator takes the cordon while the hold is active by replacing
// the ownership annotation. The exit must leave spec.unschedulable set.
func TestOperatorCordonDuringHoldStaysCordoned(t *testing.T) {
	leakcheck.Check(t)
	cache, dir, sup := bootOneMismatch(t)
	name := "skew-mid-" + itoa(skewNodeSeq.Add(1))
	cs, patches := registerSkewNode(t, apiNode(name, false, ""))

	e2, inner := supEngineNamed(t, cache, dir, name, nil)
	t.Cleanup(func() { _ = e2.Close() })
	got := getNode(t, cs, name)
	if !got.Spec.Unschedulable || got.Annotations[node.AnnotationCordonedBy] != node.CordonOwnerSupervisorSkew {
		t.Fatalf("hold did not cordon with our annotation: unschedulable %v annotations %v", got.Spec.Unschedulable, got.Annotations)
	}
	got.Spec.Unschedulable = true
	if got.Annotations == nil {
		got.Annotations = map[string]string{}
	}
	got.Annotations[node.AnnotationCordonedBy] = "operator"
	if _, err := cs.CoreV1().Nodes().Update(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	exitSupervisor(t, sup, inner)
	got = getNode(t, cs, name)
	if !got.Spec.Unschedulable {
		t.Fatal("exit cleared a cordon the operator took over")
	}
	if got.Annotations[node.AnnotationCordonedBy] != "operator" {
		t.Fatalf("annotation %v", got.Annotations)
	}
	if patches.setsUnschedulable(false) != 0 {
		t.Fatal("exit patched spec.unschedulable to false")
	}
	published := &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	e2.SupervisorSkew().Apply(published)
	if !published.Spec.Unschedulable {
		t.Fatal("published node cleared the operator cordon")
	}
}

// A restarted agent finds the annotation a previous process wrote and
// clears the cordon once, when the held supervisor exits.
func TestRestartedAgentUncordonsOwnedSkewOnce(t *testing.T) {
	leakcheck.Check(t)
	cache, dir, sup := bootOneMismatch(t)
	name := "skew-restart-" + itoa(skewNodeSeq.Add(1))
	cs, patches := registerSkewNode(t, apiNode(name, true, node.CordonOwnerSupervisorSkew))

	e2, inner := supEngineNamed(t, cache, dir, name, nil)
	t.Cleanup(func() { _ = e2.Close() })
	got := getNode(t, cs, name)
	if !got.Spec.Unschedulable || got.Annotations[node.AnnotationCordonedBy] != node.CordonOwnerSupervisorSkew {
		t.Fatalf("restart dropped our cordon before the supervisor exited: %+v", got.Spec)
	}

	exitSupervisor(t, sup, inner)
	got = getNode(t, cs, name)
	if got.Spec.Unschedulable {
		t.Fatal("restart left our cordon in place")
	}
	if got.Annotations[node.AnnotationCordonedBy] != "" {
		t.Fatalf("annotation %v remained", got.Annotations)
	}
	if patches.setsUnschedulable(false) != 1 {
		t.Fatalf("uncordon patches %d, want 1", patches.setsUnschedulable(false))
	}
	published := &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	e2.SupervisorSkew().Apply(published)
	again := getNode(t, cs, name)
	if again.Spec.Unschedulable || again.Annotations[node.AnnotationCordonedBy] != "" {
		t.Fatal("a later publish cordoned the node again")
	}
	if patches.setsUnschedulable(false) != 1 {
		t.Fatal("a later publish patched the cordon again")
	}
}

func TestTwoSkewHoldsUncordonAfterLast(t *testing.T) {
	leakcheck.Check(t)
	dir := shortRunDir(t)
	s0 := startSupMajorSlot(t, dir, 0, 2)
	s1 := startSupMajorSlot(t, dir, 1, 2)
	cache := t.TempDir()
	e1, _ := supEngine(t, cache, dir, false)
	if err := e1.Create(context.Background(), supPod("a", "uid-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	if err := e1.Create(context.Background(), supPod("b", "uid-b"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e1, "default", "a", corev1.PodRunning)
	waitPhase(t, e1, "default", "b", corev1.PodRunning)
	crashEngine(t, e1)

	name := "skew-two-" + itoa(skewNodeSeq.Add(1))
	cs, patches := registerSkewNode(t, apiNode(name, false, ""))
	e2, inner := supEngineNamed(t, cache, dir, name, nil)
	t.Cleanup(func() { _ = e2.Close() })
	if e2.slots.Used() != 2 {
		t.Fatalf("slots used %d, want both holds", e2.slots.Used())
	}
	got := getNode(t, cs, name)
	if !got.Spec.Unschedulable || got.Annotations[node.AnnotationCordonedBy] != node.CordonOwnerSupervisorSkew {
		t.Fatalf("two holds did not cordon once: unschedulable %v annotations %v", got.Spec.Unschedulable, got.Annotations)
	}
	if patches.setsUnschedulable(true) != 1 {
		t.Fatalf("cordon patches %d, want 1", patches.setsUnschedulable(true))
	}

	s0.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e2.slots.Used() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e2.slots.Used() != 1 {
		t.Fatalf("slots used %d after the first supervisor exited", e2.slots.Used())
	}
	got = getNode(t, cs, name)
	if !got.Spec.Unschedulable || got.Annotations[node.AnnotationCordonedBy] != node.CordonOwnerSupervisorSkew {
		t.Fatal("first exit cleared the cordon")
	}
	if patches.setsUnschedulable(false) != 0 {
		t.Fatal("first exit patched spec.unschedulable to false")
	}

	s1.cancel()
	select {
	case <-s0.done:
	case <-time.After(5 * time.Second):
		t.Fatal("slot 0 supervisor did not exit")
	}
	select {
	case <-s1.done:
	case <-time.After(5 * time.Second):
		t.Fatal("slot 1 supervisor did not exit")
	}
	inner.WaitSkew(3 * time.Second)
	got = getNode(t, cs, name)
	if got.Spec.Unschedulable {
		t.Fatal("last exit left the cordon")
	}
	if got.Annotations[node.AnnotationCordonedBy] != "" {
		t.Fatalf("annotation %v remained", got.Annotations)
	}
	if patches.setsUnschedulable(false) != 1 {
		t.Fatalf("uncordon patches %d, want 1", patches.setsUnschedulable(false))
	}
}

// kubectl uncordon during a hold clears spec.unschedulable and leaves
// annotations alone. Later publishes must not set the bit back.
func TestOperatorUncordonDuringHoldIsNotFought(t *testing.T) {
	leakcheck.Check(t)
	cache, dir, sup := bootOneMismatch(t)
	name := "skew-yield-" + itoa(skewNodeSeq.Add(1))
	cs, patches := registerSkewNode(t, apiNode(name, false, ""))
	rec := &recEvents{}
	e2, inner := supEngineNamed(t, cache, dir, name, rec)
	t.Cleanup(func() { _ = e2.Close() })

	got := getNode(t, cs, name)
	if !got.Spec.Unschedulable {
		t.Fatal("hold did not cordon")
	}
	got.Spec.Unschedulable = false
	if _, err := cs.CoreV1().Nodes().Update(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	skew := e2.SupervisorSkew()
	for i := 0; i < 5; i++ {
		skew.Apply(&corev1.Node{})
	}
	got = getNode(t, cs, name)
	if got.Spec.Unschedulable {
		t.Fatal("publish cordoned the node again after the operator uncordoned it")
	}
	if patches.setsUnschedulable(true) != 1 {
		t.Fatalf("cordon patches %d, want the original one", patches.setsUnschedulable(true))
	}
	if yieldCount(skew) != 1 {
		t.Fatalf("yield events %d", yieldCount(skew))
	}
	published := &corev1.Node{}
	skew.Apply(published)
	if published.Spec.Unschedulable {
		t.Fatal("published node fought the operator uncordon")
	}
	var yields int
	rec.mu.Lock()
	for _, w := range rec.warns {
		if bytes.Contains([]byte(w), []byte(node.ReasonSkewOperatorUncordon)) {
			yields++
		}
	}
	rec.mu.Unlock()
	if yields != 1 {
		t.Fatalf("operator-uncordon warnings %d", yields)
	}

	exitSupervisor(t, sup, inner)
	got = getNode(t, cs, name)
	if got.Spec.Unschedulable {
		t.Fatal("exit cordoned the node again")
	}
	if patches.setsUnschedulable(true) != 1 {
		t.Fatal("exit patched spec.unschedulable back to true")
	}
	if yieldCount(skew) != 1 {
		t.Fatalf("yield events after exit %d", yieldCount(skew))
	}
}
