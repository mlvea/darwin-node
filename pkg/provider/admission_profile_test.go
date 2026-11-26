package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodProfileRejects(t *testing.T) {
	for _, tc := range profileRejections() {
		t.Run(tc.name, func(t *testing.T) {
			err := decidePodProfile(tc.pod)
			if err == nil {
				t.Fatalf("decidePodProfile returned nil, want %s with message %q", tc.reason, tc.message)
			}
			if err.Reason != tc.reason || err.Message != tc.message {
				t.Fatalf("got %s: %s, want %s: %s", err.Reason, err.Message, tc.reason, tc.message)
			}
		})
	}
}

func TestPodProfileClassifiesEveryVolumeSource(t *testing.T) {
	allowed := map[string]bool{
		"hostPath":    true,
		"emptyDir":    true,
		"secret":      true,
		"configMap":   true,
		"projected":   true,
		"downwardAPI": true,
	}
	rt := reflect.TypeOf(corev1.VolumeSource{})
	if rt.NumField() == 0 {
		t.Fatal("VolumeSource has no fields")
	}
	seen := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.PkgPath != "" || sf.Type.Kind() != reflect.Pointer {
			t.Fatalf("VolumeSource.%s is not an exported pointer", sf.Name)
		}
		name := jsonName(sf)
		seen[name] = true
		pod := profilePod("vol-" + strings.ToLower(name))
		vol := corev1.Volume{Name: "data"}
		reflect.ValueOf(&vol.VolumeSource).Elem().Field(i).Set(reflect.New(sf.Type.Elem()))
		pod.Spec.Volumes = []corev1.Volume{vol}
		err := decidePodProfile(pod)
		if allowed[name] {
			if err != nil {
				t.Fatalf("%s is a supported volume and was rejected: %v", name, err)
			}
			continue
		}
		want := "spec.volumes[data]." + name + " is not supported"
		if err == nil || err.Reason != ReasonUnsupportedVolume || err.Message != want {
			t.Fatalf("%s: got %v, want %s: %s", name, err, ReasonUnsupportedVolume, want)
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Fatalf("VolumeSource no longer has %s; update the supported set", name)
		}
	}
}

func TestPodProfileClassifiesEveryProjectedSource(t *testing.T) {
	allowed := map[string]bool{
		"secret":              true,
		"downwardAPI":         true,
		"configMap":           true,
		"serviceAccountToken": true,
	}
	rt := reflect.TypeOf(corev1.VolumeProjection{})
	seen := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.PkgPath != "" || sf.Type.Kind() != reflect.Pointer {
			t.Fatalf("VolumeProjection.%s is not an exported pointer", sf.Name)
		}
		name := jsonName(sf)
		seen[name] = true
		src := corev1.VolumeProjection{}
		reflect.ValueOf(&src).Elem().Field(i).Set(reflect.New(sf.Type.Elem()))
		pod := profilePod("proj-" + strings.ToLower(name))
		pod.Spec.Volumes = []corev1.Volume{{
			Name: "proj",
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{src},
			}},
		}}
		err := decidePodProfile(pod)
		if allowed[name] {
			if err != nil {
				t.Fatalf("projected %s is supported and was rejected: %v", name, err)
			}
			continue
		}
		want := "spec.volumes[proj].projected.sources[0]." + name + " is not supported"
		if err == nil || err.Reason != ReasonUnsupportedVolume || err.Message != want {
			t.Fatalf("%s: got %v, want %s: %s", name, err, ReasonUnsupportedVolume, want)
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Fatalf("VolumeProjection no longer has %s; update the supported set", name)
		}
	}
}

