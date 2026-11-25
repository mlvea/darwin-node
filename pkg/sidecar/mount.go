package sidecar

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darwin-node/darwin-node/pkg/volume"
	"github.com/docker/docker/api/types/container"
	corev1 "k8s.io/api/core/v1"
)

func volumeSource(pod *corev1.Pod, name string) *corev1.VolumeSource {
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

func projectedContent(src *corev1.VolumeSource) bool {
	return src != nil && (src.ConfigMap != nil || src.Secret != nil || src.Projected != nil || src.DownwardAPI != nil)
}

func volumeBinds(pod *corev1.Pod, c corev1.Container, volRoot string, allowed []string) ([]string, error) {
	var binds []string
	for _, m := range c.VolumeMounts {
		host, ro, err := bindHost(pod, m, volRoot, allowed)
		if err != nil {
			return nil, err
		}
		if host == "" {
			continue
		}
		mode := "rw"
		if ro {
			mode = "ro"
		}
		binds = append(binds, host+":"+m.MountPath+":"+mode)
	}
	return binds, nil
}

func bindHost(pod *corev1.Pod, m corev1.VolumeMount, volRoot string, allowed []string) (string, bool, error) {
	if strings.TrimSpace(m.MountPath) == "" || strings.Contains(m.MountPath, ":") {
		return "", false, fmt.Errorf("volume %q: mountPath %q is invalid", m.Name, m.MountPath)
	}
	if m.SubPathExpr != "" {
		return "", false, fmt.Errorf("volume %q: subPathExpr is not supported", m.Name)
	}
	if isDockerSock(m.MountPath) {
		return "", false, fmt.Errorf("volume %q: docker.sock mounts are not supported", m.Name)
	}
	src := volumeSource(pod, m.Name)
	if src == nil {
		return "", false, nil
	}
	ro := m.ReadOnly
	var host string
	var err error
	switch {
	case src.HostPath != nil:
		if isDockerSock(src.HostPath.Path) {
			return "", false, fmt.Errorf("volume %q: docker.sock mounts are not supported", m.Name)
		}
		host, err = volume.ResolveAllowedHostPath(src.HostPath.Path, allowed)
		if err != nil {
			return "", false, fmt.Errorf("volume %q: %w", m.Name, err)
		}
	default:
		if volRoot == "" {
			return "", false, nil
		}
		host = filepath.Join(volRoot, m.Name)
		if projectedContent(src) {
			ro = true
		}
	}
	if m.SubPath != "" || src.HostPath == nil {
		host, err = confine(host, m.SubPath)
		if err != nil {
			return "", false, fmt.Errorf("volume %q: %w", m.Name, err)
		}
	}
	if src.HostPath != nil {
		if _, err := volume.ResolveAllowedHostPath(host, allowed); err != nil {
			return "", false, fmt.Errorf("volume %q: %w", m.Name, err)
		}
	}
	if isDockerSock(host) {
		return "", false, fmt.Errorf("volume %q: docker.sock mounts are not supported", m.Name)
	}
	return host, ro, nil
}

// confine resolves sub inside root and returns that path. It does not
// create directories. A symlink whose target leaves root is rejected.
// A missing path is kept only when its existing parents stay inside root.
func confine(root, sub string) (string, error) {
	if sub != "" {
		if filepath.IsAbs(sub) || strings.ContainsRune(sub, 0) {
			return "", fmt.Errorf("subPath must be relative")
		}
		rel := filepath.Clean(sub)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("subPath %q escapes the volume", sub)
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "" || part == "." || part == ".." {
				return "", fmt.Errorf("subPath %q escapes the volume", sub)
			}
		}
		sub = rel
	}
	rootResolved, err := evalOrLexical(root)
	if err != nil {
		return "", err
	}
	target := rootResolved
	if sub != "" {
		target = filepath.Join(rootResolved, sub)
	}
	resolved, err := evalOrLexical(target)
	if err != nil {
		return "", err
	}
	if !pathInside(rootResolved, resolved) {
		if sub == "" {
			return "", fmt.Errorf("volume path %q escapes %q", resolved, root)
		}
		return "", fmt.Errorf("subPath %q escapes the volume", sub)
	}
	return resolved, nil
}

func evalOrLexical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	var missing []string
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if len(missing) == 0 {
				return filepath.Clean(resolved), nil
			}
			parts := make([]string, 0, 1+len(missing))
			parts = append(parts, resolved)
			for i := len(missing) - 1; i >= 0; i-- {
				parts = append(parts, missing[i])
			}
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

func pathInside(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

func dockerHostConfig(pod *corev1.Pod, c corev1.Container, volRoot string, allowed []string) (*container.HostConfig, error) {
	_, host, err := prepareContainer(pod, c, volRoot, CreateOptions{AllowedHostPaths: allowed})
	return host, err
}
