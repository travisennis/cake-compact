// Package state remembers the current session for a working directory so the
// wrapper can resume the compacted session on the next turn.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the wrapper's per-directory session pointer.
type State struct {
	SessionID        string `json:"session_id"`
	WorkingDirectory string `json:"working_directory"`
	UpdatedAt        string `json:"updated_at,omitempty"`
}

// DefaultPath returns ~/.local/state/cake-compact/<hash-of-cwd>.json, using
// XDG_STATE_HOME when set.
func DefaultPath(cwd string) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate the home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(cwd))
	name := hex.EncodeToString(sum[:8]) + ".json"
	return filepath.Join(base, "cake-compact", name), nil
}

// Load returns the state file, or nil when it does not exist.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("invalid state file %s: %w", path, err)
	}
	return &state, nil
}

// Save writes the state atomically.
func Save(path string, state *State) error {
	if state.UpdatedAt == "" {
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".cake-compact-state-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}