func TestPodProfileAdmitsSupportedContract(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	def := corev1.DefaultProcMount
	runtimeDefault := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
	}{
		{name: "restart-empty", pod: withRestart(profilePod("restart-empty"), "")},
		{name: "restart-never", pod: withRestart(profilePod("restart-never"), corev1.RestartPolicyNever)},
		{name: "restart-on-failure", pod: withRestart(profilePod("restart-on-failure"), corev1.RestartPolicyOnFailure)},
		{name: "restart-always", pod: withRestart(profilePod("restart-always"), corev1.RestartPolicyAlways)},
		{name: "init-restart-always", pod: func() *corev1.Pod {
			pod := profilePod("init-restart-always")
			pod.Spec.InitContainers = []corev1.Container{{
				Name: "mesh", Image: "busybox:1.36", RestartPolicy: &always,
			}}
			return pod
		}()},
		{name: "host-namespaces-false", pod: func() *corev1.Pod {
			pod := profilePod("host-namespaces-false")
			pod.Spec.HostNetwork = false
			pod.Spec.HostPID = false
			pod.Spec.HostIPC = false
			return pod
		}()},
		{name: "privileged-false", pod: withContainerSC(profilePod("privileged-false"), &corev1.SecurityContext{Privileged: boolPtr(false)})},
		{name: "drop-caps", pod: withContainerSC(profilePod("drop-caps"), &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		})},
		{name: "empty-cap-add", pod: withContainerSC(profilePod("empty-cap-add"), &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{}},
		})},
		{name: "seccomp-pod-runtime-default", pod: func() *corev1.Pod {
			pod := profilePod("seccomp-pod-runtime-default")
			pod.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: runtimeDefault}
			return pod
		}()},
		{name: "seccomp-container-runtime-default", pod: withContainerSC(profilePod("seccomp-container-runtime-default"), &corev1.SecurityContext{SeccompProfile: runtimeDefault})},
		{name: "proc-mount-default", pod: withContainerSC(profilePod("proc-mount-default"), &corev1.SecurityContext{ProcMount: &def})},
		{name: "read-only-root", pod: withContainerSC(profilePod("read-only-root"), &corev1.SecurityContext{ReadOnlyRootFilesystem: boolPtr(true)})},
		{name: "empty-dir", pod: withVolume(profilePod("empty-dir"), "scratch", corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}})},
		{name: "secret", pod: withVolume(profilePod("secret"), "sec", corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}})},
		{name: "config-map", pod: withVolume(profilePod("config-map"), "cfg", corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "c"}}})},
		{name: "downward-api", pod: withVolume(profilePod("downward-api"), "down", corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{}})},
		{name: "host-path-type", pod: withVolume(profilePod("host-path-type"), "hp", corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data"}})},
		{name: "projected-sources", pod: withVolume(profilePod("projected-sources"), "proj", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
			{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}}},
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "c"}}},
			{DownwardAPI: &corev1.DownwardAPIProjection{}},
		}}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := decidePodProfile(tc.pod); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPodProfileReportsHostNamespaceFirst(t *testing.T) {
	pod := profilePod("order")
	pod.Spec.HostNetwork = true
	pod.Spec.HostPID = true
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{SELinuxOptions: &corev1.SELinuxOptions{}}
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: boolPtr(true)}
	pod.Spec.Containers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "d", DevicePath: "/dev/d"}}
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}},
	}}
	err := decidePodProfile(pod)
	want := "spec.hostNetwork must be false"
	if err == nil || err.Reason != ReasonUnsupportedPodField || err.Message != want {
		t.Fatalf("got %v, want %s: %s", err, ReasonUnsupportedPodField, want)
	}
}

func TestPodProfileNilPod(t *testing.T) {
	if err := decidePodProfile(nil); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePodRejectsPodProfile(t *testing.T) {
	env := newAdmitEnv(t, nil)
	for _, tc := range profileRejections() {
		t.Run(tc.name, func(t *testing.T) {
			assertRejected(t, env, tc.pod, tc.reason)
			st, err := env.p.GetPodStatus(context.Background(), tc.pod.Namespace, tc.pod.Name)
			if err != nil {
				t.Fatal(err)
			}
			if st.Message != tc.message {
				t.Fatalf("status message %q, want %q", st.Message, tc.message)
			}
		})
	}
	evs, err := env.kube.CoreV1().Events("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	saw := map[string]bool{}
	for _, ev := range evs.Items {
		if ev.Type == corev1.EventTypeWarning && ev.InvolvedObject.Kind == "Pod" {
			saw[ev.Reason] = true
		}
	}
	for _, reason := range []string{ReasonUnsupportedPodField, ReasonUnsupportedVolume} {
		if !saw[reason] {
			t.Fatalf("kubernetes events missing Warning %s: %+v", reason, evs.Items)
		}
	}
}

func TestPodProfileDoesNotLoosenHostPathAllowlist(t *testing.T) {
	env := newAdmitEnv(t, nil)
	pod := withVolume(profilePod("host-root"), "hp", corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: "/"},
	})
	if err := decidePodProfile(pod); err != nil {
		t.Fatalf("hostPath is a supported volume type; the allowlist stays in the engine: %v", err)
	}
	before := env.rt.Starts()
	err := env.p.CreatePod(context.Background(), pod)
	if err == nil {
		t.Fatal("hostPath / was created; the allowlist was loosened")
	}
	if !strings.Contains(err.Error(), "hostPath") && !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("CreatePod err = %v, want the hostPath allowlist", err)
	}
	if env.slots.Used() != 0 {
		t.Fatalf("slots used %d", env.slots.Used())
	}
	if env.rt.Starts() != before {
		t.Fatalf("runtime starts %d -> %d", before, env.rt.Starts())
	}
}

