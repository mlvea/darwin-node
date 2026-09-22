// Package volume materializes Kubernetes volumes on the host for virtio-fs.
package volume

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const dirPerm os.FileMode = 0o700
const filePerm os.FileMode = 0o644
const secretPerm os.FileMode = 0o600

// Placement is a host directory that will become a virtio-fs share.
type Placement struct {
	Name      string
	HostPath  string
	GuestPath string
	ReadOnly  bool
	Mode      string // link | copy
}

// Request is everything needed to materialize a pod's volumes.
type Request struct {
	Pod              *corev1.Pod
	Container        corev1.Container
	RootDir          string // cache/pods/<uid>/volumes
	ConfigMaps       map[string]*corev1.ConfigMap
	Secrets          map[string]*corev1.Secret
	ServiceToken     string
	AllowedHostPaths []string // empty denies all hostPath
}

// Materialize writes volume contents and returns shares + guest placements.
func Materialize(req Request) ([]types.Share, []Placement, error) {
	if err := os.MkdirAll(req.RootDir, dirPerm); err != nil {
		return nil, nil, err
	}
	var shares []types.Share
	var places []Placement
	seen := map[string]bool{}

	for _, mount := range req.Container.VolumeMounts {
		if err := safeVolumeName(mount.Name); err != nil {
			return nil, nil, err
		}
		src := findVolume(req.Pod, mount.Name)
		if src == nil {
			return nil, nil, fmt.Errorf("volume %q referenced by mount but not defined", mount.Name)
		}
		hostPath, readOnly, mode, err := materializeOne(req, mount, src)
		if err != nil {
			return nil, nil, fmt.Errorf("volume %q: %w", mount.Name, err)
		}
		if mount.SubPathExpr != "" {
			return nil, nil, fmt.Errorf("volume %q: subPathExpr is not supported", mount.Name)
		}
		guest, err := cleanMountPath(mount.MountPath)
		if err != nil {
			return nil, nil, fmt.Errorf("volume %q: %w", mount.Name, err)
		}
		shareName := mount.Name
		if mount.SubPath != "" {
			// subPath selects a directory inside the volume. The virtio-fs
			// share must be that directory: sharing the parent and only
			// changing the guest path would expose sibling files.
			sub, err := volumeSubPath(hostPath, mount.SubPath)
			if err != nil {
				return nil, nil, fmt.Errorf("volume %q: %w", mount.Name, err)
			}
			hostPath = sub
			sum := sha256.Sum256([]byte(mount.SubPath))
			shareName = mount.Name + "-" + hex.EncodeToString(sum[:4])
		}
		if mount.ReadOnly {
			readOnly = true
		}
		if !seen[shareName] {
			shares = append(shares, types.Share{
				Name:     shareName,
				HostPath: hostPath,
				ReadOnly: readOnly,
			})
			seen[shareName] = true
		}
		places = append(places, Placement{
			Name:      shareName,
			HostPath:  hostPath,
			GuestPath: guest,
			ReadOnly:  readOnly,
			Mode:      mode,
		})
	}
	return shares, places, nil
}

func findVolume(pod *corev1.Pod, name string) *corev1.VolumeSource {
	if pod == nil {
		return nil
	}
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i].VolumeSource
		}
	}
	return nil
}

