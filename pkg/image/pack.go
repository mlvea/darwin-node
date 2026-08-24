package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PackDir writes a Darwin-Node config.json next to disk.img/aux.img.
func PackDir(dir string, cfg Config) error {
	if cfg.OS == "" {
		cfg.OS = "darwin"
	}
	if cfg.MediaType == "" {
		cfg.MediaType = MediaTypeConfig
	}
	if len(cfg.Storage) == 0 {
		cfg.Storage = []BlobRef{
			{MediaType: MediaTypeDisk, File: "disk.img"},
			{MediaType: MediaTypeAux, File: "aux.img"},
		}
	}
	if cfg.HardwareModelData == "" {
		return fmt.Errorf("pack: hardwareModelData is required (pass --hardware-model)")
	}
	for _, s := range cfg.Storage {
		if _, err := os.Stat(filepath.Join(dir, s.File)); err != nil {
			return fmt.Errorf("pack: missing %s: %w", s.File, err)
		}
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o644)
}
