package taskplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// snapshot is the persisted execution progress: the last committed
// world state and the tasks that still have to be accomplished.
type snapshot struct {
	State     []string  `json:"state"`
	Remaining []TaskRef `json:"remaining"`
	Executed  int       `json:"executed"`
	Done      bool      `json:"done"`
}

// loadSnapshot reads a snapshot from path. It returns (nil, nil) when
// the file does not exist, and (nil, err) when it exists but is corrupt.
func loadSnapshot(path string) (*snapshot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("taskplan: cannot read snapshot %s: %w", path, err)
	}
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("taskplan: corrupt snapshot %s: %w", path, err)
	}
	return &s, nil
}

// saveSnapshot atomically writes the snapshot to path (tmp + rename).
func saveSnapshot(path string, s *snapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("taskplan: cannot encode snapshot: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".taskplan-snapshot-*")
	if err != nil {
		return fmt.Errorf("taskplan: cannot create temp snapshot: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("taskplan: cannot write snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("taskplan: cannot close snapshot: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("taskplan: cannot commit snapshot: %w", err)
	}
	return nil
}
