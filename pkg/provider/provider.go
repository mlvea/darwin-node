// Package provider is the Virtual Kubelet adapter around the pod engine.
package provider

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/node"

	dto "github.com/prometheus/client_model/go"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

var _ nodeutil.Provider = (*Provider)(nil)

const ComponentName = "darwin-node"

// Provider implements the Virtual Kubelet PodLifecycleHandler plus extras.
type Provider struct {
	cfg    config.Config
	engine *engine.Engine
	inv    node.Inventory
	events event.Recorder
	creds  CredentialResolver

	usageMu   sync.Mutex
	usageAt   time.Time
	usageHost node.Host
	usagePct  float64
}

func New(cfg config.Config, eng *engine.Engine, inv node.Inventory, rec event.Recorder) *Provider {
	if rec == nil {
		rec = event.Nop{}
	}
	return &Provider{cfg: cfg, engine: eng, inv: inv, events: rec}
}

func (p *Provider) Engine() *engine.Engine { return p.engine }

func (p *Provider) Inventory() node.Inventory { return p.inv }

func (p *Provider) SetCredentialResolver(r CredentialResolver) { p.creds = r }

func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	var creds engine.Credentials
	if p.creds != nil {
		c, err := p.creds.Resolve(ctx, pod)
		if err != nil {
			return err
		}
		creds = c
	}
	return p.engine.Create(ctx, pod, creds)
}

func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	return errdefs.InvalidInput("pod updates are not supported; delete and recreate")
}

func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	return p.engine.Delete(ctx, pod.Namespace, pod.Name, deleteGrace(pod))
}

func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return p.engine.Get(namespace, name)
}

func (p *Provider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	return p.engine.List(), nil
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.engine.Get(namespace, name)
	if err != nil {
		return nil, err
	}
	return &pod.Status, nil
}

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	pod, err := p.engine.Get(namespace, podName)
	if err != nil {
		return nil, err
	}
	if len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Name == containerName {
		return p.engine.LogsVM(ctx, namespace, podName, opts)
	}
	return p.engine.SidecarLogs(ctx, namespace, podName, containerName, opts)
}

func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	pod, err := p.engine.Get(namespace, podName)
	if err != nil {
		return err
	}
	if len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Name == containerName {
		return p.engine.ExecInVM(ctx, namespace, podName, cmd, attach)
	}
	return p.engine.ExecInSidecar(ctx, namespace, podName, containerName, cmd, attach)
}

func (p *Provider) AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) error {
	return p.RunInContainer(ctx, namespace, podName, containerName, []string{"/bin/zsh"}, attach)
}

func (p *Provider) ConfigureNode(ctx context.Context, n *corev1.Node) error {
	p.inv.Slots = p.engine.Slots()
	return node.Apply(ctx, n, p.inv)
}

const hostUsageTTL = 30 * time.Second
const podMetricsTimeout = 2 * time.Second

func (p *Provider) setHostUsage(host node.Host, percent float64) {
	p.usageMu.Lock()
	p.usageHost = host
	p.usagePct = percent
	p.usageAt = time.Now()
	p.usageMu.Unlock()
}

func (p *Provider) hostUsage(ctx context.Context) (node.Host, float64) {
	p.usageMu.Lock()
	if !p.usageAt.IsZero() && time.Since(p.usageAt) < hostUsageTTL {
		h, pct := p.usageHost, p.usagePct
		p.usageMu.Unlock()
		return h, pct
	}
	p.usageMu.Unlock()

	host := p.inv.Host
	if h, err := node.ProbeHost(ctx); err == nil {
		host = h
	}
	percent := 0.0
	if cs, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(cs) > 0 {
		percent = cs[0]
	}
	p.setHostUsage(host, percent)
	return host, percent
}

func (p *Provider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	now := metav1.NewTime(time.Now())
	host, percent := p.hostUsage(ctx)
	nodeCPU := node.UsageNanoCores(host.LogicalCPUs, percent)
	nodeMem := node.UsageBytes(host.MemoryBytes, host.MemoryUsedFrac)
	summary := &statsv1alpha1.Summary{
		Node: statsv1alpha1.NodeStats{
			NodeName: p.cfg.NodeName,
			CPU:      &statsv1alpha1.CPUStats{Time: now, UsageNanoCores: uint64Ptr(nodeCPU)},
			Memory:   &statsv1alpha1.MemoryStats{Time: now, UsageBytes: uint64Ptr(nodeMem), WorkingSetBytes: uint64Ptr(nodeMem)},
		},
	}
	pods := p.engine.List()
	type sample struct {
		ok  bool
		cpu uint64
		mem uint64
	}
	samples := make([]sample, len(pods))
	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(i int, ns, name string) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, podMetricsTimeout)
			defer cancel()
			m, ok := p.engine.PodMetrics(pctx, ns, name)
			if !ok {
				return
			}
			samples[i] = sample{ok: true, cpu: m.CPUNanoCores, mem: m.MemoryWorkingSet}
		}(i, pod.Namespace, pod.Name)
	}
	wg.Wait()
	for i, pod := range pods {
		st := statsv1alpha1.PodStats{
			PodRef: statsv1alpha1.PodReference{Name: pod.Name, Namespace: pod.Namespace, UID: string(pod.UID)},
			StartTime: func() metav1.Time {
				if pod.Status.StartTime != nil {
					return *pod.Status.StartTime
				}
				return now
			}(),
		}
		if samples[i].ok {
			cpuN, memN := samples[i].cpu, samples[i].mem
			st.CPU = &statsv1alpha1.CPUStats{Time: now, UsageNanoCores: uint64Ptr(cpuN)}
			st.Memory = &statsv1alpha1.MemoryStats{Time: now, UsageBytes: uint64Ptr(memN), WorkingSetBytes: uint64Ptr(memN)}
			if len(pod.Spec.Containers) > 0 {
				st.Containers = []statsv1alpha1.ContainerStats{{
					Name:      pod.Spec.Containers[0].Name,
					StartTime: st.StartTime,
					CPU:       &statsv1alpha1.CPUStats{Time: now, UsageNanoCores: uint64Ptr(cpuN)},
					Memory:    &statsv1alpha1.MemoryStats{Time: now, UsageBytes: uint64Ptr(memN), WorkingSetBytes: uint64Ptr(memN)},
				}}
			}
		}
		summary.Pods = append(summary.Pods, st)
	}
	return summary, nil
}

func uint64Ptr(v uint64) *uint64 { return &v }

func (p *Provider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	return []*dto.MetricFamily{}, nil
}

func (p *Provider) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) error {
	return errdefs.InvalidInput("port-forward is not implemented; use hostPort or bridged PodIP")
}
