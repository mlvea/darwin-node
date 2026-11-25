package simvz

import (
	"encoding/json"
	"os"
)

// saveDescriptor is the JSON body of a simulated save file. Restore
// requires the same machine id, hardware model, MAC, CPU count, and
// memory size. A mismatch is VZErrorRestore (code 12).
type saveDescriptor struct {
	Version       int    `json:"version"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	UID           string `json:"uid"`
	HardwareModel string `json:"hardware_model"`
	MAC           string `json:"mac"`
	CPU           uint   `json:"cpu"`
	MemoryBytes   uint64 `json:"memory_bytes"`
}

func (m *machine) descriptor() saveDescriptor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return saveDescriptor{
		Version:       1,
		Namespace:     m.spec.ID.Namespace,
		Name:          m.spec.ID.Name,
		UID:           m.spec.ID.UID,
		HardwareModel: m.spec.HardwareModelData,
		MAC:           m.mac,
		CPU:           m.spec.CPU,
		MemoryBytes:   m.spec.MemoryBytes,
	}
}

func (m *machine) matches(d saveDescriptor) bool {
	want := m.descriptor()
	want.Version = d.Version
	return d.Version == 1 && d == want
}

func (m *machine) writeSnapshot(path string) error {
	body, err := json.MarshalIndent(m.descriptor(), "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o600)
}

func readSnapshot(path string) (saveDescriptor, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return saveDescriptor{}, err
	}
	var d saveDescriptor
	if err := json.Unmarshal(body, &d); err != nil {
		return saveDescriptor{}, err
	}
	return d, nil
}
