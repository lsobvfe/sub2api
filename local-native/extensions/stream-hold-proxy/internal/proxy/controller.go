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

type holdKeyRegistry struct {
	mu   sync.RWMutex
	file string
	keys map[string]struct{}
}

type persistedKeys struct {
	KeyHashes []string `json:"key_hashes"`
}

func newHoldKeyRegistry(file string) (*holdKeyRegistry, error) {
	registry := &holdKeyRegistry{file: file, keys: make(map[string]struct{})}
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return registry, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read stream hold key registry: %w", err)
	}
	var state persistedKeys
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode stream hold key registry: %w", err)
	}
	for _, hash := range state.KeyHashes {
		registry.keys[hash] = struct{}{}
	}
	return registry, nil
}

func (r *holdKeyRegistry) Contains(hash string) bool {
	r.mu.RLock()
	_, ok := r.keys[hash]
	r.mu.RUnlock()
	return ok
}

func (r *holdKeyRegistry) Set(hash string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if enabled {
		r.keys[hash] = struct{}{}
	} else {
		delete(r.keys, hash)
	}
	return r.persistLocked()
}

func (r *holdKeyRegistry) Replace(hashes []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		r.keys[hash] = struct{}{}
	}
	return r.persistLocked()
}

func (r *holdKeyRegistry) persistLocked() error {
	hashes := make([]string, 0, len(r.keys))
	for hash := range r.keys {
		hashes = append(hashes, hash)
	}
	data, err := json.Marshal(persistedKeys{KeyHashes: hashes})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.file), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(r.file), ".stream-hold-keys-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, r.file)
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