type profileRejection struct {
	name    string
	pod     *corev1.Pod
	reason  string
	message string
}

func profileRejections() []profileRejection {
	out := []profileRejection{
		hostRejection("host-network", func(pod *corev1.Pod) { pod.Spec.HostNetwork = true }, "spec.hostNetwork must be false"),
		hostRejection("host-pid", func(pod *corev1.Pod) { pod.Spec.HostPID = true }, "spec.hostPID must be false"),
		hostRejection("host-ipc", func(pod *corev1.Pod) { pod.Spec.HostIPC = true }, "spec.hostIPC must be false"),
		podFieldRejection("seccomp-pod-unconfined", &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		}, "spec.securityContext.seccompProfile must be unset or RuntimeDefault (got \"Unconfined\")"),
		podFieldRejection("seccomp-pod-localhost", &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost, LocalhostProfile: strPtr("local.json")},
		}, "spec.securityContext.seccompProfile must be unset or RuntimeDefault (got \"Localhost\")"),
		podFieldRejection("seccomp-pod-empty", &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{},
		}, "spec.securityContext.seccompProfile must be unset or RuntimeDefault (got \"\")"),
		podFieldRejection("apparmor-pod", &corev1.PodSecurityContext{
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault},
		}, "spec.securityContext.appArmorProfile is not supported"),
		podFieldRejection("selinux-pod", &corev1.PodSecurityContext{
			SELinuxOptions: &corev1.SELinuxOptions{},
		}, "spec.securityContext.seLinuxOptions is not supported"),
	}
	kinds := []struct {
		name   string
		prefix string
		apply  func(*corev1.Pod, *corev1.SecurityContext, []corev1.VolumeDevice)
	}{
		{name: "container", prefix: "spec.containers[macos]", apply: func(pod *corev1.Pod, sc *corev1.SecurityContext, dev []corev1.VolumeDevice) {
			pod.Spec.Containers[0].SecurityContext = sc
			pod.Spec.Containers[0].VolumeDevices = dev
		}},
		{name: "init", prefix: "spec.initContainers[prep]", apply: func(pod *corev1.Pod, sc *corev1.SecurityContext, dev []corev1.VolumeDevice) {
			pod.Spec.InitContainers = []corev1.Container{{
				Name: "prep", Image: "busybox:1.36", SecurityContext: sc, VolumeDevices: dev,
			}}
		}},
		{name: "ephemeral", prefix: "spec.ephemeralContainers[dbg]", apply: func(pod *corev1.Pod, sc *corev1.SecurityContext, dev []corev1.VolumeDevice) {
			pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "dbg", Image: "busybox:1.36", SecurityContext: sc, VolumeDevices: dev,
				},
			}}
		}},
	}
	rules := []struct {
		name    string
		reason  string
		suffix  string
		detail  string
		sc      *corev1.SecurityContext
		devices bool
	}{
		{name: "privileged", reason: ReasonUnsupportedPodField, suffix: "securityContext.privileged", detail: "must be false", sc: &corev1.SecurityContext{Privileged: boolPtr(true)}},
		{name: "caps-add", reason: ReasonUnsupportedPodField, suffix: "securityContext.capabilities.add", detail: "is not supported", sc: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN", "SYS_ADMIN"}}}},
		{name: "seccomp-unconfined", reason: ReasonUnsupportedPodField, suffix: "securityContext.seccompProfile", detail: "must be unset or RuntimeDefault (got \"Unconfined\")", sc: &corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}},
		{name: "seccomp-localhost", reason: ReasonUnsupportedPodField, suffix: "securityContext.seccompProfile", detail: "must be unset or RuntimeDefault (got \"Localhost\")", sc: &corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost}}},
		{name: "seccomp-empty", reason: ReasonUnsupportedPodField, suffix: "securityContext.seccompProfile", detail: "must be unset or RuntimeDefault (got \"\")", sc: &corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{}}},
		{name: "apparmor", reason: ReasonUnsupportedPodField, suffix: "securityContext.appArmorProfile", detail: "is not supported", sc: &corev1.SecurityContext{AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}}},
		{name: "selinux", reason: ReasonUnsupportedPodField, suffix: "securityContext.seLinuxOptions", detail: "is not supported", sc: &corev1.SecurityContext{SELinuxOptions: &corev1.SELinuxOptions{}}},
		{name: "procmount-unmasked", reason: ReasonUnsupportedPodField, suffix: "securityContext.procMount", detail: "must not be Unmasked", sc: &corev1.SecurityContext{ProcMount: procMountPtr(corev1.UnmaskedProcMount)}},
		{name: "volume-devices", reason: ReasonUnsupportedVolume, suffix: "volumeDevices", detail: "is not supported", devices: true},
	}
	for _, kind := range kinds {
		for _, rule := range rules {
			pod := profilePod(kind.name + "-" + rule.name)
			var dev []corev1.VolumeDevice
			if rule.devices {
				dev = []corev1.VolumeDevice{{Name: "disk", DevicePath: "/dev/disk"}}
			}
			kind.apply(pod, rule.sc, dev)
			out = append(out, profileRejection{
				name:    kind.name + "-" + rule.name,
				pod:     pod,
				reason:  rule.reason,
				message: kind.prefix + "." + rule.suffix + " " + rule.detail,
			})
		}
	}
	out = append(out,
		volumeRejection("volume-pvc", "data", corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "claim"}}, "spec.volumes[data].persistentVolumeClaim is not supported"),
		volumeRejection("volume-csi", "data", corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "disk.csi.example"}}, "spec.volumes[data].csi is not supported"),
		volumeRejection("volume-ephemeral", "data", corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}, "spec.volumes[data].ephemeral is not supported"),
		volumeRejection("volume-image", "data", corev1.VolumeSource{Image: &corev1.ImageVolumeSource{Reference: "example.com/rootfs:1"}}, "spec.volumes[data].image is not supported"),
		volumeRejection("volume-empty", "data", corev1.VolumeSource{}, "spec.volumes[data] has no supported volume source"),
		volumeRejection("projected-trust", "proj", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
			ClusterTrustBundle: &corev1.ClusterTrustBundleProjection{},
		}}}}, "spec.volumes[proj].projected.sources[0].clusterTrustBundle is not supported"),
		volumeRejection("projected-cert", "proj", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
			PodCertificate: &corev1.PodCertificateProjection{SignerName: "example.com/signer"},
		}}}}, "spec.volumes[proj].projected.sources[0].podCertificate is not supported"),
		volumeRejection("projected-empty-source", "proj", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{},
		}}}, "spec.volumes[proj].projected.sources[0] has no supported projection"),
		volumeRejection("projected-secret-and-trust", "proj", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
			Secret:             &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}},
			ClusterTrustBundle: &corev1.ClusterTrustBundleProjection{},
		}}}}, "spec.volumes[proj].projected.sources[0].clusterTrustBundle is not supported"),
	)
	return out
}

