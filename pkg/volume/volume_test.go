package volume

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