func materializeOne(req Request, mount corev1.VolumeMount, src *corev1.VolumeSource) (hostPath string, readOnly bool, mode string, err error) {
	mode = "link"
	switch {
	case src.HostPath != nil:
		hostPath, err = materializeHostPath(src.HostPath, req.AllowedHostPaths)
		if err != nil {
			return "", false, "", err
		}
		return hostPath, mount.ReadOnly, mode, nil

	case src.EmptyDir != nil:
		hostPath = filepath.Join(req.RootDir, mount.Name)
		return hostPath, false, mode, os.MkdirAll(hostPath, 0o755)

	case src.ConfigMap != nil:
		cm := req.ConfigMaps[src.ConfigMap.Name]
		if cm == nil {
			if src.ConfigMap.Optional != nil && *src.ConfigMap.Optional {
				hostPath = filepath.Join(req.RootDir, mount.Name)
				return hostPath, true, "copy", os.MkdirAll(hostPath, dirPerm)
			}
			return "", false, "", fmt.Errorf("configmap %q not found", src.ConfigMap.Name)
		}
		hostPath = filepath.Join(req.RootDir, mount.Name)
		return hostPath, true, "copy", writeConfigMap(hostPath, cm, src.ConfigMap.Items, modeOr(src.ConfigMap.DefaultMode, filePerm))

	case src.Secret != nil:
		sec := req.Secrets[src.Secret.SecretName]
		if sec == nil {
			if src.Secret.Optional != nil && *src.Secret.Optional {
				hostPath = filepath.Join(req.RootDir, mount.Name)
				return hostPath, true, "copy", os.MkdirAll(hostPath, dirPerm)
			}
			return "", false, "", fmt.Errorf("secret %q not found", src.Secret.SecretName)
		}
		hostPath = filepath.Join(req.RootDir, mount.Name)
		return hostPath, true, "copy", writeSecret(hostPath, sec, src.Secret.Items, modeOr(src.Secret.DefaultMode, secretPerm))

	case src.Projected != nil:
		hostPath = filepath.Join(req.RootDir, mount.Name)
		return hostPath, true, "copy", writeProjected(req, hostPath, src.Projected)

	case src.DownwardAPI != nil:
		hostPath = filepath.Join(req.RootDir, mount.Name)
		return hostPath, true, "copy", writeDownward(req.Pod, req.Container.Name, hostPath, src.DownwardAPI.Items)

	default:
		return "", false, "", fmt.Errorf("unsupported volume type (pvc/csi/gitRepo/etc. are not implemented)")
	}
}

func writeConfigMap(dir string, cm *corev1.ConfigMap, items []corev1.KeyToPath, def os.FileMode) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	if len(items) == 0 {
		for k, v := range cm.Data {
			if err := writeVolumeFile(dir, k, []byte(v), def); err != nil {
				return err
			}
		}
		for k, v := range cm.BinaryData {
			if err := writeVolumeFile(dir, k, v, def); err != nil {
				return err
			}
		}
		return nil
	}
	for _, it := range items {
		mode := def
		if it.Mode != nil {
			mode = modeOr(it.Mode, def)
		}
		if v, ok := cm.Data[it.Key]; ok {
			if err := writeVolumeFile(dir, it.Path, []byte(v), mode); err != nil {
				return err
			}
			continue
		}
		if v, ok := cm.BinaryData[it.Key]; ok {
			if err := writeVolumeFile(dir, it.Path, v, mode); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("configmap key %q missing", it.Key)
	}
	return nil
}

func writeSecret(dir string, sec *corev1.Secret, items []corev1.KeyToPath, def os.FileMode) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	if len(items) == 0 {
		for k, v := range sec.Data {
			if err := writeVolumeFile(dir, k, v, def); err != nil {
				return err
			}
		}
		return nil
	}
	for _, it := range items {
		mode := def
		if it.Mode != nil {
			mode = modeOr(it.Mode, def)
		}
		v, ok := sec.Data[it.Key]
		if !ok {
			return fmt.Errorf("secret key %q missing", it.Key)
		}
		if err := writeVolumeFile(dir, it.Path, v, mode); err != nil {
			return err
		}
	}
	return nil
}