func hostRejection(name string, mutate func(*corev1.Pod), message string) profileRejection {
	pod := profilePod(name)
	mutate(pod)
	return profileRejection{name: name, pod: pod, reason: ReasonUnsupportedPodField, message: message}
}

func podFieldRejection(name string, sc *corev1.PodSecurityContext, message string) profileRejection {
	pod := profilePod(name)
	pod.Spec.SecurityContext = sc
	return profileRejection{name: name, pod: pod, reason: ReasonUnsupportedPodField, message: message}
}

func volumeRejection(name, vol string, src corev1.VolumeSource, message string) profileRejection {
	return profileRejection{
		name:    name,
		pod:     withVolume(profilePod(name), vol, src),
		reason:  ReasonUnsupportedVolume,
		message: message,
	}
}

func profilePod(name string) *corev1.Pod {
	return testPod(name, corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyAlways,
		Containers:    []corev1.Container{{Name: "macos", Image: "local/macos:test"}},
	})
}

func withRestart(pod *corev1.Pod, policy corev1.RestartPolicy) *corev1.Pod {
	pod.Spec.RestartPolicy = policy
	return pod
}

func withContainerSC(pod *corev1.Pod, sc *corev1.SecurityContext) *corev1.Pod {
	pod.Spec.Containers[0].SecurityContext = sc
	return pod
}

func withVolume(pod *corev1.Pod, name string, src corev1.VolumeSource) *corev1.Pod {
	pod.Spec.Volumes = []corev1.Volume{{Name: name, VolumeSource: src}}
	return pod
}

func boolPtr(v bool) *bool { return &v }

func procMountPtr(v corev1.ProcMountType) *corev1.ProcMountType { return &v }

func jsonName(sf reflect.StructField) string {
	name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return sf.Name
	}
	return name
}
