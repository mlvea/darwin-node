package provider

import (
	"context"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/node"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
)

// cpuSample is one series of container_cpu_usage_seconds_total. The guest
// reports an instantaneous nano-core rate, not a cumulative counter, so
// the counter is the integral of that rate across scrapes. The first
// sample is 0. A gap longer than cpuIntegrateCap does not invent a
// multi-hour sample from one rate.
const cpuIntegrateCap = 300 * time.Second

type cpuSample struct {
	at    time.Time
	cores float64
	total float64
}

// metricsResource is the /metrics/resource body metrics-server scrapes.
// Guest CPU and memory are used when the VM reports them. When it does
// not, the gauge is the configured VM allocation (requests, or the 2 CPU
// / 4Gi default) and the CPU counter advances at that many cores. That
// is the VM size, not measured use. Sidecar CPU is not included. Node
// series come from the host probe.
func (p *Provider) metricsResource(ctx context.Context) []*dto.MetricFamily {
	now := time.Now()
	if p.metricsNow != nil {
		now = p.metricsNow()
	}
	pods := p.engine.List()
	type sample struct {
		cores  float64
		mem    float64
		source string
		ok     bool
	}
	samples := make([]sample, len(pods))
	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(i int, pod *corev1.Pod) {
			defer wg.Done()
			samples[i] = p.podResource(ctx, pod)
		}(i, pod)
	}
	wg.Wait()

	var containerCPU, containerMem, podCPU, podMem []*dto.Metric
	for i, pod := range pods {
		s := samples[i]
		if !s.ok || len(pod.Spec.Containers) == 0 {
			continue
		}
		ns, name := pod.Namespace, pod.Name
		container := pod.Spec.Containers[0].Name
		base := []*dto.LabelPair{
			labelPair("namespace", ns),
			labelPair("pod", name),
		}
		clabels := append(append([]*dto.LabelPair{}, base...), labelPair("container", container), labelPair("source", s.source))
		plabels := append(append([]*dto.LabelPair{}, base...), labelPair("source", s.source))
		ckey := s.source + "|" + ns + "/" + name + "/" + container
		pkey := s.source + "|pod|" + ns + "/" + name
		containerCPU = append(containerCPU, counterMetric(clabels, p.integrateCPU(ckey, now, s.cores)))
		containerMem = append(containerMem, gaugeMetric(clabels, s.mem))
		podCPU = append(podCPU, counterMetric(plabels, p.integrateCPU(pkey, now, s.cores)))
		podMem = append(podMem, gaugeMetric(plabels, s.mem))
	}

	host, percent := p.hostUsage(ctx)
	nodeLabels := []*dto.LabelPair{labelPair("node", p.cfg.NodeName)}
	nodeCores := float64(node.UsageNanoCores(host.LogicalCPUs, percent)) / 1e9
	nodeMem := float64(node.UsageBytes(host.MemoryBytes, host.MemoryUsedFrac))
	return []*dto.MetricFamily{
		counterFamily("container_cpu_usage_seconds_total", "Cumulative CPU seconds. Guest samples are integrated from the instantaneous nano-core rate. source=allocation means the configured VM size, not measured use.", containerCPU),
		gaugeFamily("container_memory_working_set_bytes", "Memory working set. source=allocation is the configured VM memory.", containerMem),
		counterFamily("pod_cpu_usage_seconds_total", "Pod CPU seconds. Sidecar CPU is not included.", podCPU),
		gaugeFamily("pod_memory_working_set_bytes", "Pod memory working set. Sidecar memory is not included.", podMem),
		counterFamily("node_cpu_usage_seconds_total", "Host CPU seconds from the host probe.", []*dto.Metric{counterMetric(nodeLabels, p.integrateCPU("node", now, nodeCores))}),
		gaugeFamily("node_memory_working_set_bytes", "Host memory working set from the host probe.", []*dto.Metric{gaugeMetric(nodeLabels, nodeMem)}),
	}
}

func (p *Provider) podResource(ctx context.Context, pod *corev1.Pod) struct {
	cores  float64
	mem    float64
	source string
	ok     bool
} {
	out := struct {
		cores  float64
		mem    float64
		source string
		ok     bool
	}{ok: true}
	if pod == nil || len(pod.Spec.Containers) == 0 {
		out.ok = false
		return out
	}
	pctx, cancel := context.WithTimeout(ctx, podMetricsTimeout)
	defer cancel()
	if m, ok := p.engine.PodMetrics(pctx, pod.Namespace, pod.Name); ok {
		out.cores = float64(m.CPUNanoCores) / 1e9
		out.mem = float64(m.MemoryWorkingSet)
		out.source = "guest"
		return out
	}
	cpu, mem, err := engine.VMResources(pod.Spec.Containers[0])
	if err != nil {
		out.ok = false
		return out
	}
	out.cores = float64(cpu)
	out.mem = float64(mem)
	out.source = "allocation"
	return out
}

func (p *Provider) integrateCPU(key string, now time.Time, cores float64) float64 {
	p.cpuMu.Lock()
	defer p.cpuMu.Unlock()
	if p.cpuAcc == nil {
		p.cpuAcc = map[string]cpuSample{}
	}
	prev, ok := p.cpuAcc[key]
	total := 0.0
	if ok {
		dt := now.Sub(prev.at)
		if dt < 0 {
			dt = 0
		}
		if dt > cpuIntegrateCap {
			dt = cpuIntegrateCap
		}
		total = prev.total + prev.cores*dt.Seconds()
	}
	p.cpuAcc[key] = cpuSample{at: now, cores: cores, total: total}
	return total
}

func labelPair(k, v string) *dto.LabelPair {
	return &dto.LabelPair{Name: strPtr(k), Value: strPtr(v)}
}

func strPtr(s string) *string { return &s }

func f64Ptr(v float64) *float64 { return &v }

func counterMetric(labels []*dto.LabelPair, v float64) *dto.Metric {
	return &dto.Metric{Label: labels, Counter: &dto.Counter{Value: f64Ptr(v)}}
}

func gaugeMetric(labels []*dto.LabelPair, v float64) *dto.Metric {
	return &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: f64Ptr(v)}}
}

func counterFamily(name, help string, metrics []*dto.Metric) *dto.MetricFamily {
	t := dto.MetricType_COUNTER
	return &dto.MetricFamily{Name: strPtr(name), Help: strPtr(help), Type: &t, Metric: metrics}
}

func gaugeFamily(name, help string, metrics []*dto.Metric) *dto.MetricFamily {
	t := dto.MetricType_GAUGE
	return &dto.MetricFamily{Name: strPtr(name), Help: strPtr(help), Type: &t, Metric: metrics}
}
