package image

import (
	"encoding/json"
	"fmt"
)

// Canonical media types.
const (
	ArtifactType    = "application/vnd.darwin-node.macosvm"
	MediaTypeDisk   = "application/vnd.darwin-node.disk.v1"
	MediaTypeAux    = "application/vnd.darwin-node.aux.v1"
	MediaTypeConfig = "application/vnd.darwin-node.config.v1+json"
)

// Agoda media types accepted on read. Derived from Agoda
// macOS-vz-kubelet pkg/oci (Apache-2.0); the JSON field names
// (hardwareModelData, machineIdData, storage[].mediatype/file) match
// that config so existing registries keep working.
const (
	AgodaArtifact   = "application/vnd.agoda.macosvz.artifact"
	AgodaDisk       = "application/vnd.agoda.macosvz.disk.image.v1"
	AgodaAux        = "application/vnd.agoda.macosvz.aux.image.v1"
	AgodaConfig     = "application/vnd.agoda.macosvz.config.v1+json"
	AgodaUncompDig  = "com.agoda.macosvz.content.uncompressed-digest"
	AgodaUncompSize = "com.agoda.macosvz.content.uncompressed-size"
)

// Annotation keys we write.
const (
	AnnotUncompressedDigest = "dev.darwin-node.content.uncompressed-digest"
	AnnotUncompressedSize   = "dev.darwin-node.content.uncompressed-size"

	// Delta-layer annotations. A delta artifact carries its disk patch as a
	// MediaTypeDiskDelta layer whose descriptor is annotated with:
	//   - where the base image lives (base-ref)
	//   - which base disk content it patches (base-disk-sha256, hex)
	//   - what the patched disk must hash to (uncompressed-digest/size,
	//     the same annotations a full disk layer carries)
	AnnotDeltaBaseRef    = "dev.darwin-node.delta.base-ref"
	AnnotDeltaBaseDigest = "dev.darwin-node.delta.base-disk-sha256"
)

// GuestAgentInfo is recorded in the image config when the agent is baked in.
type GuestAgentInfo struct {
	Version   string `json:"version,omitempty"`
	VsockPort int    `json:"vsockPort,omitempty"`
}

// GraphicsInfo is the display used at bake time (overridable at run).
type GraphicsInfo struct {
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	PPI    int `json:"ppi,omitempty"`
}

// BlobRef names a file in the artifact.
type BlobRef struct {
	MediaType string `json:"mediatype"`
	File      string `json:"file"`
}

// Config is the Darwin-Node macOS VM image config.
type Config struct {
	MediaType         string         `json:"mediatype,omitempty"`
	OS                string         `json:"os"`
	OSVersion         string         `json:"osVersion,omitempty"`
	HardwareModelData string         `json:"hardwareModelData"`
	MachineIdData     string         `json:"machineIdData"`
	GuestAgent        GuestAgentInfo `json:"guestAgent"`
	GuestSSHHostKey   string         `json:"guestSSHHostKey,omitempty"` // OpenSSH authorized_keys line
	Graphics          GraphicsInfo   `json:"graphics"`
	Storage           []BlobRef      `json:"storage"`
}

// ParseConfig accepts Darwin-Node and Agoda config JSON.
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, err
	}
	if c.OS == "" {
		c.OS = "darwin"
	}
	if c.MediaType == AgodaConfig {
		c.MediaType = MediaTypeConfig
	}
	if c.MediaType == "" {
		c.MediaType = MediaTypeConfig
	}
	if c.HardwareModelData == "" {
		return Config{}, fmt.Errorf("image config missing hardwareModelData")
	}
	for i, s := range c.Storage {
		c.Storage[i].MediaType = CanonicalMediaType(s.MediaType)
	}
	return c, nil
}

// CanonicalMediaType maps Agoda types onto Darwin-Node types.
func CanonicalMediaType(mt string) string {
	switch mt {
	case AgodaDisk:
		return MediaTypeDisk
	case AgodaAux:
		return MediaTypeAux
	case AgodaConfig:
		return MediaTypeConfig
	default:
		return mt
	}
}

// IsSupportedMediaType reports whether we can pull this layer.
func IsSupportedMediaType(mt string) bool {
	switch CanonicalMediaType(mt) {
	case MediaTypeDisk, MediaTypeAux, MediaTypeConfig:
		return true
	default:
		return false
	}
}

// UncompressedDigestFromAnnotations reads Darwin-Node or Agoda keys.
func UncompressedDigestFromAnnotations(ann map[string]string) string {
	if ann == nil {
		return ""
	}
	if v := ann[AnnotUncompressedDigest]; v != "" {
		return v
	}
	return ann[AgodaUncompDig]
}
