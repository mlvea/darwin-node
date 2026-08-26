// Delta images: a small verifiable patch that turns one cached boot disk
// into another, so monthly image bumps are GB-scale pulls instead of full
// multi-GB re-downloads.
//
// A delta directory contains delta.json plus one <name>.patch per patched
// file. The patch is a flat sequence of records
//
//	[offset uint64 big-endian][length uint32 big-endian][data...]
//
// describing every chunk of the target file that differs from the base,
// followed by an implicit truncate to the target size. Apply is
// copy-on-write: the base image dir is cloned into the destination first
// (APFS clonefile), then patched in place, then verified against the
// recorded SHA-256 of each whole target file — correctness never depends on
// the diff algorithm.
package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/darwin-node/darwin-node/internal/clonefile"
)

const (
	// MediaTypeDiskDelta marks a delta patch layer for the boot disk.
	MediaTypeDiskDelta = "application/vnd.darwin-node.disk.delta.v1"

	// DefaultDeltaChunkSize balances patch granularity against record overhead.
	DefaultDeltaChunkSize = 4 << 20 // 4 MiB

	deltaSchema = 1
)

// PatchRef describes one binary patch file inside a delta directory.
type PatchRef struct {
	Name     string `json:"name"`    // base and destination file name (e.g. disk.img)
	BaseSHA  string `json:"baseSha"` // sha256 the base file must have
	BaseSize int64  `json:"baseSize"`
	DestSHA  string `json:"destSha"` // sha256 the patched result must have
	DestSize int64  `json:"destSize"`
}

// DeltaManifest is delta.json: what this delta applies to and what it yields.
type DeltaManifest struct {
	Schema    int        `json:"schema"`
	BaseImage string     `json:"baseImage,omitempty"` // image ref or dir the base came from
	Patches   []PatchRef `json:"patches"`
}

// WriteDeltaManifest stores manifest at dir/delta.json.
func WriteDeltaManifest(dir string, man DeltaManifest) error {
	man.Schema = deltaSchema
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "delta.json"), b, 0o644)
}

// ReadDeltaManifest loads dir/delta.json.
func ReadDeltaManifest(dir string) (DeltaManifest, error) {
	var man DeltaManifest
	b, err := os.ReadFile(filepath.Join(dir, "delta.json"))
	if err != nil {
		return man, err
	}
	if err := json.Unmarshal(b, &man); err != nil {
		return man, fmt.Errorf("delta.json: %w", err)
	}
	if man.Schema != deltaSchema {
		return man, fmt.Errorf("unsupported delta schema %d", man.Schema)
	}
	return man, nil
}

// CreatePatch compares basePath against targetPath, writes
// outDir/name.patch, appends it to outDir/delta.json, and returns its
// PatchRef. Both files must exist.
func CreatePatch(basePath, targetPath, outDir, name string) (PatchRef, error) {
	baseInfo, err := os.Stat(basePath)
	if err != nil {
		return PatchRef{}, fmt.Errorf("base: %w", err)
	}
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		return PatchRef{}, fmt.Errorf("target: %w", err)
	}
	baseSum, err := fileSHA256(basePath)
	if err != nil {
		return PatchRef{}, err
	}
	targetSum, err := fileSHA256(targetPath)
	if err != nil {
		return PatchRef{}, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return PatchRef{}, err
	}
	patchPath := filepath.Join(outDir, name+".patch")
	chunks, patchBytes, err := writePatchFile(basePath, targetPath, baseInfo.Size(), targetInfo.Size(), patchPath)
	if err != nil {
		return PatchRef{}, err
	}
	fmt.Fprintf(os.Stderr, "delta %s: %d changed chunks, %d byte patch for %d byte target\n",
		name, chunks, patchBytes, targetInfo.Size())
	ref := PatchRef{
		Name:     name,
		BaseSHA:  "sha256-" + hex.EncodeToString(baseSum),
		BaseSize: baseInfo.Size(),
		DestSHA:  "sha256-" + hex.EncodeToString(targetSum),
		DestSize: targetInfo.Size(),
	}
	man, err := ReadDeltaManifest(outDir)
	if err != nil {
		man = DeltaManifest{}
	}
	// One patch per target file: re-running replaces the previous entry.
	replaced := false
	for i := range man.Patches {
		if man.Patches[i].Name == ref.Name {
			man.Patches[i] = ref
			replaced = true
			break
		}
	}
	if !replaced {
		man.Patches = append(man.Patches, ref)
	}
	if err := WriteDeltaManifest(outDir, man); err != nil {
		return PatchRef{}, err
	}
	return ref, nil
}

