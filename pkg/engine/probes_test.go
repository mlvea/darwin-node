package engine

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestProbeTiming(t *testing.T) {
	initial, period, th := probeTiming(&corev1.Probe{InitialDelaySeconds: 5, PeriodSeconds: 2, FailureThreshold: 4})
	if initial != 5*time.Second || period != 2*time.Second || th != 4 {
		t.Fatalf("%v %v %d", initial, period, th)
	}
	_, period, th = probeTiming(nil)
	if period != 10*time.Second || th != 3 {
		t.Fatalf("defaults %v %d", period, th)
	}
	_, period, th = probeTiming(&corev1.Probe{})
	if period != 10*time.Second || th != 3 {
		t.Fatalf("zero period %v %d", period, th)
	}
	if probeSuccessThreshold(nil) != 1 || probeSuccessThreshold(&corev1.Probe{SuccessThreshold: 3}) != 3 {
		t.Fatal("success threshold")
	}
}

func TestProbeContextIndependentTimeout(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx, stop := probeContext(parent, 50*time.Millisecond)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("probe context should time out independently")
	}
}

func TestK8sProbeExecHTTP(t *testing.T) {
	req, ok := k8sProbe(&corev1.Probe{
		ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
		TimeoutSeconds: 2,
	})
	if !ok || req.Type != "exec" || req.Argv[0] != "true" {
		t.Fatalf("%+v %v", req, ok)
	}
	req, ok = k8sProbe(&corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: "/ready", Port: intstr.FromInt(8080),
	}}})
	if !ok || req.Type != "httpGet" {
		t.Fatalf("%+v", req)
	}
	_, ok = k8sProbe(nil)
	if ok {
		t.Fatal("nil")
	}
}

func TestVMForwardIPRejectsUnassignedAndSpecial(t *testing.T) {
	cases := []struct {
		reported string
		assigned string
	}{
		{"127.0.0.1", ""},
		{"127.0.0.2", ""},
		{"::1", ""},
		{"169.254.1.1", ""},
		{"fe80::1", ""},
		{"0.0.0.0", ""},
		{"255.255.255.255", ""},
		{"224.0.0.1", ""},
		{"192.168.64.9", ""},
		{"10.1.2.3", ""},
	}
	for _, tc := range cases {
		ip, err := vmForwardIP(tc.reported, tc.assigned)
		if err == nil || ip != "" {
			t.Fatalf("vmForwardIP(%q, %q) = %q, %v; want rejected", tc.reported, tc.assigned, ip, err)
		}
	}
}

func TestVMForwardIPUsesHostAssignment(t *testing.T) {
	ip, err := vmForwardIP("192.168.64.2", "192.168.64.2")
	if err != nil || ip != "192.168.64.2" {
		t.Fatalf("match: %q %v", ip, err)
	}
	ip, err = vmForwardIP("::ffff:192.168.64.2", "192.168.64.2")
	if err != nil || ip != "192.168.64.2" {
		t.Fatalf("canonical: %q %v", ip, err)
	}
	ip, err = vmForwardIP("2001:0db8:0000::0001", "2001:db8::1")
	if err != nil || ip != "2001:db8::1" {
		t.Fatalf("ipv6 canonical: %q %v", ip, err)
	}
	ip, err = vmForwardIP("", "192.168.64.2")
	if err != nil || ip != "192.168.64.2" {
		t.Fatalf("assigned only: %q %v", ip, err)
	}
	ip, err = vmForwardIP("127.0.0.1", "192.168.64.2")
	if err == nil || ip != "192.168.64.2" || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("loopback lie: %q %v", ip, err)
	}
	ip, err = vmForwardIP("10.9.9.9", "192.168.64.2")
	if err == nil || ip != "192.168.64.2" {
		t.Fatalf("other network: %q %v", ip, err)
	}
	ip, err = vmForwardIP("169.254.1.1", "192.168.64.2")
	if err == nil || ip != "192.168.64.2" {
		t.Fatalf("link-local lie: %q %v", ip, err)
	}
	if _, err := vmForwardIP("192.168.64.2", "127.0.0.1"); err == nil {
		t.Fatal("loopback assignment accepted")
	}
	if _, err := vmForwardIP("2001:db8::1", "169.254.1.1"); err == nil {
		t.Fatal("link-local assignment accepted")
	}
	ip, err = vmForwardIP("2001:db8::1", "2001:db8::1")
	if err != nil || ip != "2001:db8::1" {
		t.Fatalf("ipv6: %q %v", ip, err)
	}
}

func TestHostPortMapsUsesAssignmentAndBindHost(t *testing.T) {
	pod := samplePod("hp", "uid")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{
		HostPort: 8080, ContainerPort: 80, Protocol: corev1.ProtocolTCP,
	}}
	if maps := hostPortMaps(pod, "127.0.0.1", "10.1.2.3"); len(maps) != 0 {
		t.Fatalf("loopback forwarded: %+v", maps)
	}
	if maps := hostPortMaps(pod, "169.254.1.1", "10.1.2.3"); len(maps) != 0 {
		t.Fatalf("link-local forwarded: %+v", maps)
	}
	maps := hostPortMaps(pod, "192.168.64.2", "10.1.2.3")
	if len(maps) != 1 || maps[0].PodIP != "192.168.64.2" || maps[0].HostIP != "10.1.2.3" {
		t.Fatalf("%+v", maps)
	}
	pod.Spec.Containers[0].Ports[0].HostIP = "192.0.2.10"
	maps = hostPortMaps(pod, "192.168.64.2", "10.1.2.3")
	if len(maps) != 1 || maps[0].HostIP != "192.0.2.10" {
		t.Fatalf("explicit hostIP: %+v", maps)
	}
}

func TestChooseBindHost(t *testing.T) {
	local := func(ip net.IP) bool {
		s := ip.String()
		return s == "10.1.2.3" || s == "127.0.0.1"
	}
	if got := chooseBindHost("", "10.1.2.3", local); got != "10.1.2.3" {
		t.Fatalf("node IP: %q", got)
	}
	if got := chooseBindHost("", "127.0.0.1", local); got != "" {
		t.Fatalf("loopback node IP must not become the implicit bind, got %q", got)
	}
	if got := chooseBindHost("", "10.0.0.1", local); got != "" {
		t.Fatalf("non-local node IP: %q", got)
	}
	if got := chooseBindHost("127.0.0.1", "10.1.2.3", local); got != "10.1.2.3" {
		t.Fatalf("loopback listen address must fall through to the node IP, got %q", got)
	}
	if got := chooseBindHost("10.1.2.3", "10.9.9.9", local); got != "10.1.2.3" {
		t.Fatalf("configured listen address: %q", got)
	}
	if got := chooseBindHost("169.254.1.1", "10.1.2.3", local); got != "10.1.2.3" {
		t.Fatalf("link-local listen address: %q", got)
	}
	if got := chooseBindHost("10.9.9.9", "", local); got != "" {
		t.Fatalf("non-local listen address: %q", got)
	}
}
