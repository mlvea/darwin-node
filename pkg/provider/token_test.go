package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/node"
	rtfake "github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func projectedPod(uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default", UID: k8stypes.UID(uid)},
		Spec: corev1.PodSpec{
			ServiceAccountName: "builder",
			Volumes: []corev1.Volume{{
				Name: "proj",
				VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", Audience: "api"}},
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "extra", Audience: "build"}},
				}}},
			}},
			Containers: []corev1.Container{{
				Name:  "macos",
				Image: "local/macos:test",
				VolumeMounts: []corev1.VolumeMount{{
					Name: "proj", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
				}},
			}},
		},
	}
}

func tokenReactor(fn func(req *authenticationv1.TokenRequest) error) *k8sfake.Clientset {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "builder", Namespace: "default"}}
	client := k8sfake.NewSimpleClientset(sa)
	client.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		cre, ok := action.(k8stesting.CreateAction)
		if !ok {
			return true, nil, errors.New("token create action has no object")
		}
		req, ok := cre.GetObject().(*authenticationv1.TokenRequest)
		if !ok || req == nil {
			return true, nil, errors.New("token request missing")
		}
		out := req.DeepCopy()
		if err := fn(out); err != nil {
			return true, nil, err
		}
		return true, out, nil
	})
	return client
}

func TestKubeResolverRejectsEmptyAndFailedToken(t *testing.T) {
	pod := projectedPod("uid-empty")
	empty := tokenReactor(func(req *authenticationv1.TokenRequest) error { return nil })
	if _, err := (KubeResolver{Client: empty}).Resolve(context.Background(), pod); err == nil {
		t.Fatal("empty TokenRequest status must fail closed")
	}
	failed := tokenReactor(func(req *authenticationv1.TokenRequest) error {
		return errors.New("apiserver down")
	})
	_, err := (KubeResolver{Client: failed}).Resolve(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), "apiserver down") {
		t.Fatalf("error %v", err)
	}
}

func TestKubeResolverMintsOneTokenPerProjection(t *testing.T) {
	client := tokenReactor(func(req *authenticationv1.TokenRequest) error {
		aud := "none"
		if len(req.Spec.Audiences) == 1 {
			aud = req.Spec.Audiences[0]
		}
		req.Status.Token = "tok-" + aud
		req.Status.ExpirationTimestamp = metav1.NewTime(time.Now().Add(time.Hour))
		return nil
	})
	creds, err := (KubeResolver{Client: client}).Resolve(context.Background(), projectedPod("uid-two"))
	if err != nil {
		t.Fatal(err)
	}
	if len(creds.Tokens) != 2 {
		t.Fatalf("tokens %+v", creds.Tokens)
	}
	got := map[string]string{}
	for _, tok := range creds.Tokens {
		got[tok.Path] = tok.Token
		if tok.ExpiresAt.IsZero() {
			t.Fatalf("missing expiry %+v", tok)
		}
	}
	if got["token"] != "tok-api" || got["extra"] != "tok-build" {
		t.Fatalf("tokens %+v", got)
	}
}

func TestCreatePodFailsClosedWhenTokenMintFails(t *testing.T) {
	leakcheck.Check(t)
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	eng := engine.New(cfg, slots, rtfake.New(), sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30}, Cfg: cfg}, event.Nop{})
	client := tokenReactor(func(*authenticationv1.TokenRequest) error {
		return errors.New("tokenreviews forbidden")
	})
	p.SetCredentialResolver(KubeResolver{Client: client})
	err := p.CreatePod(context.Background(), projectedPod("uid-deny"))
	if err == nil || !strings.Contains(err.Error(), "tokenreviews forbidden") {
		t.Fatalf("create: %v", err)
	}
	if _, getErr := p.GetPod(context.Background(), "default", "macos"); getErr == nil {
		t.Fatal("failed mint must not admit the pod")
	}
	if slots.Used() != 0 {
		t.Fatalf("slots %d", slots.Used())
	}
}

func TestProviderRefreshesProjectedToken(t *testing.T) {
	leakcheck.Check(t)
	var calls atomic.Int32
	client := tokenReactor(func(req *authenticationv1.TokenRequest) error {
		n := calls.Add(1)
		req.Status.Token = "first"
		req.Status.ExpirationTimestamp = metav1.NewTime(time.Now().Add(120 * time.Millisecond))
		if n > 1 {
			req.Status.Token = "second"
			req.Status.ExpirationTimestamp = metav1.NewTime(time.Now().Add(time.Hour))
		}
		return nil
	})
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.ProbeInterval = 20 * time.Millisecond
	eng := engine.New(cfg, slots, rtfake.New(), sidecar.None{}, event.Nop{}, "10.0.0.8")
	p := New(cfg, eng, node.Inventory{Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30}, Cfg: cfg}, event.Nop{})
	p.SetCredentialResolver(KubeResolver{Client: client})
	pod := projectedPod("uid-refresh")
	pod.Spec.Volumes[0].Projected.Sources = pod.Spec.Volumes[0].Projected.Sources[:1]
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.DeletePod(context.Background(), pod) })
	tokenPath := filepath.Join(cfg.CacheDir, "pods", "uid-refresh", "volumes", "proj", "token")
	deadline := time.Now().Add(3 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(tokenPath)
		last = b
		if err == nil && string(b) == "second" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("token %q after %d mints", last, calls.Load())
}
