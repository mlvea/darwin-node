package engine

import (
	"context"
	"log/slog"
	"sync"

	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// skewNodeAPIs is filled before Engine.New. Recover cordons from inside
// New, which is before the process has built its cluster client.
var skewNodeAPIs sync.Map

var (
	kubeMu   sync.Mutex
	kubeOnce bool
	kubePath string
	kubeErr  error
	kubeNode node.NodePatcher

	rotationCordonOnce sync.Once
)

// RegisterSkewNodeAPI installs the node reader for nodeName. A nil
// reader removes it. Tests use this; a cluster process uses the
// kubeconfig when no entry is registered.
func RegisterSkewNodeAPI(nodeName string, nodes node.NodePatcher) {
	if nodeName == "" {
		return
	}
	if nodes == nil {
		skewNodeAPIs.Delete(nodeName)
		return
	}
	skewNodeAPIs.Store(nodeName, nodes)
}

// SetSkewNodeClient attaches the node client after Engine.New. Client
// certificate rotation has no client until the CSR is signed, which is
// after Recover. Reread corrects a hold that could not see the node.
func (e *Engine) SetSkewNodeClient(nodes node.NodePatcher) {
	if e == nil || nodes == nil {
		return
	}
	b := e.skewBridge()
	b.cordon.SetNodeAPI(e.cfg.NodeName, nodes)
	b.cordon.Reread(context.Background())
}

func (b *skewBridge) attachNodeAPI() {
	if b == nil || b.e == nil || b.cordon == nil {
		return
	}
	b.cordon.SetReporter(func(kind, reason, message string) {
		if kind != "yield" || b.e == nil || b.e.events == nil {
			return
		}
		b.e.events.Warn(context.Background(), reason, message)
		slog.Warn(message, "reason", reason, "node", b.e.cfg.NodeName)
	})
	if b.e.cfg.Cluster {
		b.cordon.SetNodeRequired(true)
	}
	name := b.e.cfg.NodeName
	if name == "" {
		return
	}
	if v, ok := skewNodeAPIs.Load(name); ok {
		if p, ok := v.(node.NodePatcher); ok && p != nil {
			b.cordon.SetNodeAPI(name, p)
		}
		return
	}
	if !b.e.cfg.Cluster || b.e.cfg.RotateClientCertificates {
		if b.e.cfg.RotateClientCertificates {
			rotationCordonOnce.Do(func() {
				slog.Warn("supervisor skew: client certificate rotation is on; call SetSkewNodeClient once the node client exists")
			})
		}
		return
	}
	p, err := clusterNodePatcher(b.e.cfg.KubeConfig)
	if err != nil {
		slog.Warn("supervisor skew: node spec was not read; an operator cordon will be left in place", "err", err)
		return
	}
	b.cordon.SetNodeAPI(name, p)
}

type coreNodePatcher struct {
	nodes interface {
		Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error)
		Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*corev1.Node, error)
	}
}

func (p coreNodePatcher) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error) {
	return p.nodes.Get(ctx, name, opts)
}

func (p coreNodePatcher) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*corev1.Node, error) {
	return p.nodes.Patch(ctx, name, pt, data, opts, subresources...)
}

// clusterNodePatcher builds one client per kubeconfig and remembers a
// failure. ClientsetFromEnv falls back to the in-cluster token, and a
// missing token should not be retried on every hold.
func clusterNodePatcher(kubeconfig string) (node.NodePatcher, error) {
	kubeMu.Lock()
	defer kubeMu.Unlock()
	if kubeOnce && kubePath == kubeconfig {
		return kubeNode, kubeErr
	}
	cs, err := nodeutil.ClientsetFromEnv(kubeconfig)
	kubeOnce = true
	kubePath = kubeconfig
	kubeErr = err
	if err != nil {
		kubeNode = nil
		return nil, err
	}
	kubeNode = coreNodePatcher{nodes: cs.CoreV1().Nodes()}
	return kubeNode, nil
}
