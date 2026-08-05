package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

type enableController struct {
	enabled atomic.Bool
	file    string
	mu      sync.Mutex
}

type persistedState struct {
	Enabled bool `json:"enabled"`
}

func newEnableController(defaultEnabled bool, file string) (*enableController, error) {
	controller := &enableController{file: file}
	controller.enabled.Store(defaultEnabled)

	raw, err := os.ReadFile(controller.file)
	if err != nil {
		if os.IsNotExist(err) {
			return controller, nil
		}
		return nil, fmt.Errorf("read stream hold state: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode stream hold state: %w", err)
	}
	controller.enabled.Store(state.Enabled)
	return controller, nil
}

func (c *enableController) Enabled() bool {
	return c != nil && c.enabled.Load()
}

func (c *enableController) Set(enabled bool) error {
	if c == nil {
		return fmt.Errorf("enable controller is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(c.file), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(c.file), ".stream-hold-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("chmod state file: %w", err)
	}
	if err := json.NewEncoder(temp).Encode(persistedState{Enabled: enabled}); err != nil {
		temp.Close()
		return fmt.Errorf("write state file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync state file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close state file: %w", err)
	}
	if err := os.Rename(tempName, c.file); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	c.enabled.Store(enabled)
	return nil
}
