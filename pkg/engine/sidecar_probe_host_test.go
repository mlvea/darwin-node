package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mlvea/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type addressedRuntime struct {
	sidecar.Runtime
	ip string
}

func (a addressedRuntime) Endpoint(context.Context, string, string, string) (string, error) {
	if _, ok := sidecar.RoutableIP(a.ip); !ok {
		return "", fmt.Errorf("sidecar has no address")
	}
	return a.ip, nil
}

func routableIPv4s(t *testing.T) []string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			ip := ipnet.IP.To4().String()
			if _, ok := sidecar.RoutableIP(ip); !ok {
				continue
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
			if err != nil {
				continue
			}
			_ = ln.Close()
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		t.Fatal("no bindable non-loopback IPv4; sidecar probe tests cannot prove the dial target")
	}
	return out
}

func assertNoProbeBody(t *testing.T, p *corev1.Pod, secret string) {
	t.Helper()
	if p == nil {
		t.Fatal("nil pod")
	}
	if strings.Contains(p.Status.Message, secret) {
		t.Fatalf("pod message leaked probe body: %q", p.Status.Message)
	}
	for _, c := range p.Status.Conditions {
		if strings.Contains(c.Message, secret) {
			t.Fatalf("condition %s leaked probe body: %q", c.Type, c.Message)
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && strings.Contains(cs.State.Waiting.Message, secret) {
			t.Fatalf("waiting leaked probe body: %q", cs.State.Waiting.Message)
		}
	}
}

func serveRoutable(t *testing.T, h http.Handler) (string, int) {
	t.Helper()
	ip := routableIPv4s(t)[0]
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ip, ln.Addr().(*net.TCPAddr).Port
}

// An empty sidecar probe host must not be rewritten to the node loopback.
// The dial runs in the node process, and the message is what a pod event publishes.
func TestSidecarProbeEmptyHostDoesNotDialNodeLoopback(t *testing.T) {
	const secret = "TOP-SECRET-PROBE-BODY"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, secret, http.StatusInternalServerError)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	ok, msg := runSidecarProbe(context.Background(), sidecar.None{}, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromInt(port), Path: "/healthz",
		}},
		TimeoutSeconds: 1,
	}, nil)
	if hits.Load() != 0 {
		t.Fatalf("empty HTTP host dialed the node loopback (%d hits); probe ok=%v msg=%q", hits.Load(), ok, msg)
	}
	if ok {
		t.Fatal("probe succeeded without a container address")
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("probe message leaked the response body: %q", msg)
	}

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var tcpHits atomic.Int32
	go func() {
		for {
			c, err := tcpLn.Accept()
			if err != nil {
				return
			}
			tcpHits.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = tcpLn.Close() })
	ok, msg = runSidecarProbe(context.Background(), sidecar.None{}, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
			Port: intstr.FromInt(tcpLn.Addr().(*net.TCPAddr).Port),
		}},
		TimeoutSeconds: 1,
	}, nil)
	if tcpHits.Load() != 0 {
		t.Fatalf("empty TCP host dialed the node loopback (%d hits); probe ok=%v msg=%q", tcpHits.Load(), ok, msg)
	}
	if ok {
		t.Fatal("TCP probe succeeded without a container address")
	}
}

// A probe that names the node loopback is not dialed there. Only the
// sidecar's own address may be dialed, and the body is not copied into
// the message published on the pod.
func TestSidecarProbeExplicitHostDoesNotDialNodeOrLeakBody(t *testing.T) {
	const secret = "TOP-SECRET-PROBE-BODY-EXPLICIT"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, secret)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	ok, msg := runSidecarProbe(context.Background(), sidecar.None{}, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Host: "127.0.0.1", Port: intstr.FromInt(port), Path: "/",
		}},
		TimeoutSeconds: 1,
	}, nil)
	if hits.Load() != 0 {
		t.Fatalf("explicit loopback host was dialed on the node (%d hits) msg=%q", hits.Load(), msg)
	}
	if ok {
		t.Fatal("loopback probe succeeded")
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("probe message leaked the response body: %q", msg)
	}
}

