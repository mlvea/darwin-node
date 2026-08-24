package volume

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// AllowHostPath reports whether path may be shared into a guest.
// An empty allowlist denies every hostPath. The host root is never allowed.
func AllowHostPath(path string, allowed []string) error {
	resolved, err := resolvePath(path)
	if err != nil {
		return err
	}
	return allowResolved(resolved, allowed)
}

func materializeHostPath(src *corev1.HostPathVolumeSource, allowed []string) (string, error) {
	if src == nil {
		return "", fmt.Errorf("nil hostPath")
	}
	resolved, err := resolvePath(src.Path)
	if err != nil {
		return "", err
	}
	if err := allowResolved(resolved, allowed); err != nil {
		return "", err
	}
	if err := ensureHostPathType(resolved, src.Type); err != nil {
		return "", err
	}
	out, err := resolvePath(resolved)
	if err != nil {
		return "", err
	}
	if err := allowResolved(out, allowed); err != nil {
		return "", err
	}
	return out, nil
}

func allowResolved(resolved string, allowed []string) error {
	if resolved == "" {
		return fmt.Errorf("empty hostPath")
	}
	if resolved == string(os.PathSeparator) || resolved == "/" {
		return fmt.Errorf("hostPath %q is not allowed (refusing to mount host root)", resolved)
	}
	if len(allowed) == 0 {
		return fmt.Errorf("hostPath %q is not allowed (empty allowlist; set --allowed-host-paths or DARWIN_NODE_ALLOWED_HOST_PATHS)", resolved)
	}
	sep := string(os.PathSeparator)
	for _, a := range allowed {
		if strings.TrimSpace(a) == "" {
			continue
		}
		prefix, err := resolvePath(a)
		if err != nil {
			continue
		}
		if prefix == sep || prefix == "/" {
			return nil
		}
		if resolved == prefix || strings.HasPrefix(resolved, prefix+sep) {
			return nil
		}
	}
	return fmt.Errorf("hostPath %q is not under allowed prefixes %q", resolved, allowed)
}

func resolvePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty hostPath")
	}
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
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

func ensureHostPathType(path string, t *corev1.HostPathType) error {
	if t == nil || *t == corev1.HostPathUnset {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("hostPath %q: %w", path, err)
		}
		return nil
	}
	switch *t {
	case corev1.HostPathDirectoryOrCreate:
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("hostPath %q is not a directory", path)
		}
	case corev1.HostPathDirectory:
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("hostPath %q: %w", path, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("hostPath %q is not a directory", path)
		}
	case corev1.HostPathFileOrCreate:
		fi, err := os.Stat(path)
		if os.IsNotExist(err) {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, filePerm)
			if err != nil {
				return err
			}
			return f.Close()
		}
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return fmt.Errorf("hostPath %q is a directory, not a file", path)
		}
	case corev1.HostPathFile:
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("hostPath %q: %w", path, err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("hostPath %q is not a file", path)
		}
	case corev1.HostPathSocket:
		fi, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("hostPath %q: %w", path, err)
		}
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("hostPath %q is not a socket", path)
		}
	case corev1.HostPathCharDev:
		fi, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("hostPath %q: %w", path, err)
		}
		if fi.Mode()&os.ModeCharDevice == 0 {
			return fmt.Errorf("hostPath %q is not a character device", path)
		}
	case corev1.HostPathBlockDev:
		fi, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("hostPath %q: %w", path, err)
		}
		m := fi.Mode()
		if m&os.ModeDevice == 0 || m&os.ModeCharDevice != 0 {
			return fmt.Errorf("hostPath %q is not a block device", path)
		}
	default:
		return fmt.Errorf("unknown hostPath type %q", *t)
	}
	return nil
}