func writeProjected(req Request, dir string, proj *corev1.ProjectedVolumeSource) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	for _, s := range proj.Sources {
		switch {
		case s.ServiceAccountToken != nil:
			path := s.ServiceAccountToken.Path
			if path == "" {
				path = "token"
			}
			if err := writeVolumeFile(dir, path, []byte(req.ServiceToken), secretPerm); err != nil {
				return err
			}
		case s.ConfigMap != nil:
			cm := req.ConfigMaps[s.ConfigMap.Name]
			if cm == nil {
				return fmt.Errorf("projected configmap %q not found", s.ConfigMap.Name)
			}
			def := filePerm
			if proj.DefaultMode != nil {
				def = modeOr(proj.DefaultMode, filePerm)
			}
			if err := writeConfigMap(dir, cm, s.ConfigMap.Items, def); err != nil {
				return err
			}
		case s.Secret != nil:
			sec := req.Secrets[s.Secret.Name]
			if sec == nil {
				return fmt.Errorf("projected secret %q not found", s.Secret.Name)
			}
			def := secretPerm
			if proj.DefaultMode != nil {
				def = modeOr(proj.DefaultMode, secretPerm)
			}
			if err := writeSecret(dir, sec, s.Secret.Items, def); err != nil {
				return err
			}
		case s.DownwardAPI != nil:
			if err := writeDownward(req.Pod, req.Container.Name, dir, s.DownwardAPI.Items); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeDownward(pod *corev1.Pod, containerName, dir string, items []corev1.DownwardAPIVolumeFile) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	for _, it := range items {
		var val string
		var err error
		switch {
		case it.FieldRef != nil:
			val, err = fieldPath(pod, it.FieldRef.FieldPath)
		case it.ResourceFieldRef != nil:
			val, err = resourceFileValue(pod, containerName, it.ResourceFieldRef)
		default:
			err = fmt.Errorf("downwardAPI item %q has no fieldRef or resourceFieldRef", it.Path)
		}
		if err != nil {
			return err
		}
		mode := filePerm
		if it.Mode != nil {
			mode = modeOr(it.Mode, filePerm)
		}
		if err := writeVolumeFile(dir, it.Path, []byte(val), mode); err != nil {
			return err
		}
	}
	return nil
}

func resourceFileValue(pod *corev1.Pod, fallback string, ref *corev1.ResourceFieldSelector) (string, error) {
	if ref == nil {
		return "", fmt.Errorf("nil resourceFieldRef")
	}
	name := ref.ContainerName
	if name == "" {
		name = fallback
	}
	c := findContainer(pod, name)
	if c == nil {
		return "", fmt.Errorf("container %q not found for resourceFieldRef", name)
	}
	switch ref.Resource {
	case "limits.cpu":
		return formatQuantity(c.Resources.Limits[corev1.ResourceCPU], ref.Divisor, true)
	case "requests.cpu":
		return formatQuantity(c.Resources.Requests[corev1.ResourceCPU], ref.Divisor, true)
	case "limits.memory":
		return formatQuantity(c.Resources.Limits[corev1.ResourceMemory], ref.Divisor, false)
	case "requests.memory":
		return formatQuantity(c.Resources.Requests[corev1.ResourceMemory], ref.Divisor, false)
	case "limits.ephemeral-storage":
		return formatQuantity(c.Resources.Limits[corev1.ResourceEphemeralStorage], ref.Divisor, false)
	case "requests.ephemeral-storage":
		return formatQuantity(c.Resources.Requests[corev1.ResourceEphemeralStorage], ref.Divisor, false)
	default:
		return "", fmt.Errorf("unsupported resourceFieldRef %q", ref.Resource)
	}
}

// formatQuantity matches kubelet: a zero divisor means 1, and the file
// contains ceil(value/divisor) as a decimal integer. CPU is compared in
// milli-units so 100m / 1 is 1, not 0.
func formatQuantity(q, divisor resource.Quantity, cpu bool) (string, error) {
	if divisor.IsZero() {
		divisor = resource.MustParse("1")
	}
	var n float64
	if cpu {
		d := divisor.MilliValue()
		if d == 0 {
			return "", fmt.Errorf("cpu divisor is zero")
		}
		n = float64(q.MilliValue()) / float64(d)
	} else {
		d := divisor.Value()
		if d == 0 {
			return "", fmt.Errorf("divisor is zero")
		}
		n = float64(q.Value()) / float64(d)
	}
	return strconv.FormatInt(int64(math.Ceil(n)), 10), nil
}

func findContainer(pod *corev1.Pod, name string) *corev1.Container {
	if pod == nil || name == "" {
		return nil
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func modeOr(ptr *int32, def os.FileMode) os.FileMode {
	if ptr == nil {
		return def
	}
	return os.FileMode(*ptr) & os.ModePerm
}

func writeVolumeFile(dir, name string, data []byte, mode os.FileMode) error {
	full, err := safeVolumePath(dir, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), dirPerm); err != nil {
		return err
	}
	return os.WriteFile(full, data, mode)
}

// safeVolumePath joins name under dir and rejects absolute paths and "..".
// ConfigMap keys and KeyToPath paths are otherwise written with filepath.Join,
// which would let "../../etc/cron.d/x" land outside the pod volume directory.
func safeVolumePath(dir, name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("empty volume file path")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("volume file path %q must be relative", name)
	}
	rel := filepath.Clean(name)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("volume file path %q escapes the volume directory", name)
	}
	full := filepath.Join(dir, rel)
	if !pathWithin(dir, full) {
		return "", fmt.Errorf("volume file path %q escapes the volume directory", name)
	}
	return full, nil
}

