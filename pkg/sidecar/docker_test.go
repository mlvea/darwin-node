package sidecar

import (
	"bytes"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDrainImagePull(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	rc := io.NopCloser(bytes.NewReader(payload))
	if err := drainImagePull(rc, nil); err != nil {
		t.Fatal(err)
	}
	if err := drainImagePull(nil, io.EOF); err == nil {
		t.Fatal("pull error must surface")
	}
}

func TestVolumeBindsAndResources(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}}}},
				{Name: "hp", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/host/ci"}}},
			},
		},
	}
	c := corev1.Container{
		Name: "log",
		VolumeMounts: []corev1.VolumeMount{
			{Name: "cfg", MountPath: "/etc/ci", ReadOnly: true},
			{Name: "hp", MountPath: "/host"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}
	hc := dockerHostConfig(pod, c, "/vol")
	if len(hc.Binds) != 2 {
		t.Fatalf("binds %v", hc.Binds)
	}
	foundCfg := false
	for _, b := range hc.Binds {
		if strings.Contains(b, "/etc/ci") && strings.HasSuffix(b, ":ro") {
			foundCfg = true
		}
	}
	if !foundCfg {
		t.Fatalf("configmap bind missing: %v", hc.Binds)
	}
	if hc.NanoCPUs != 250*1_000_000 {
		t.Fatalf("nanocpus %d", hc.NanoCPUs)
	}
	if hc.Memory != 64<<20 {
		t.Fatalf("memory %d", hc.Memory)
	}
}
