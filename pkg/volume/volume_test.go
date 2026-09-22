package volume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMaterializeConfigMapSecretEmptyDir(t *testing.T) {
	root := t.TempDir()
	optional := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid", Labels: map[string]string{"app": "x"}},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}}}},
				{Name: "sec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}},
				{Name: "ed", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "proj", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
					{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{
						{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
						{Path: "app", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['app']"}},
					}}},
				}}}},
			},
			Containers: []corev1.Container{{
				Name: "macos",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "cfg", MountPath: "/etc/cfg"},
					{Name: "sec", MountPath: "/etc/sec"},
					{Name: "ed", MountPath: "/scratch"},
					{Name: "proj", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"},
				},
			}},
		},
	}
	_ = optional
	shares, places, err := Materialize(Request{
		Pod:       pod,
		Container: pod.Spec.Containers[0],
		RootDir:   root,
		ConfigMaps: map[string]*corev1.ConfigMap{
			"cm": {Data: map[string]string{"app.plist": "ok"}},
		},
		Secrets: map[string]*corev1.Secret{
			"s": {Data: map[string][]byte{"token": []byte("shh")}},
		},
		ServiceToken: "satoken",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 4 || len(places) != 4 {
		t.Fatalf("shares=%d places=%d", len(shares), len(places))
	}
	b, err := os.ReadFile(filepath.Join(root, "cfg", "app.plist"))
	if err != nil || string(b) != "ok" {
		t.Fatalf("configmap: %s %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(root, "sec", "token"))
	if err != nil || string(b) != "shh" {
		t.Fatalf("secret: %s %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(root, "proj", "token"))
	if err != nil || string(b) != "satoken" {
		t.Fatalf("sa token: %s %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(root, "proj", "namespace"))
	if err != nil || string(b) != "ns" {
		t.Fatalf("ns: %s %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(root, "proj", "app"))
	if err != nil || string(b) != "x" {
		t.Fatalf("label: %s %v", b, err)
	}
}

func TestUnknownVolumeFailsClosed(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "pvc",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "x"},
				},
			}},
			Containers: []corev1.Container{{
				VolumeMounts: []corev1.VolumeMount{{Name: "pvc", MountPath: "/data"}},
			}},
		},
	}
	_, _, err := Materialize(Request{Pod: pod, Container: pod.Spec.Containers[0], RootDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected unsupported volume error")
	}
}

func TestMissingVolumeRef(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			VolumeMounts: []corev1.VolumeMount{{Name: "nope", MountPath: "/x"}},
		}},
	}}
	_, _, err := Materialize(Request{Pod: pod, Container: pod.Spec.Containers[0], RootDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSubPathIsTheShareNotAGuestSuffix(t *testing.T) {
	root := t.TempDir()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "ed", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			Containers: []corev1.Container{{
				Name: "macos",
				VolumeMounts: []corev1.VolumeMount{{Name: "ed", MountPath: "/data", SubPath: "cache"}},
			}},
		},
	}
	shares, places, err := Materialize(Request{Pod: pod, Container: pod.Spec.Containers[0], RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 || len(places) != 1 {
		t.Fatalf("shares=%d places=%d", len(shares), len(places))
	}
	if places[0].GuestPath != "/data" {
		t.Fatalf("guest path %q, subPath must not be appended to mountPath", places[0].GuestPath)
	}
	if !strings.HasSuffix(shares[0].HostPath, string(filepath.Separator)+"cache") {
		t.Fatalf("share host path %q is not the subPath directory", shares[0].HostPath)
	}
	// A sibling written at the volume root must not be inside the share.
	if err := os.WriteFile(filepath.Join(root, "ed", "secret"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shares[0].HostPath, "secret")); !os.IsNotExist(err) {
		t.Fatal("subPath share exposed a sibling file")
	}
}

func TestSubPathSymlinkCannotEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := volumeSubPath(root, "escape"); err == nil {
		t.Fatal("symlink subPath must not escape the volume")
	}
	if _, err := volumeSubPath(root, "escape/nested"); err == nil {
		t.Fatal("nested symlink subPath must not escape")
	}
	if _, err := os.Stat(filepath.Join(outside, "nested")); !os.IsNotExist(err) {
		t.Fatal("escape symlink created a directory outside the volume")
	}
}

func TestSecretItemPathCannotEscape(t *testing.T) {
	root := t.TempDir()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "sec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}}},
			Containers: []corev1.Container{{
				Name:         "macos",
				VolumeMounts: []corev1.VolumeMount{{Name: "sec", MountPath: "/etc/sec"}},
			}},
		},
	}
	pod.Spec.Volumes[0].Secret.Items = []corev1.KeyToPath{{Key: "token", Path: "../../escape"}}
	_, _, err := Materialize(Request{
		Pod: pod, Container: pod.Spec.Containers[0], RootDir: root,
		Secrets: map[string]*corev1.Secret{"s": {Data: map[string][]byte{"token": []byte("shh")}}},
	})
	if err == nil {
		t.Fatal("expected path escape error")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "escape")); !os.IsNotExist(statErr) {
		t.Fatal("secret bytes escaped the volume directory")
	}
}

func TestDownwardResourceFieldRef(t *testing.T) {
	root := t.TempDir()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "down", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{
				Items: []corev1.DownwardAPIVolumeFile{
					{Path: "cpu", ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}},
					{Path: "mem", ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "requests.memory", Divisor: resource.MustParse("1Mi")}},
				},
			}}}},
			Containers: []corev1.Container{{
				Name: "macos",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"),
				}, Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				}},
				VolumeMounts: []corev1.VolumeMount{{Name: "down", MountPath: "/etc/down"}},
			}},
		},
	}
	_, _, err := Materialize(Request{Pod: pod, Container: pod.Spec.Containers[0], RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	cpu, err := os.ReadFile(filepath.Join(root, "down", "cpu"))
	if err != nil {
		t.Fatal(err)
	}
	// 500m / 1 core = 0.5, kubelet ceiling is 1.
	if string(cpu) != "1" {
		t.Fatalf("cpu file %q", cpu)
	}
	mem, err := os.ReadFile(filepath.Join(root, "down", "mem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mem) != "64" {
		t.Fatalf("mem file %q", mem)
	}
}

func TestSubPathExprRejected(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "ed", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			Containers: []corev1.Container{{
				VolumeMounts: []corev1.VolumeMount{{Name: "ed", MountPath: "/data", SubPathExpr: "$(POD_NAME)"}},
			}},
		},
	}
	if _, _, err := Materialize(Request{Pod: pod, Container: pod.Spec.Containers[0], RootDir: t.TempDir()}); err == nil {
		t.Fatal("subPathExpr must fail closed")
	}
}
