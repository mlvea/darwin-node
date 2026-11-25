package sidecar

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/image"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/registry"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDockerLogsTailZeroMeansAll(t *testing.T) {
	cli := &logDocker{}
	d := &Docker{cli: cli, ids: map[string]map[string]string{"ns/p": {"log": "abc"}}}
	rc, err := d.Logs(context.Background(), "ns", "p", "log", api.ContainerLogOpts{})
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if cli.opts.Tail != "all" {
		t.Fatalf("tail %q, an unset tail was sent as 0 and kubectl logs is empty", cli.opts.Tail)
	}
	rc, err = d.Logs(context.Background(), "ns", "p", "log", api.ContainerLogOpts{Tail: 10})
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if cli.opts.Tail != "10" {
		t.Fatalf("tail %q", cli.opts.Tail)
	}
}

type logDocker struct {
	scriptedDocker
	opts container.LogsOptions
}

func (f *logDocker) ContainerLogs(_ context.Context, _ string, opts container.LogsOptions) (io.ReadCloser, error) {
	f.opts = opts
	return io.NopCloser(strings.NewReader("a\nb\n")), nil
}

func TestDockerExecReturnsWhenContextEnds(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	cli := &execDocker{block: client, attached: make(chan struct{})}
	d := &Docker{cli: cli, ids: map[string]map[string]string{"ns/p": {"log": "abc"}}}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Exec(ctx, "ns", "p", "log", []string{"sleep"}, nil)
	}()
	select {
	case <-cli.attached:
	case <-time.After(2 * time.Second):
		t.Fatal("exec did not attach")
	}
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled exec returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("exec stream copy ignored context; a sidecar probe would hold pod delete and the VM slot")
	}
}

type stdinAttach struct {
	bufAttach
	in io.Reader
}

func (a *stdinAttach) Stdin() io.Reader { return a.in }

func TestDockerExecCopiesStdin(t *testing.T) {
	server, client := net.Pipe()
	cli := &execDocker{stdinConn: client, stdout: "ok\n", attached: make(chan struct{})}
	d := &Docker{cli: cli, ids: map[string]map[string]string{"ns/p": {"log": "abc"}}}
	var got bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&got, server)
		close(done)
	}()
	err := d.Exec(context.Background(), "ns", "p", "log", []string{"cat"}, &stdinAttach{in: strings.NewReader("hello-stdin")})
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stdin copy did not finish")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "hello-stdin" {
		t.Fatalf("stdin %q, exec did not copy stdin", got.String())
	}
}

func TestDockerResourcesUseLimits(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	c := corev1.Container{
		Name: "log",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}
	hc, err := dockerHostConfig(pod, c, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hc.NanoCPUs != 1_000_000_000 {
		t.Fatalf("nanocpus %d, cpu limit was ignored", hc.NanoCPUs)
	}
	if hc.Memory != 128<<20 {
		t.Fatalf("memory %d, memory limit was ignored", hc.Memory)
	}
	if hc.MemoryReservation != 64<<20 {
		t.Fatalf("reservation %d, memory request was dropped", hc.MemoryReservation)
	}
	if hc.CPUShares != 256 {
		t.Fatalf("shares %d", hc.CPUShares)
	}
}

func TestDockerCreateRejectsPrivilegedAndHostNamespaces(t *testing.T) {
	priv := true
	nonRoot := true
	cases := []struct {
		name string
		pod  *corev1.Pod
		c    corev1.Container
		want string
	}{
		{
			name: "privileged",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-1"}},
			c: corev1.Container{Name: "log", Image: "busybox", ImagePullPolicy: corev1.PullNever, SecurityContext: &corev1.SecurityContext{
				Privileged: &priv,
			}},
			want: "privileged",
		},
		{
			name: "hostNetwork",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-1"}, Spec: corev1.PodSpec{HostNetwork: true}},
			c:    corev1.Container{Name: "log", Image: "busybox", ImagePullPolicy: corev1.PullNever},
			want: "hostNetwork",
		},
		{
			name: "hostPID",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-1"}, Spec: corev1.PodSpec{HostPID: true}},
			c:    corev1.Container{Name: "log", Image: "busybox", ImagePullPolicy: corev1.PullNever},
			want: "hostPID",
		},
		{
			name: "hostIPC",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-1"}, Spec: corev1.PodSpec{HostIPC: true}},
			c:    corev1.Container{Name: "log", Image: "busybox", ImagePullPolicy: corev1.PullNever},
			want: "hostIPC",
		},
		{
			name: "added cap",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-1"}},
			c: corev1.Container{Name: "log", Image: "busybox", ImagePullPolicy: corev1.PullNever, SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}},
			}},
			want: "capabilities",
		},
		{
			name: "docker.sock",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-1"},
				Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
					Name: "sock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}},
				}}},
			},
			c: corev1.Container{Name: "log", Image: "busybox", ImagePullPolicy: corev1.PullNever, VolumeMounts: []corev1.VolumeMount{{
				Name: "sock", MountPath: "/var/run/docker.sock",
			}}},
			want: "docker.sock",
		},
		{
			name: "runAsNonRoot without user",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-1"}},
			c: corev1.Container{Name: "log", Image: "busybox", ImagePullPolicy: corev1.PullNever, SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot: &nonRoot,
			}},
			want: "runAsNonRoot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &scriptedDocker{}
			d := &Docker{cli: cli, ids: map[string]map[string]string{}}
			err := d.Create(context.Background(), tc.pod, tc.c, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if cli.creates != 0 {
				t.Fatalf("rejected sidecar was still created (%d)", cli.creates)
			}
		})
	}
}

