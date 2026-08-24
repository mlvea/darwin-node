package engine

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidatePod(t *testing.T) {
	err := ValidatePod(&corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "x"}}}}, nil)
	if err == nil {
		t.Fatal("missing uid")
	}
	err = ValidatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "u"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: "img"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLooksLikeVMImageAndInitReject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/disk.img", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !LooksLikeVMImage(dir) {
		t.Fatal("dir with disk.img")
	}
	if LooksLikeVMImage("busybox:latest") {
		t.Fatal("linux image")
	}
	err := ValidatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "u"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "i", Image: dir}},
			Containers:     []corev1.Container{{Image: "img"}},
		},
	}, nil)
	if err == nil {
		t.Fatal("expected init vm image error")
	}
}

func TestValidatePodHostPathDefaultDeny(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "u"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "img"}},
			Volumes: []corev1.Volume{{
				Name:         "root",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
			}},
		},
	}
	if err := ValidatePod(pod, nil); err == nil {
		t.Fatal("expected hostPath / to be rejected on empty allowlist")
	}
	if err := ValidatePod(pod, []string{"/"}); err == nil {
		t.Fatal("expected hostPath / to be rejected even if / is allowlisted")
	}
}

func TestValidatePodHostPathAllowlist(t *testing.T) {
	dir := t.TempDir()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "u"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "img"}},
			Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: dir}},
			}},
		},
	}
	if err := ValidatePod(pod, nil); err == nil {
		t.Fatal("empty allowlist must deny hostPath")
	}
	if err := ValidatePod(pod, []string{dir}); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePod(pod, []string{filepath.Join(dir, "nope")}); err == nil {
		t.Fatal("expected deny for path outside allowlist")
	}
}

func TestVMResources(t *testing.T) {
	cpu, mem, err := VMResources(corev1.Container{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2500m"),
			corev1.ResourceMemory: resource.MustParse("3Gi"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cpu != 3 {
		t.Fatalf("cpu %d", cpu)
	}
	if mem < 3<<30 {
		t.Fatalf("mem %d", mem)
	}
	_, _, err = VMResources(corev1.Container{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}})
	if err == nil {
		t.Fatal("expected limit < request error")
	}
}
