package image

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/darwin-node/darwin-node/internal/digest"
	"github.com/darwin-node/darwin-node/internal/disk"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// Puller fetches an image into the local cache.
type Puller interface {
	Pull(ctx context.Context, ref string, creds RegistryCreds, ignoreCache bool) (LocalImage, error)
}

// Manager single-flights pulls by ref.
type Manager struct {
	CacheDir           string
	InsecureRegistries []string

	mu       sync.Mutex
	inflight map[string]*pullWait
}

type pullWait struct {
	done chan struct{}
	img  LocalImage
	err  error
}

func NewManager(cacheDir string) *Manager {
	return &Manager{CacheDir: cacheDir, inflight: map[string]*pullWait{}}
}

func (m *Manager) Pull(ctx context.Context, ref string, creds RegistryCreds, ignoreCache bool) (LocalImage, error) {
	if !ignoreCache {
		if img, err := LoadDir(CacheDir(m.CacheDir, ref)); err == nil {
			if err := img.VerifyOptional(); err == nil {
				return img, nil
			}
		}
	}
	m.mu.Lock()
	if w, ok := m.inflight[ref]; ok {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return LocalImage{}, ctx.Err()
		case <-w.done:
			return w.img, w.err
		}
	}
	w := &pullWait{done: make(chan struct{})}
	m.inflight[ref] = w
	m.mu.Unlock()

	img, err := m.pullOnce(ctx, ref, creds)
	w.img, w.err = img, err
	close(w.done)

	m.mu.Lock()
	delete(m.inflight, ref)
	m.mu.Unlock()
	return img, err
}

func (m *Manager) pullOnce(ctx context.Context, ref string, creds RegistryCreds) (LocalImage, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return LocalImage{}, fmt.Errorf("parse image ref %q: %w", ref, err)
	}
	repo.Client = &auth.Client{
		Client: retry.DefaultClient,
		Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
			Username:     creds.Username,
			Password:     creds.Password,
			RefreshToken: creds.IdentityToken,
		}),
	}
	if isPlainHTTP(repo.Reference.Registry, m.InsecureRegistries) {
		repo.PlainHTTP = true
	}
	destDir := CacheDir(m.CacheDir, ref)
	if err := os.MkdirAll(destDir, cacheDirPerm); err != nil {
		return LocalImage{}, err
	}
	fs, err := file.New(destDir)
	if err != nil {
		return LocalImage{}, err
	}
	defer fs.Close()

	var (
		copiedMu sync.Mutex
		copied   []ocispec.Descriptor
	)
	record := func(_ context.Context, d ocispec.Descriptor) error {
		copiedMu.Lock()
		copied = append(copied, d)
		copiedMu.Unlock()
		return nil
	}
	opts := oras.DefaultCopyOptions
	opts.PostCopy = record
	opts.OnCopySkipped = record

	desc, err := oras.Copy(ctx, repo, repo.Reference.Reference, fs, "", opts)
	if err != nil {
		return LocalImage{}, fmt.Errorf("pull %s: %w", ref, err)
	}
	copiedMu.Lock()
	copied = append(copied, desc)
	nodes := copied
	copiedMu.Unlock()

	deltaInfo, found, derr := findDeltaLayer(nodes)
	if derr != nil {
		_ = os.RemoveAll(destDir)
		return LocalImage{}, fmt.Errorf("pull %s: %w", ref, derr)
	}
	if deltaInfo != nil && found {
		return m.pullDeltaArtifact(ctx, destDir, deltaInfo, creds)
	}

	prov := provenanceFromDescriptors(nodes)
	if err := materializePulled(destDir); err != nil {
		return LocalImage{}, err
	}
	if err := requireMaterializedDisk(destDir); err != nil {
		return LocalImage{}, fmt.Errorf("pull %s: %w", ref, err)
	}
	if err := completeProvenance(destDir, &prov); err != nil {
		return LocalImage{}, err
	}
	if err := WriteProvenance(destDir, prov); err != nil {
		return LocalImage{}, err
	}
	return LoadDir(destDir)
}

const cacheDirPerm os.FileMode = 0o700

func requireMaterializedDisk(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "disk.img")); err != nil {
		return fmt.Errorf("no disk.img identified by media type %s or title disk.img", MediaTypeDisk)
	}
	return nil
}

// isPlainHTTP is true only for loopback registries or an explicit allow-list.
func isPlainHTTP(registry string, insecure []string) bool {
	host := registry
	if h, _, err := net.SplitHostPort(registry); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if host == "localhost" || host == "::1" || (ip != nil && ip.IsLoopback()) {
		return true
	}
	for _, h := range insecure {
		if h == "" {
			continue
		}
		if host == h || registry == h {
			return true
		}
	}
	return false
}

// layerRole maps an OCI layer to config/disk/aux using media type or exact title.
func layerRole(name, mediaType, title string) string {
	switch CanonicalMediaType(mediaType) {
	case MediaTypeConfig:
		return "config"
	case MediaTypeDisk:
		return "disk"
	case MediaTypeAux:
		return "aux"
	}
	t := title
	if t == "" {
		t = name
	}
	switch t {
	case "config.json":
		return "config"
	case "disk.img":
		return "disk"
	case "aux.img":
		return "aux"
	}
	return ""
}

func destForRole(dir, role string) string {
	switch role {
	case "config":
		return filepath.Join(dir, "config.json")
	case "disk":
		return filepath.Join(dir, "disk.img")
	case "aux":
		return filepath.Join(dir, "aux.img")
	default:
		return ""
	}
}

// materializePulled finds titled blobs ORAS wrote and, if they are gzip,
// sparsely decompresses them to disk.img / aux.img / config.json.
func materializePulled(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		role := layerRole(name, "", name)
		if role == "" {
			continue
		}
		src := filepath.Join(dir, name)
		dest := destForRole(dir, role)
		if role == "config" {
			if name != "config.json" {
				if err := os.Rename(src, dest); err != nil {
					return err
				}
			}
			continue
		}
		if err := maybeDecompress(src, dest); err != nil {
			return err
		}
	}
	return nil
}

func maybeDecompress(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [2]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if hdr[0] == 0x1f && hdr[1] == 0x8b {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		tmp := dest + ".ungz.tmp"
		if err := disk.CopySparse(tmp, gz, 0); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, dest); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		sum, err := digest.FileSHA256(dest)
		if err != nil {
			return err
		}
		return digest.WriteSidecar(dest, sum)
	}
	if src != dest {
		return copyFile(src, dest)
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// UncompressedMeta reads Darwin-Node or Agoda annotations from a descriptor.
func UncompressedMeta(ann map[string]string) (digestStr string, size int64) {
	digestStr = UncompressedDigestFromAnnotations(ann)
	if ann == nil {
		return digestStr, 0
	}
	s := ann[AnnotUncompressedSize]
	if s == "" {
		s = ann[AgodaUncompSize]
	}
	if s != "" {
		size, _ = strconv.ParseInt(s, 10, 64)
	}
	return digestStr, size
}

// ValidateRef is admission-time: same parser as pull.
func ValidateRef(ref string) error {
	_, err := remote.NewRepository(ref)
	return err
}