// writePatchFile diffs in DefaultDeltaChunkSize blocks, appending records
// only for blocks that differ. Returns block count and bytes written.
func writePatchFile(basePath, targetPath string, baseSize, targetSize int64, outPath string) (int, int64, error) {
	baseF, err := os.Open(basePath)
	if err != nil {
		return 0, 0, err
	}
	defer baseF.Close()
	targetF, err := os.Open(targetPath)
	if err != nil {
		return 0, 0, err
	}
	defer targetF.Close()

	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, 0, err
	}
	defer out.Close()

	const chunk = DefaultDeltaChunkSize
	baseBuf := make([]byte, chunk)
	tgtBuf := make([]byte, chunk)
	var hdr [12]byte
	count := 0
	written := int64(0)
	for off := int64(0); off < targetSize; off += chunk {
		nT, rerr := io.ReadFull(targetF, tgtBuf)
		if rerr != nil && rerr != io.ErrUnexpectedEOF {
			if nT == 0 {
				break
			}
		}
		if nT == 0 {
			break
		}
		chunkTarget := tgtBuf[:nT]
		wantBase := chunkTarget
		if remain := baseSize - off; remain < int64(len(wantBase)) {
			if remain < 0 {
				remain = 0
			}
			wantBase = wantBase[:remain]
		}
		nB, _ := io.ReadFull(baseF, baseBuf[:len(wantBase)])
		same := nB == len(chunkTarget) && bytes.Equal(baseBuf[:nB], chunkTarget)
		if !same {
			binary.BigEndian.PutUint64(hdr[:8], uint64(off))
			binary.BigEndian.PutUint32(hdr[8:], uint32(nT))
			if _, err := out.Write(hdr[:]); err != nil {
				return 0, 0, err
			}
			if _, err := out.Write(chunkTarget); err != nil {
				return 0, 0, err
			}
			written += int64(12 + nT)
			count++
		}
	}
	return count, written, nil
}

// ApplyDelta produces destDir as a full copy of the base image dir with
// every listed patch applied. Base files are digest-verified before
// patching; results are verified after; destDir must not already exist.
func ApplyDelta(baseDir, deltaDir, destDir string) error {
	man, err := ReadDeltaManifest(deltaDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(destDir); err == nil {
		return fmt.Errorf("destination %s already exists", destDir)
	}

	img, err := LoadDir(baseDir)
	if err != nil {
		return fmt.Errorf("load base image: %w", err)
	}
	if err := img.VerifyOptional(); err != nil {
		return fmt.Errorf("verify base image: %w", err)
	}

	tmpDir := destDir + ".delta-tmp"
	_ = os.RemoveAll(tmpDir)
	defer func() { _ = os.RemoveAll(tmpDir) }()
	if err := clonefile.Dir(baseDir, tmpDir); err != nil {
		return fmt.Errorf("clone base: %w", err)
	}

	for _, p := range man.Patches {
		if err := applyOne(filepath.Join(baseDir, p.Name), filepath.Join(tmpDir, p.Name), filepath.Join(deltaDir, p.Name+".patch"), p); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpDir, destDir); err != nil {
		return err
	}
	return nil
}

func applyOne(src, dst, patchPath string, p PatchRef) error {
	sum, err := fileSHA256(src)
	if err != nil {
		return err
	}
	if got := "sha256-" + hex.EncodeToString(sum); got != p.BaseSHA {
		return fmt.Errorf("%s: base mismatch, have %s want %s", p.Name, got, p.BaseSHA)
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.Size() != p.BaseSize {
		return fmt.Errorf("%s: base size mismatch, have %d want %d", p.Name, info.Size(), p.BaseSize)
	}

	patch, err := os.Open(patchPath)
	if err != nil {
		return err
	}
	defer patch.Close()

	f, err := os.OpenFile(dst, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	hdr := make([]byte, 12)
	for {
		if _, err := io.ReadFull(patch, hdr); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		off := int64(binary.BigEndian.Uint64(hdr[:8]))
		length := binary.BigEndian.Uint32(hdr[8:])
		data := make([]byte, length)
		if _, err := io.ReadFull(patch, data); err != nil {
			return err
		}
		if _, err := f.WriteAt(data, off); err != nil {
			return err
		}
	}
	if err := f.Truncate(p.DestSize); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	result, err := fileSHA256(dst)
	if err != nil {
		return err
	}
	if got := "sha256-" + hex.EncodeToString(result); got != p.DestSHA {
		return fmt.Errorf("%s: verification failed, have %s want %s", p.Name, got, p.DestSHA)
	}
	return nil
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
