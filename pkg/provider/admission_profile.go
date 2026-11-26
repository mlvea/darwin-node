package provider

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// ReasonUnsupportedPodField is the event and pod-status reason when a
	// pod field cannot be honored on a macOS VM.
	ReasonUnsupportedPodField = "UnsupportedPodField"
	// ReasonUnsupportedVolume is the event and pod-status reason when a
	// volume source is not one this node materializes.
	ReasonUnsupportedVolume = "UnsupportedVolume"
)

// allowedVolumeSources are the volume types pkg/volume materializes.
// hostPath is one of them. The prefix allowlist stays in the engine and
// is not applied or widened here. Every other VolumeSource, including
// persistentVolumeClaim, csi, ephemeral, and image, is rejected.
// clusterTrustBundle and podCertificate projections are not written.
// Rejecting them keeps those files from being skipped.
var allowedVolumeSources = map[string]struct{}{
	"hostPath":    {},
	"emptyDir":    {},
	"secret":      {},
	"configMap":   {},
	"projected":   {},
	"downwardAPI": {},
}

var allowedProjections = map[string]struct{}{
	"secret":              {},
	"downwardAPI":         {},
	"configMap":           {},
	"serviceAccountToken": {},
}

// decidePodProfile rejects pod fields and volumes a macOS VM pod cannot
// run. Nil means the profile is supported. restartPolicy is not inspected:
// Never, OnFailure, and Always are all admitted. Always restarts the guest
// process inside the one VM.
func decidePodProfile(pod *corev1.Pod) *admissionError {
	if pod == nil {
		return nil
	}
	if pod.Spec.HostNetwork {
		return podField("spec.hostNetwork", "must be false")
	}
	if pod.Spec.HostPID {
		return podField("spec.hostPID", "must be false")
	}
	if pod.Spec.HostIPC {
		return podField("spec.hostIPC", "must be false")
	}
	if err := podSecurityProblem(pod.Spec.SecurityContext); err != nil {
		return err
	}
	for i, c := range pod.Spec.Containers {
		if err := containerProblem("containers", c.Name, i, c.SecurityContext, c.VolumeDevices); err != nil {
			return err
		}
	}
	for i, c := range pod.Spec.InitContainers {
		if err := containerProblem("initContainers", c.Name, i, c.SecurityContext, c.VolumeDevices); err != nil {
			return err
		}
	}
	for i, c := range pod.Spec.EphemeralContainers {
		if err := containerProblem("ephemeralContainers", c.Name, i, c.SecurityContext, c.VolumeDevices); err != nil {
			return err
		}
	}
	for i, v := range pod.Spec.Volumes {
		if err := volumeProblem(i, v); err != nil {
			return err
		}
	}
	return nil
}

func podSecurityProblem(sc *corev1.PodSecurityContext) *admissionError {
	if sc == nil {
		return nil
	}
	if err := seccompProblem("spec.securityContext.seccompProfile", sc.SeccompProfile); err != nil {
		return err
	}
	if sc.AppArmorProfile != nil {
		return podField("spec.securityContext.appArmorProfile", "is not supported")
	}
	if sc.SELinuxOptions != nil {
		return podField("spec.securityContext.seLinuxOptions", "is not supported")
	}
	return nil
}

func containerProblem(group, name string, index int, sc *corev1.SecurityContext, devices []corev1.VolumeDevice) *admissionError {
	path := containerPath(group, name, index)
	if err := securityContextProblem(path, sc); err != nil {
		return err
	}
	if len(devices) > 0 {
		return volField(path+".volumeDevices", "is not supported")
	}
	return nil
}

func securityContextProblem(path string, sc *corev1.SecurityContext) *admissionError {
	if sc == nil {
		return nil
	}
	if sc.Privileged != nil && *sc.Privileged {
		return podField(path+".securityContext.privileged", "must be false")
	}
	if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
		return podField(path+".securityContext.capabilities.add", "is not supported")
	}
	if err := seccompProblem(path+".securityContext.seccompProfile", sc.SeccompProfile); err != nil {
		return err
	}
	if sc.AppArmorProfile != nil {
		return podField(path+".securityContext.appArmorProfile", "is not supported")
	}
	if sc.SELinuxOptions != nil {
		return podField(path+".securityContext.seLinuxOptions", "is not supported")
	}
	if sc.ProcMount != nil && *sc.ProcMount == corev1.UnmaskedProcMount {
		return podField(path+".securityContext.procMount", "must not be Unmasked")
	}
	return nil
}

func seccompProblem(path string, profile *corev1.SeccompProfile) *admissionError {
	if profile == nil || profile.Type == corev1.SeccompProfileTypeRuntimeDefault {
		return nil
	}
	return podField(path, fmt.Sprintf("must be unset or RuntimeDefault (got %q)", profile.Type))
}

func volumeProblem(index int, v corev1.Volume) *admissionError {
	base := "spec.volumes[" + volumeID(v.Name, index) + "]"
	fields := setJSONFields(v.VolumeSource)
	if len(fields) == 0 {
		return volField(base, "has no supported volume source")
	}
	if err := rejectDisallowed(base, fields, allowedVolumeSources); err != nil {
		return err
	}
	if v.Projected != nil {
		return projectedProblem(base, v.Projected)
	}
	return nil
}

func projectedProblem(base string, proj *corev1.ProjectedVolumeSource) *admissionError {
	if proj == nil {
		return nil
	}
	for i, src := range proj.Sources {
		prefix := fmt.Sprintf("%s.projected.sources[%d]", base, i)
		fields := setJSONFields(src)
		if len(fields) == 0 {
			return volField(prefix, "has no supported projection")
		}
		if err := rejectDisallowed(prefix, fields, allowedProjections); err != nil {
			return err
		}
	}
	return nil
}

func rejectDisallowed(base string, fields []string, allowed map[string]struct{}) *admissionError {
	for _, name := range fields {
		if _, ok := allowed[name]; !ok {
			return volField(base+"."+name, "is not supported")
		}
	}
	return nil
}

func containerPath(group, name string, index int) string {
	return "spec." + group + "[" + volumeID(name, index) + "]"
}

func volumeID(name string, index int) string {
	if name != "" {
		return name
	}
	return strconv.Itoa(index)
}

func podField(path, detail string) *admissionError {
	return &admissionError{Reason: ReasonUnsupportedPodField, Message: path + " " + detail}
}

func volField(path, detail string) *admissionError {
	return &admissionError{Reason: ReasonUnsupportedVolume, Message: path + " " + detail}
}

// setJSONFields returns the json names of fields that are set, in struct
// order. A later Kubernetes API that adds a volume type is rejected until
// it is added to the allow list.
func setJSONFields(v any) []string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	rt := rv.Type()
	var names []string
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.PkgPath != "" || !fieldSet(rv.Field(i)) {
			continue
		}
		names = append(names, jsonFieldName(sf))
	}
	return names
}

func fieldSet(fv reflect.Value) bool {
	switch fv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return !fv.IsNil()
	default:
		return !fv.IsZero()
	}
}

func jsonFieldName(sf reflect.StructField) string {
	name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return sf.Name
	}
	return name
}
