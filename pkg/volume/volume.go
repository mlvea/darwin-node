// Package volume materializes Kubernetes volumes on the host for virtio-fs.
package volume

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	Tokens           []ProjectedToken
	AllowedHostPaths []string // empty denies all hostPath
}

// ProjectedToken is one service-account token file inside a projected volume.
// An empty Token is refused. ExpiresAt is zero when the issuer did not say.
type ProjectedToken struct {
	Volume    string
	Path      string
	Token     string
	ExpiresAt time.Time
}

// EmptyDirLimit is a mounted emptyDir whose sizeLimit is enforced by
// directory walk. Medium Memory is still a directory; this is not an APFS quota.
type EmptyDirLimit struct {
	Name  string
	Path  string
	Bytes int64
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
		return hostPath, true, "copy", writeProjected(req, mount.Name, hostPath, src.Projected)

	case src.DownwardAPI != nil:
		hostPath = filepath.Join(req.RootDir, mount.Name)
		return hostPath, true, "copy", writeDownward(req.Pod, req.Container.Name, hostPath, src.DownwardAPI.Items)

	default:
		return "", false, "", fmt.Errorf("unsupported volume type (pvc/csi/gitRepo/etc. are not implemented)")
	}
}

func writeConfigMap(dir string, cm *corev1.ConfigMap, items []corev1.KeyToPath, def os.FileMode) error {
	files, err := configMapFiles(cm, items, def)
	if err != nil {
		return err
	}
	return publishVolume(dir, files)
}