func TestDockerCreateIsolatesSidecar(t *testing.T) {
	uid := int64(1000)
	gid := int64(2000)
	ro := true
	nonRoot := true
	cli := &scriptedDocker{}
	d := &Docker{cli: cli, ids: map[string]map[string]string{}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-isolate"}}
	c := corev1.Container{
		Name:            "log",
		Image:           "busybox",
		ImagePullPolicy: corev1.PullNever,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:              &uid,
			RunAsGroup:             &gid,
			RunAsNonRoot:           &nonRoot,
			ReadOnlyRootFilesystem: &ro,
			Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if err := d.Create(context.Background(), pod, c, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cli.lastHost == nil {
		t.Fatal("no host config")
	}
	if cli.lastHost.NetworkMode == "" || cli.lastHost.NetworkMode == "default" || cli.lastHost.NetworkMode == "bridge" || cli.lastHost.NetworkMode == "host" {
		t.Fatalf("network %q, sidecar stayed on the default bridge", cli.lastHost.NetworkMode)
	}
	if len(cli.netCreates) != 1 || string(cli.lastHost.NetworkMode) != cli.netCreates[0] {
		t.Fatalf("networks %v mode %q", cli.netCreates, cli.lastHost.NetworkMode)
	}
	if cli.lastCfg == nil || cli.lastCfg.User != "1000:2000" {
		t.Fatalf("user %v", cli.lastCfg)
	}
	if !cli.lastHost.ReadonlyRootfs {
		t.Fatal("readOnlyRootFilesystem was ignored")
	}
	if !containsString([]string(cli.lastHost.CapDrop), "ALL") {
		t.Fatalf("cap drop %v", cli.lastHost.CapDrop)
	}
	if !containsString(cli.lastHost.SecurityOpt, "no-new-privileges:true") {
		t.Fatalf("security opt %v", cli.lastHost.SecurityOpt)
	}
	if cli.lastHost.Privileged {
		t.Fatal("privileged defaulted on")
	}
	if cli.lastNet == nil || cli.lastNet.EndpointsConfig[string(cli.lastHost.NetworkMode)] == nil {
		t.Fatalf("container was not attached to %s: %+v", cli.lastHost.NetworkMode, cli.lastNet)
	}
	c2 := c
	c2.Name = "log2"
	if err := d.Create(context.Background(), pod, c2, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(cli.netCreates) != 1 {
		t.Fatalf("second container created another network: %v", cli.netCreates)
	}
	if err := d.RemovePod(context.Background(), "ns", "p", 0); err != nil {
		t.Fatal(err)
	}
	if len(cli.netRemoves) != 1 {
		t.Fatalf("network removes %v", cli.netRemoves)
	}

	pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "q", UID: "uid-other"}}
	cli2 := &scriptedDocker{}
	d2 := &Docker{cli: cli2, ids: map[string]map[string]string{}}
	if err := d2.Create(context.Background(), pod2, c, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(cli2.netCreates) != 1 || cli2.netCreates[0] == cli.netCreates[0] {
		t.Fatalf("pods share a network: %v and %v", cli.netCreates, cli2.netCreates)
	}
}

func TestHostPathBindResolvesSymlinkInsideAllowlist(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "hp", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: link}},
		}}},
	}
	c := corev1.Container{Name: "log", VolumeMounts: []corev1.VolumeMount{{Name: "hp", MountPath: "/host"}}}
	binds, err := volumeBinds(pod, c, t.TempDir(), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range binds {
		src := strings.SplitN(b, ":", 2)[0]
		if src == link {
			t.Fatalf("docker received the symlink %s; it resolves the target after the allowlist check", link)
		}
		if src == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("binds %v, resolved path %s was not the mount source", binds, want)
	}
}

func TestHostPathBindRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "safe")
	outside := filepath.Join(root, "secret")
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "hp", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: link}},
		}}},
	}
	c := corev1.Container{Name: "log", VolumeMounts: []corev1.VolumeMount{{Name: "hp", MountPath: "/host"}}}
	binds, err := volumeBinds(pod, c, t.TempDir(), []string{allowed})
	if err == nil {
		t.Fatalf("escaped symlink was mounted: %v", binds)
	}
	for _, b := range binds {
		src := strings.SplitN(b, ":", 2)[0]
		if src == link || src == outside || strings.HasPrefix(src, outside+string(os.PathSeparator)) {
			t.Fatalf("bind %s leaves the allowlist via %s", b, link)
		}
	}
	if len(binds) != 0 {
		t.Fatalf("escaped symlink was mounted: %v", binds)
	}
}

func TestEnvValueFromIsNotDropped(t *testing.T) {
	opt := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid-1", Labels: map[string]string{"app": "side"}},
		Spec:       corev1.PodSpec{ServiceAccountName: "sa", NodeName: "mac"},
		Status:     corev1.PodStatus{PodIP: "10.1.1.1", HostIP: "10.0.0.8"},
	}
	c := corev1.Container{
		Name: "log",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		}},
		Env: []corev1.EnvVar{
			{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "token",
			}}},
			{Name: "CFG", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}, Key: "k",
			}}},
			{Name: "NS", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "SA", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.serviceAccountName"}}},
			{Name: "LBL", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['app']"}}},
			{Name: "CPU", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}},
			{Name: "MEM", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
				Resource: "limits.memory", Divisor: resource.MustParse("1Mi"),
			}}},
			{Name: "MISS", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "gone"}, Key: "token", Optional: &opt,
			}}},
		},
	}
	pod.Spec.Containers = []corev1.Container{c}
	opts := CreateOptions{
		Secrets:    map[string]*corev1.Secret{"s": {Data: map[string][]byte{"token": []byte("s3cret")}}},
		ConfigMaps: map[string]*corev1.ConfigMap{"cm": {Data: map[string]string{"k": "from-cm"}}},
	}
	got, err := resolveEnv(pod, c, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"TOKEN=s3cret", "CFG=from-cm", "NS=ns", "SA=sa", "LBL=side", "CPU=1", "MEM=64", "MISS="}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("env %v", got)
	}
	_, err = resolveEnv(pod, corev1.Container{Name: "log", Env: []corev1.EnvVar{{
		Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "missing",
		}},
	}}}, opts)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err %v", err)
	}
	_, err = resolveEnv(pod, corev1.Container{Name: "log", EnvFrom: []corev1.EnvFromSource{{Prefix: "X"}}}, opts)
	if err == nil || !strings.Contains(err.Error(), "envFrom") {
		t.Fatalf("err %v", err)
	}
}

func TestSubPathIsMountedNotTheVolumeRoot(t *testing.T) {
	volRoot := t.TempDir()
	dir := filepath.Join(volRoot, "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "token")
	if err := os.WriteFile(key, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}},
	}
	c := corev1.Container{Name: "log", VolumeMounts: []corev1.VolumeMount{{
		Name: "data", MountPath: "/run/token", SubPath: "token",
	}}}
	binds, err := volumeBinds(pod, c, volRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 || !strings.HasPrefix(binds[0], want+":") {
		t.Fatalf("binds %v, subPath was ignored and the volume root would be mounted", binds)
	}
}

func TestSubPathSymlinkCannotEscape(t *testing.T) {
	volRoot := t.TempDir()
	dir := filepath.Join(volRoot, "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}},
	}
	c := corev1.Container{Name: "log", VolumeMounts: []corev1.VolumeMount{{
		Name: "data", MountPath: "/run/token", SubPath: "escape/token",
	}}}
	binds, err := volumeBinds(pod, c, volRoot, nil)
	if err == nil {
		t.Fatalf("subPath symlink was mounted: %v", binds)
	}
	c.VolumeMounts[0].SubPath = "../secret"
	c.VolumeMounts[0].SubPathExpr = ""
	if _, err := volumeBinds(pod, c, volRoot, nil); err == nil {
		t.Fatal("subPath .. was mounted")
	}
	c.VolumeMounts[0].SubPath = ""
	c.VolumeMounts[0].SubPathExpr = "$(POD_NAME)"
	if _, err := volumeBinds(pod, c, volRoot, nil); err == nil || !strings.Contains(err.Error(), "subPathExpr") {
		t.Fatalf("err %v", err)
	}
}