func cleanMountPath(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", fmt.Errorf("mountPath must be absolute")
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return "", fmt.Errorf("mountPath %q contains ..", p)
		}
	}
	cleaned := filepath.Clean(p)
	if cleaned == "/" {
		return "", fmt.Errorf("mountPath must be below /")
	}
	return cleaned, nil
}

// volumeSubPath resolves sub inside root, creating missing directories, and
// refuses any symlink that points outside root.
func volumeSubPath(root, sub string) (string, error) {
	if filepath.IsAbs(sub) {
		return "", fmt.Errorf("subPath must be relative")
	}
	rel := filepath.Clean(sub)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("subPath %q escapes the volume", sub)
	}
	rootResolved, err := resolvePath(root)
	if err != nil {
		return "", err
	}
	cur := rootResolved
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("subPath %q escapes the volume", sub)
		}
		last := i == len(parts)-1
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(next)
		if os.IsNotExist(err) {
			if err := os.Mkdir(next, 0o755); err != nil {
				return "", err
			}
			cur = next
			continue
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(next)
			if err != nil {
				return "", err
			}
			if !pathWithin(rootResolved, target) {
				return "", fmt.Errorf("subPath %q symlink escapes the volume", sub)
			}
			if !last {
				st, err := os.Stat(target)
				if err != nil {
					return "", err
				}
				if !st.IsDir() {
					return "", fmt.Errorf("subPath %q traverses a non-directory", sub)
				}
			}
			cur = target
			continue
		}
		if !fi.IsDir() {
			if !last || !fi.Mode().IsRegular() {
				return "", fmt.Errorf("subPath %q is not a directory", sub)
			}
		}
		cur = next
	}
	if !pathWithin(rootResolved, cur) {
		return "", fmt.Errorf("subPath %q escapes the volume", sub)
	}
	return cur, nil
}

func safeVolumeName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("invalid volume name %q", name)
	}
	return nil
}

func pathWithin(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(path, root+sep)
}

func fieldPath(pod *corev1.Pod, path string) (string, error) {
	switch path {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return string(pod.UID), nil
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "status.podIP":
		return pod.Status.PodIP, nil
	default:
		if len(path) > len("metadata.labels['") && path[:len("metadata.labels")] == "metadata.labels" {
			return lookupMap(pod.Labels, path), nil
		}
		if len(path) > len("metadata.annotations['") && path[:len("metadata.annotations")] == "metadata.annotations" {
			return lookupMap(pod.Annotations, path), nil
		}
		return "", fmt.Errorf("unsupported downwardAPI fieldPath %q", path)
	}
}

func lookupMap(m map[string]string, expr string) string {
	// metadata.labels['foo'] or metadata.labels['foo']
	start := -1
	end := -1
	for i, c := range expr {
		if c == '\'' && start < 0 {
			start = i + 1
		} else if c == '\'' && start >= 0 {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		return ""
	}
	key := expr[start:end]
	if m == nil {
		return ""
	}
	return m[key]
}