func configMapFiles(cm *corev1.ConfigMap, items []corev1.KeyToPath, def os.FileMode) (map[string]volFile, error) {
	if cm == nil {
		return nil, fmt.Errorf("nil configmap")
	}
	out := map[string]volFile{}
	add := func(rel string, data []byte, mode os.FileMode) error {
		c, err := cleanVolumeRel(rel)
		if err != nil {
			return err
		}
		out[c] = volFile{data: append([]byte(nil), data...), mode: mode}
		return nil
	}
	if len(items) == 0 {
		for k, v := range cm.Data {
			if err := add(k, []byte(v), def); err != nil {
				return nil, err
			}
		}
		for k, v := range cm.BinaryData {
			if err := add(k, v, def); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	for _, it := range items {
		mode := def
		if it.Mode != nil {
			mode = modeOr(it.Mode, def)
		}
		if v, ok := cm.Data[it.Key]; ok {
			if err := add(it.Path, []byte(v), mode); err != nil {
				return nil, err
			}
			continue
		}
		if v, ok := cm.BinaryData[it.Key]; ok {
			if err := add(it.Path, v, mode); err != nil {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("configmap key %q missing", it.Key)
	}
	return out, nil
}

func writeSecret(dir string, sec *corev1.Secret, items []corev1.KeyToPath, def os.FileMode) error {
	files, err := secretFiles(sec, items, def)
	if err != nil {
		return err
	}
	return publishVolume(dir, files)
}

func secretFiles(sec *corev1.Secret, items []corev1.KeyToPath, def os.FileMode) (map[string]volFile, error) {
	if sec == nil {
		return nil, fmt.Errorf("nil secret")
	}
	out := map[string]volFile{}
	add := func(rel string, data []byte, mode os.FileMode) error {
		c, err := cleanVolumeRel(rel)
		if err != nil {
			return err
		}
		out[c] = volFile{data: append([]byte(nil), data...), mode: mode}
		return nil
	}
	if len(items) == 0 {
		for k, v := range sec.Data {
			if err := add(k, v, def); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	for _, it := range items {
		mode := def
		if it.Mode != nil {
			mode = modeOr(it.Mode, def)
		}
		v, ok := sec.Data[it.Key]
		if !ok {
			return nil, fmt.Errorf("secret key %q missing", it.Key)
		}
		if err := add(it.Path, v, mode); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func writeProjected(req Request, volumeName, dir string, proj *corev1.ProjectedVolumeSource) error {
	if proj == nil {
		return fmt.Errorf("nil projected volume")
	}
	all := map[string]volFile{}
	for _, s := range proj.Sources {
		var files map[string]volFile
		var err error
		switch {
		case s.ServiceAccountToken != nil:
			path := s.ServiceAccountToken.Path
			if path == "" {
				path = "token"
			}
			files, err = projectedTokenFile(req, volumeName, path, proj)
		case s.ConfigMap != nil:
			cm := req.ConfigMaps[s.ConfigMap.Name]
			if cm == nil {
				return fmt.Errorf("projected configmap %q not found", s.ConfigMap.Name)
			}
			def := filePerm
			if proj.DefaultMode != nil {
				def = modeOr(proj.DefaultMode, filePerm)
			}
			files, err = configMapFiles(cm, s.ConfigMap.Items, def)
		case s.Secret != nil:
			sec := req.Secrets[s.Secret.Name]
			if sec == nil {
				return fmt.Errorf("projected secret %q not found", s.Secret.Name)
			}
			def := secretPerm
			if proj.DefaultMode != nil {
				def = modeOr(proj.DefaultMode, secretPerm)
			}
			files, err = secretFiles(sec, s.Secret.Items, def)
		case s.DownwardAPI != nil:
			files, err = downwardFiles(req.Pod, req.Container.Name, s.DownwardAPI.Items)
		}
		if err != nil {
			return err
		}
		for k, v := range files {
			all[k] = v
		}
	}
	return publishVolume(dir, all)
}

func projectedTokenFile(req Request, volumeName, path string, proj *corev1.ProjectedVolumeSource) (map[string]volFile, error) {
	tok, ok := projectedToken(req, volumeName, path, proj)
	if !ok || strings.TrimSpace(tok) == "" {
		return nil, fmt.Errorf("projected service account token %q: refusing to write an empty token", path)
	}
	rel, err := cleanVolumeRel(path)
	if err != nil {
		return nil, err
	}
	return map[string]volFile{rel: {data: []byte(tok), mode: secretPerm}}, nil
}

func projectedToken(req Request, volume, path string, proj *corev1.ProjectedVolumeSource) (string, bool) {
	for _, t := range req.Tokens {
		p := t.Path
		if p == "" {
			p = "token"
		}
		if t.Volume == volume && p == path {
			return t.Token, true
		}
	}
	// A single projection may still use the legacy pod-level token. Two
	// projections would otherwise be handed the same credential.
	if serviceAccountProjectionCount(proj) == 1 && strings.TrimSpace(req.ServiceToken) != "" {
		return req.ServiceToken, true
	}
	return "", false
}

func serviceAccountProjectionCount(proj *corev1.ProjectedVolumeSource) int {
	if proj == nil {
		return 0
	}
	n := 0
	for _, s := range proj.Sources {
		if s.ServiceAccountToken != nil {
			n++
		}
	}
	return n
}

// WriteProjectedTokens rewrites token files under root/<volume>/<path>.
// It does not create an empty file. Other regular files already in the
// volume are kept. Symlinks are not opened or followed.
func WriteProjectedTokens(root string, tokens []ProjectedToken) error {
	grouped := map[string][]ProjectedToken{}
	for _, t := range tokens {
		if err := safeVolumeName(t.Volume); err != nil {
			return err
		}
		if strings.TrimSpace(t.Token) == "" {
			return fmt.Errorf("projected service account token %s/%s: refusing to write an empty token", t.Volume, t.Path)
		}
		grouped[t.Volume] = append(grouped[t.Volume], t)
	}
	for volume, list := range grouped {
		dir := filepath.Join(root, volume)
		files, err := readPublished(dir)
		if err != nil {
			return err
		}
		for _, t := range list {
			path := t.Path
			if path == "" {
				path = "token"
			}
			rel, err := cleanVolumeRel(path)
			if err != nil {
				return err
			}
			files[rel] = volFile{data: []byte(t.Token), mode: secretPerm}
		}
		if err := publishVolume(dir, files); err != nil {
			return err
		}
	}
	return nil
}

// EmptyDirLimits lists mounted emptyDirs that declared sizeLimit.
// An unmounted volume has no directory to account, so it is skipped.
func EmptyDirLimits(pod *corev1.Pod, root string) ([]EmptyDirLimit, error) {
	if pod == nil {
		return nil, nil
	}
	var out []EmptyDirLimit
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil {
			continue
		}
		if !volumeMounted(pod, v.Name) {
			continue
		}
		if err := safeVolumeName(v.Name); err != nil {
			return nil, err
		}
		q := v.EmptyDir.SizeLimit
		if q.Sign() < 0 {
			return nil, fmt.Errorf("emptyDir %q sizeLimit is negative", v.Name)
		}
		n, ok := q.AsInt64()
		if !ok || n < 0 {
			return nil, fmt.Errorf("emptyDir %q sizeLimit is not a byte count", v.Name)
		}
		out = append(out, EmptyDirLimit{
			Name:  v.Name,
			Path:  filepath.Join(root, v.Name),
			Bytes: n,
		})
	}
	return out, nil
}

func volumeMounted(pod *corev1.Pod, name string) bool {
	check := func(mounts []corev1.VolumeMount) bool {
		for _, m := range mounts {
			if m.Name == name {
				return true
			}
		}
		return false
	}
	for _, c := range pod.Spec.InitContainers {
		if check(c.VolumeMounts) {
			return true
		}
	}
	for _, c := range pod.Spec.Containers {
		if check(c.VolumeMounts) {
			return true
		}
	}
	return false
}

// DirUsage is the sum of regular-file sizes under root. Directory metadata
// and symlinks are not counted. Guest symlinks are not followed: an absolute
// link is a path in the guest, and a dangling link is not payload. A relative
// link that lexically leaves root still fails closed, and the error does not
// include the host path. A missing file during the walk (ENOENT) is ignored.
// A missing root is zero bytes.
func DirUsage(root string) (int64, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return 0, errUsage(err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, errUsage(err)
	}
	// Resolve ancestor symlinks (macOS /var -> /private/var) so the volume
	// root is canonical. Guest symlinks under that root are not resolved.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, errUsage(err)
	}
	root = resolved
	if !info.IsDir() {
		info, err = os.Lstat(root)
		if err != nil {
			if os.IsNotExist(err) {
				return 0, nil
			}
			return 0, errUsage(err)
		}
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("emptyDir is not a directory")
	}
	var n int64
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return errUsage(err)
		}
		if d == nil || path == root {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return judgeSymlink(root, path)
		}
		st, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return errUsage(err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return judgeSymlink(root, path)
		}
		if !st.Mode().IsRegular() {
			return nil
		}
		if n > math.MaxInt64-st.Size() {
			return fmt.Errorf("emptyDir usage overflow")
		}
		n += st.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// judgeSymlink does not stat or EvalSymlinks the target. Absolute targets
// belong to the guest. Relative targets are checked lexically so ".."
// outside this directory still fails closed.
func judgeSymlink(root, path string) error {
	target, err := os.Readlink(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("emptyDir symlink")
	}
	if filepath.IsAbs(target) {
		return nil
	}
	cleaned := filepath.Clean(filepath.Join(filepath.Dir(path), target))
	if cleaned != root && !pathWithin(root, cleaned) {
		return fmt.Errorf("emptyDir symlink escapes the volume")
	}
	return nil
}

func errUsage(err error) error {
	if err == nil {
		return nil
	}
	var pe *os.PathError
	if errors.As(err, &pe) && pe.Err != nil {
		return fmt.Errorf("emptyDir: %s", pe.Err.Error())
	}
	if strings.ContainsAny(err.Error(), `/\`) {
		return fmt.Errorf("emptyDir check failed")
	}
	return err
}

func writeDownward(pod *corev1.Pod, containerName, dir string, items []corev1.DownwardAPIVolumeFile) error {
	files, err := downwardFiles(pod, containerName, items)
	if err != nil {
		return err
	}
	return publishVolume(dir, files)
}

func downwardFiles(pod *corev1.Pod, containerName string, items []corev1.DownwardAPIVolumeFile) (map[string]volFile, error) {
	out := map[string]volFile{}
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
			return nil, err
		}
		mode := filePerm
		if it.Mode != nil {
			mode = modeOr(it.Mode, filePerm)
		}
		rel, err := cleanVolumeRel(it.Path)
		if err != nil {
			return nil, err
		}
		out[rel] = volFile{data: []byte(val), mode: mode}
	}
	return out, nil
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

func cleanMountPath(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", fmt.Errorf("mountPath must be absolute")
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return "", fmt.Errorf("mountPath %q contains a '..' segment", p)
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
