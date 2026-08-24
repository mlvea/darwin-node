// Package volume materializes Kubernetes volumes on the host for virtio-fs.
package volume

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
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
		src := findVolume(req.Pod, mount.Name)
		if src == nil {
			return nil, nil, fmt.Errorf("volume %q referenced by mount but not defined", mount.Name)
		}
		hostPath, readOnly, mode, err := materializeOne(req, mount, src)
		if err != nil {
			return nil, nil, fmt.Errorf("volume %q: %w", mount.Name, err)
		}
		guest := mount.MountPath
		if mount.SubPath != "" {
			guest = filepath.Join(mount.MountPath, mount.SubPath)
		}
		if mount.ReadOnly {
			readOnly = true
		}
		if !seen[mount.Name] {
			shares = append(shares, types.Share{
				Name:     mount.Name,
				HostPath: hostPath,
				ReadOnly: readOnly,
			})
			seen[mount.Name] = true
		}
		places = append(places, Placement{
			Name:      mount.Name,
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
		return hostPath, true, "copy", writeConfigMap(hostPath, cm, src.ConfigMap.Items)

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
		return hostPath, true, "copy", writeSecret(hostPath, sec, src.Secret.Items)

	case src.Projected != nil:
		hostPath = filepath.Join(req.RootDir, mount.Name)
		return hostPath, true, "copy", writeProjected(req, hostPath, src.Projected)

	case src.DownwardAPI != nil:
		hostPath = filepath.Join(req.RootDir, mount.Name)
		return hostPath, true, "copy", writeDownward(req.Pod, hostPath, src.DownwardAPI.Items)

	default:
		return "", false, "", fmt.Errorf("unsupported volume type (pvc/csi/gitRepo/etc. are not implemented)")
	}
}

func writeConfigMap(dir string, cm *corev1.ConfigMap, items []corev1.KeyToPath) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	if len(items) == 0 {
		for k, v := range cm.Data {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v), filePerm); err != nil {
				return err
			}
		}
		for k, v := range cm.BinaryData {
			if err := os.WriteFile(filepath.Join(dir, k), v, filePerm); err != nil {
				return err
			}
		}
		return nil
	}
	for _, it := range items {
		mode := filePerm
		if it.Mode != nil {
			mode = os.FileMode(*it.Mode)
		}
		if v, ok := cm.Data[it.Key]; ok {
			if err := os.WriteFile(filepath.Join(dir, it.Path), []byte(v), mode); err != nil {
				return err
			}
			continue
		}
		if v, ok := cm.BinaryData[it.Key]; ok {
			if err := os.WriteFile(filepath.Join(dir, it.Path), v, mode); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("configmap key %q missing", it.Key)
	}
	return nil
}

func writeSecret(dir string, sec *corev1.Secret, items []corev1.KeyToPath) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	if len(items) == 0 {
		for k, v := range sec.Data {
			if err := os.WriteFile(filepath.Join(dir, k), v, secretPerm); err != nil {
				return err
			}
		}
		return nil
	}
	for _, it := range items {
		mode := secretPerm
		if it.Mode != nil {
			mode = os.FileMode(*it.Mode)
		}
		v, ok := sec.Data[it.Key]
		if !ok {
			return fmt.Errorf("secret key %q missing", it.Key)
		}
		if err := os.WriteFile(filepath.Join(dir, it.Path), v, mode); err != nil {
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
			if err := os.WriteFile(filepath.Join(dir, path), []byte(req.ServiceToken), secretPerm); err != nil {
				return err
			}
		case s.ConfigMap != nil:
			cm := req.ConfigMaps[s.ConfigMap.Name]
			if cm == nil {
				return fmt.Errorf("projected configmap %q not found", s.ConfigMap.Name)
			}
			if err := writeConfigMap(dir, cm, s.ConfigMap.Items); err != nil {
				return err
			}
		case s.Secret != nil:
			sec := req.Secrets[s.Secret.Name]
			if sec == nil {
				return fmt.Errorf("projected secret %q not found", s.Secret.Name)
			}
			if err := writeSecret(dir, sec, s.Secret.Items); err != nil {
				return err
			}
		case s.DownwardAPI != nil:
			if err := writeDownward(req.Pod, dir, s.DownwardAPI.Items); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeDownward(pod *corev1.Pod, dir string, items []corev1.DownwardAPIVolumeFile) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	for _, it := range items {
		if it.FieldRef == nil {
			continue
		}
		val, err := fieldPath(pod, it.FieldRef.FieldPath)
		if err != nil {
			return err
		}
		mode := filePerm
		if it.Mode != nil {
			mode = os.FileMode(*it.Mode)
		}
		if err := os.WriteFile(filepath.Join(dir, it.Path), []byte(val), mode); err != nil {
			return err
		}
	}
	return nil
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
