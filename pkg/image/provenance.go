package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/darwin-node/darwin-node/internal/digest"

	ocidigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ProvenanceFile is the lockfile written next to config.json at pull time.
const ProvenanceFile = "provenance.json"

// Provenance maps cache filenames (disk.img, aux.img, config.json) to the
// digest recorded when the image was pulled (uncompressed digest from ORAS
// annotations when present, otherwise the hash of the materialized file).
type Provenance struct {
	Files map[string]string `json:"files"`
	Sizes map[string]int64  `json:"sizes,omitempty"`
}

func (p Provenance) digestFor(name string) (ocidigest.Digest, bool) {
	if p.Files == nil || name == "" {
		return "", false
	}
	s := p.Files[name]
	if s == "" {
		return "", false
	}
	d, err := ocidigest.Parse(s)
	if err != nil {
		return "", false
	}
	return d, true
}

// WriteProvenance persists p as dir/provenance.json.
func WriteProvenance(dir string, p Provenance) error {
	if p.Files == nil {
		p.Files = map[string]string{}
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ProvenanceFile), raw, 0o600)
}

// LoadProvenance reads dir/provenance.json.
func LoadProvenance(dir string) (Provenance, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ProvenanceFile))
	if err != nil {
		return Provenance{}, err
	}
	var p Provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return Provenance{}, fmt.Errorf("parse %s: %w", ProvenanceFile, err)
	}
	if p.Files == nil {
		p.Files = map[string]string{}
	}
	return p, nil
}

func (img *LocalImage) applyProvenance(p Provenance) {
	if d, ok := p.digestFor(filepath.Base(img.DiskPath)); ok {
		img.DiskDig = d
	} else if d, ok := p.digestFor("disk.img"); ok {
		img.DiskDig = d
	}
	if d, ok := p.digestFor(filepath.Base(img.AuxPath)); ok {
		img.AuxDig = d
	} else if d, ok := p.digestFor("aux.img"); ok {
		img.AuxDig = d
	}
}

// provenanceFromDescriptors records expected dest-file digests from ORAS
// layer descriptors. Disk/aux use uncompressed annotations when present;
// compressed blob digests are not used as the dest-file expected digest.
func provenanceFromDescriptors(descs []ocispec.Descriptor) Provenance {
	p := Provenance{Files: map[string]string{}, Sizes: map[string]int64{}}
	for _, d := range descs {
		title := ""
		if d.Annotations != nil {
			title = d.Annotations[ocispec.AnnotationTitle]
		}
		role := layerRole(title, d.MediaType, title)
		if role == "" {
			continue
		}
		name := filepath.Base(destForRole(".", role))
		if name == "" || name == "." {
			continue
		}
		uncomp, usize := UncompressedMeta(d.Annotations)
		switch {
		case uncomp != "":
			p.Files[name] = uncomp
			if usize > 0 {
				p.Sizes[name] = usize
			}
		case role == "config" && d.Digest != "":
			p.Files[name] = d.Digest.String()
			if d.Size > 0 {
				p.Sizes[name] = d.Size
			}
		}
	}
	return p
}

// completeProvenance fills missing dest-file digests by hashing (or reading
// an existing sidecar) and checks recorded sizes. expected digests already
// in p are verified against the files.
func completeProvenance(dir string, p *Provenance) error {
	if p.Files == nil {
		p.Files = map[string]string{}
	}
	if p.Sizes == nil {
		p.Sizes = map[string]int64{}
	}
	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		path := filepath.Join(dir, name)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		if want := p.Sizes[name]; want > 0 && st.Size() != want {
			return fmt.Errorf("%s size %d != provenance %d", name, st.Size(), want)
		}
		expected := p.Files[name]
		if expected != "" {
			d, err := ocidigest.Parse(expected)
			if err != nil {
				return fmt.Errorf("provenance %s: %w", name, err)
			}
			if err := digest.Verify(path, d); err != nil {
				return err
			}
			if p.Sizes[name] == 0 {
				p.Sizes[name] = st.Size()
			}
			continue
		}
		var d ocidigest.Digest
		if got, err := digest.ReadSidecar(path); err == nil {
			d = got
		} else {
			sum, err := digest.FileSHA256(path)
			if err != nil {
				return err
			}
			d = sum
		}
		if err := digest.WriteSidecar(path, d); err != nil {
			return err
		}
		p.Files[name] = d.String()
		p.Sizes[name] = st.Size()
	}
	return nil
}
