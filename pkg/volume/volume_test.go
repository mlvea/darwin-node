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
				Name:         "macos",
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

func TestEmptyDirUsageCountsFilesAndRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "b"), []byte("xy"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Directory metadata is not guest payload. A symlink that stays inside
	// the volume must not be counted twice.
	if err := os.Symlink("a", filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	got, err := DirUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != 6 {
		t.Fatalf("usage %d, want 6", got)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := DirUsage(root); err == nil {
		t.Fatal("symlink that leaves the volume must fail closed")
	}
}

func TestEmptyDirLimitsFromPodSpec(t *testing.T) {
	limit := resource.MustParse("4")
	mem := resource.MustParse("1Ki")
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}}},
				{Name: "mem", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &mem}}},
				{Name: "plain", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "unmounted", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}}},
			},
			Containers: []corev1.Container{{
				Name: "macos",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "scratch", MountPath: "/scratch"},
					{Name: "mem", MountPath: "/mem"},
					{Name: "plain", MountPath: "/plain"},
				},
			}},
		},
	}
	limits, err := EmptyDirLimits(pod, "/cache/pods/uid/volumes")
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 {
		t.Fatalf("limits %+v", limits)
	}
	if limits[0].Name != "scratch" || limits[0].Bytes != 4 || limits[0].Path != "/cache/pods/uid/volumes/scratch" {
		t.Fatalf("scratch %+v", limits[0])
	}
	if limits[1].Name != "mem" || limits[1].Bytes != 1024 {
		t.Fatalf("memory medium is still a directory with a limit: %+v", limits[1])
	}
}

func TestProjectedTokenRequiresDistinctNonEmptyTokens(t *testing.T) {
	root := t.TempDir()
	pod := projectedPod()
	_, _, err := Materialize(Request{Pod: pod, Container: pod.Spec.Containers[0], RootDir: root})
	if err == nil {
		t.Fatal("missing service account token must fail closed")
	}
	if _, statErr := os.Stat(filepath.Join(root, "proj", "token")); !os.IsNotExist(statErr) {
		t.Fatal("refused token must not leave a file")
	}

	_, _, err = Materialize(Request{
		Pod: pod, Container: pod.Spec.Containers[0], RootDir: root,
		ServiceToken: "shared",
	})
	if err == nil {
		t.Fatal("one token must not be written to two projected paths")
	}

	_, _, err = Materialize(Request{
		Pod: pod, Container: pod.Spec.Containers[0], RootDir: t.TempDir(),
		Tokens: []ProjectedToken{
			{Volume: "proj", Path: "token", Token: "audience-a"},
			{Volume: "proj", Path: "extra", Token: ""},
		},
	})
	if err == nil {
		t.Fatal("empty projected token must fail closed")
	}

	dir := t.TempDir()
	_, _, err = Materialize(Request{
		Pod: pod, Container: pod.Spec.Containers[0], RootDir: dir,
		Tokens: []ProjectedToken{
			{Volume: "proj", Path: "token", Token: "audience-a"},
			{Volume: "proj", Path: "extra", Token: "audience-b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.ReadFile(filepath.Join(dir, "proj", "token"))
	if err != nil || string(a) != "audience-a" {
		t.Fatalf("token file %q %v", a, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "proj", "extra"))
	if err != nil || string(b) != "audience-b" {
		t.Fatalf("extra file %q %v", b, err)
	}
	info, err := os.Stat(filepath.Join(dir, "proj", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %o", info.Mode().Perm())
	}
}

func projectedPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "proj",
				VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", Audience: "a"}},
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "extra", Audience: "b"}},
				}}},
			}},
			Containers: []corev1.Container{{
				Name:         "macos",
				VolumeMounts: []corev1.VolumeMount{{Name: "proj", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"}},
			}},
		},
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
