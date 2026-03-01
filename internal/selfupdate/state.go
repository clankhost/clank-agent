package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const stateFile = "update-state.json"

// UpdateState tracks an in-progress update for crash recovery and rollback.
type UpdateState struct {
	Status      string `json:"status"`       // "pending" or "verified"
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	Attempts    int    `json:"attempts"`
}

// LoadState reads the update state file from the config directory.
// Returns nil if no state file exists.
func LoadState(cfgDir string) *UpdateState {
	path := filepath.Join(cfgDir, stateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s UpdateState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	return &s
}

// SaveState writes the update state file to the config directory.
func SaveState(cfgDir string, s *UpdateState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfgDir, stateFile), data, 0600)
}

// ClearState removes the update state file.
func ClearState(cfgDir string) {
	os.Remove(filepath.Join(cfgDir, stateFile))
}
