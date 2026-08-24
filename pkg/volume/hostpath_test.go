package volume

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func hpType(t corev1.HostPathType) *corev1.HostPathType { return &t }

func hostPathPod(name, path string, t *corev1.HostPathType) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: path, Type: t},
				},
			}},
			Containers: []corev1.Container{{
				Name:         "macos",
				VolumeMounts: []corev1.VolumeMount{{Name: name, MountPath: "/mnt"}},
			}},
		},
	}
}

func materializeHP(t *testing.T, path string, typ *corev1.HostPathType, allowed []string) error {
	t.Helper()
	pod := hostPathPod("hp", path, typ)
	_, _, err := Materialize(Request{
		Pod:              pod,
		Container:        pod.Spec.Containers[0],
		RootDir:          t.TempDir(),
		AllowedHostPaths: allowed,
	})
	return err
}

func TestMaterializeHostPathDefaultDeny(t *testing.T) {
	dir := t.TempDir()
	if err := materializeHP(t, dir, hpType(corev1.HostPathDirectory), nil); err == nil {
		t.Fatal("empty allowlist must deny hostPath")
	}
}

func TestMaterializeHostPathRootDenied(t *testing.T) {
	if err := materializeHP(t, "/", hpType(corev1.HostPathDirectory), []string{"/"}); err == nil {
		t.Fatal("must not mount host root even if / is allowlisted")
	}
	if err := AllowHostPath("/", nil); err == nil {
		t.Fatal("AllowHostPath(/) with empty allowlist")
	}
	if err := AllowHostPath("/", []string{"/"}); err == nil {
		t.Fatal("AllowHostPath(/) must fail")
	}
}

func TestMaterializeHostPathAllowlist(t *testing.T) {
	allowed := t.TempDir()
	target := filepath.Join(allowed, "data")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	pod := hostPathPod("hp", target, hpType(corev1.HostPathDirectory))
	shares, _, err := Materialize(Request{
		Pod:              pod,
		Container:        pod.Spec.Containers[0],
		RootDir:          t.TempDir(),
		AllowedHostPaths: []string{allowed},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 {
		t.Fatalf("shares=%d", len(shares))
	}
	got, err := filepath.EvalSymlinks(shares[0].HostPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("shared %q want %q", got, want)
	}
}

func TestMaterializeHostPathTraversalDenied(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "safe")
	other := filepath.Join(root, "other")
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	sneak := filepath.Join(allowed, "..", "other")
	if err := materializeHP(t, sneak, hpType(corev1.HostPathDirectory), []string{allowed}); err == nil {
		t.Fatal("expected .. traversal to be denied")
	}
}

func TestMaterializeHostPathSymlinkEscapeDenied(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "safe")
	other := filepath.Join(root, "other")
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := materializeHP(t, link, hpType(corev1.HostPathDirectory), []string{allowed}); err == nil {
		t.Fatal("expected symlink escape to be denied")
	}
}

func TestMaterializeHostPathSiblingPrefixDenied(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "work")
	sibling := filepath.Join(root, "workevil")
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := materializeHP(t, sibling, hpType(corev1.HostPathDirectory), []string{allowed}); err == nil {
		t.Fatal("sibling prefix must not match")
	}
}

func TestHostPathTypes(t *testing.T) {
	allowed := t.TempDir()
	dir := filepath.Join(allowed, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(allowed, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{allowed}

	if err := materializeHP(t, file, hpType(corev1.HostPathDirectory), allow); err == nil {
		t.Fatal("Directory type on a file")
	}
	if err := materializeHP(t, dir, hpType(corev1.HostPathFile), allow); err == nil {
		t.Fatal("File type on a directory")
	}
	if err := materializeHP(t, filepath.Join(allowed, "missing"), hpType(corev1.HostPathDirectory), allow); err == nil {
		t.Fatal("Directory type on missing path")
	}
	if err := materializeHP(t, file, hpType(corev1.HostPathSocket), allow); err == nil {
		t.Fatal("Socket type on a file")
	}
	if err := materializeHP(t, file, hpType(corev1.HostPathCharDev), allow); err == nil {
		t.Fatal("CharDevice type on a file")
	}
	if err := materializeHP(t, file, hpType(corev1.HostPathBlockDev), allow); err == nil {
		t.Fatal("BlockDevice type on a file")
	}

	createdFile := filepath.Join(allowed, "created")
	if err := materializeHP(t, createdFile, hpType(corev1.HostPathFileOrCreate), allow); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(createdFile)
	if err != nil || st.IsDir() {
		t.Fatalf("FileOrCreate: %+v %v", st, err)
	}

	createdDir := filepath.Join(allowed, "newdir")
	if err := materializeHP(t, createdDir, hpType(corev1.HostPathDirectoryOrCreate), allow); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(createdDir)
	if err != nil || !st.IsDir() {
		t.Fatalf("DirectoryOrCreate: %+v %v", st, err)
	}

	if err := materializeHP(t, dir, hpType(corev1.HostPathFileOrCreate), allow); err == nil {
		t.Fatal("FileOrCreate on a directory")
	}

	sock := filepath.Join(allowed, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := materializeHP(t, sock, hpType(corev1.HostPathSocket), allow); err != nil {
		t.Fatal(err)
	}
	if err := materializeHP(t, dir, hpType(corev1.HostPathDirectory), allow); err != nil {
		t.Fatal(err)
	}
	if err := materializeHP(t, file, hpType(corev1.HostPathFile), allow); err != nil {
		t.Fatal(err)
	}
}

func TestAllowHostPathEmptyAndRoot(t *testing.T) {
	if err := AllowHostPath("/var/tmp", nil); err == nil {
		t.Fatal("empty allowlist")
	}
	if err := AllowHostPath("", []string{"/tmp"}); err == nil {
		t.Fatal("empty path")
	}
	dir := t.TempDir()
	if err := AllowHostPath(dir, []string{dir}); err != nil {
		t.Fatal(err)
	}
	if err := AllowHostPath(filepath.Join(dir, "child"), []string{dir}); err != nil {
		t.Fatal(err)
	}
	if err := AllowHostPath("/", []string{dir}); err == nil {
		t.Fatal("root must be denied")
	}
}