func TestHostPathBindRejectsResolvedDockerSock(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	if err := os.WriteFile(sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "sock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: link}},
		}}},
	}
	c := corev1.Container{Name: "log", VolumeMounts: []corev1.VolumeMount{{Name: "sock", MountPath: "/run/sock"}}}
	if _, err := volumeBinds(pod, c, t.TempDir(), []string{dir}); err == nil || !strings.Contains(err.Error(), "docker.sock") {
		t.Fatalf("err %v", err)
	}
}

func TestDockerEndpointUsesInspectAddress(t *testing.T) {
	cli := &scriptedDocker{inspectIP: "172.18.0.4", inspectNet: "darwin-node-uid"}
	d := &Docker{
		cli:  cli,
		ids:  map[string]map[string]string{"ns/p": {"log": "abc"}},
		nets: map[string]string{"ns/p": "darwin-node-uid"},
	}
	ip, err := d.Endpoint(context.Background(), "ns", "p", "log")
	if err != nil || ip != "172.18.0.4" {
		t.Fatalf("ip %q err %v", ip, err)
	}
	cli.mu.Lock()
	cli.inspectIP = "127.0.0.1"
	cli.mu.Unlock()
	if _, err := d.Endpoint(context.Background(), "ns", "p", "log"); err == nil {
		t.Fatal("loopback inspect address was returned")
	}
	cli.mu.Lock()
	cli.inspectIP = "169.254.169.254"
	cli.mu.Unlock()
	if _, err := d.Endpoint(context.Background(), "ns", "p", "log"); err == nil {
		t.Fatal("link-local inspect address was returned")
	}
}

func TestDockerPullUsesMatchingImagePullSecret(t *testing.T) {
	ring, err := image.NewPullCreds([]image.RegistryCreds{
		{Server: "https://index.docker.io/v1/", Username: "hub", Password: "hubpass"},
		{Server: "quay.io", Username: "quser", Password: "qpass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cli := &scriptedDocker{}
	d := &Docker{cli: cli, ids: map[string]map[string]string{}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-pull"}}
	c := corev1.Container{Name: "log", Image: "quay.io/team/side:1", ImagePullPolicy: corev1.PullAlways}
	ctx := WithCreateOptions(context.Background(), CreateOptions{Pull: ring})
	if err := d.Create(ctx, pod, c, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cfg, err := registry.DecodeAuthConfig(cli.pullAuth)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "quser" || cfg.Password != "qpass" {
		t.Fatalf("auth user %q, a credential for another registry was sent", cfg.Username)
	}
	cli.mu.Lock()
	cli.running = false
	cli.failStartID = "new-id"
	cli.mu.Unlock()
	_ = d.Create(context.Background(), pod, c, t.TempDir())
	cli.mu.Lock()
	auths := append([]string(nil), cli.pullAuths...)
	cli.mu.Unlock()
	if len(auths) < 2 {
		t.Fatalf("pulls %v, restart lost the registry credential", auths)
	}
	again, err := registry.DecodeAuthConfig(auths[1])
	if err != nil {
		t.Fatal(err)
	}
	if again.Username != "quser" || again.Password != "qpass" {
		t.Fatalf("restart auth user %q", again.Username)
	}

	quayOnly, err := image.NewPullCreds([]image.RegistryCreds{
		{Server: "quay.io", Username: "quser", Password: "qpass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &scriptedDocker{}
	dh := &Docker{cli: hub, ids: map[string]map[string]string{}}
	c.Image = "busybox:1.36"
	if err := dh.Create(WithCreateOptions(context.Background(), CreateOptions{Pull: quayOnly}), pod, c, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if hub.pullAuth != "" {
		t.Fatalf("docker hub pull sent auth %q", hub.pullAuth)
	}
}

func TestUserSpecGroupOnly(t *testing.T) {
	gid := int64(50)
	got := userString(&corev1.Pod{}, corev1.Container{SecurityContext: &corev1.SecurityContext{RunAsGroup: &gid}})
	if got != ":50" {
		t.Fatalf("user %q", got)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