func TestSidecarProbeDialsContainerAddress(t *testing.T) {
	const secret = "TOP-SECRET-PROBE-BODY-CONTAINER"
	var hits atomic.Int32
	ip, port := serveRoutable(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, secret, http.StatusInternalServerError)
	}))
	rt := addressedRuntime{Runtime: sidecar.NewMemory(), ip: ip}
	ok, msg := runSidecarProbe(context.Background(), rt, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromInt(port), Path: "/healthz",
		}},
		TimeoutSeconds: 1,
	}, nil)
	if hits.Load() != 1 || ok || msg != "HTTP 500" || strings.Contains(msg, secret) {
		t.Fatalf("hits=%d ok=%v msg=%q", hits.Load(), ok, msg)
	}
	ok, msg = runSidecarProbe(context.Background(), rt, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
			Host: ip, Port: intstr.FromInt(port),
		}},
		TimeoutSeconds: 1,
	}, nil)
	if !ok || msg != "" {
		t.Fatalf("tcp ok=%v msg=%q", ok, msg)
	}

	ok, msg = runSidecarProbe(context.Background(), rt, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Host: "203.0.113.10", Port: intstr.FromInt(port), Path: "/",
		}},
		TimeoutSeconds: 1,
	}, nil)
	if ok || !strings.Contains(msg, "not the sidecar") || strings.Contains(msg, secret) {
		t.Fatalf("foreign host ok=%v msg=%q hits=%d", ok, msg, hits.Load())
	}
	if hits.Load() != 1 {
		t.Fatalf("foreign host dialed the container: hits=%d", hits.Load())
	}
}

func TestSidecarProbeDoesNotFollowRedirectsOrUseProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	const secret = "REDIRECT-SECRET-BODY"
	var loopHits atomic.Int32
	loop, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	loopSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loopHits.Add(1)
		_, _ = io.WriteString(w, secret)
	})}
	go func() { _ = loopSrv.Serve(loop) }()
	t.Cleanup(func() { _ = loopSrv.Close() })

	var hits atomic.Int32
	ip, port := serveRoutable(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "http://"+loop.Addr().String()+"/leak")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, secret)
	}))
	ok, msg := runSidecarProbe(context.Background(), addressedRuntime{Runtime: sidecar.NewMemory(), ip: ip}, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromInt(port), Path: "/healthz",
		}},
		TimeoutSeconds: 1,
	}, nil)
	if hits.Load() != 1 || loopHits.Load() != 0 || !ok || strings.Contains(msg, secret) {
		t.Fatalf("hits=%d loop=%d ok=%v msg=%q", hits.Load(), loopHits.Load(), ok, msg)
	}
}

// A named sidecar port is the container port, dialed on the container
// address. An unknown name is port 0 and is not dialed.
func TestSidecarProbeNamedPortDialsResolvedContainerPort(t *testing.T) {
	var hits atomic.Int32
	ip, port := serveRoutable(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "named-port-body", http.StatusInternalServerError)
	}))
	ports := []corev1.ContainerPort{{
		Name: "http", ContainerPort: int32(port), Protocol: corev1.ProtocolTCP,
	}}
	rt := addressedRuntime{Runtime: sidecar.NewMemory(), ip: ip}
	ok, msg := runSidecarProbe(context.Background(), rt, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromString("http"), Path: "/healthz",
		}},
		TimeoutSeconds: 1,
	}, ports)
	if hits.Load() != 1 || ok || msg != "HTTP 500" || strings.Contains(msg, "named-port-body") {
		t.Fatalf("named http: hits=%d ok=%v msg=%q", hits.Load(), ok, msg)
	}
	ok, msg = runSidecarProbe(context.Background(), rt, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
			Port: intstr.FromString("http"),
		}},
		TimeoutSeconds: 1,
	}, ports)
	if !ok || msg != "" {
		t.Fatalf("named tcp: ok=%v msg=%q", ok, msg)
	}
	before := hits.Load()
	ok, msg = runSidecarProbe(context.Background(), rt, "ns", "pod", "log", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromString("missing"), Path: "/",
		}},
		TimeoutSeconds: 1,
	}, ports)
	if ok || hits.Load() != before || !strings.Contains(msg, "port 0") {
		t.Fatalf("unknown name: hits=%d ok=%v msg=%q", hits.Load(), ok, msg)
	}
}
