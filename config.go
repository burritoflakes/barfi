package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Config is the on-disk shape of ~/.config/barfi/config.json.
// Every field is optional. Fields with zero values are omitted when written
// (via omitempty) so narrower --save invocations do not clobber previously
// saved fields.
type Config struct {
	Server     string `json:"server,omitempty"`
	Token      string `json:"token,omitempty"`
	LocationId string `json:"locationId,omitempty"`
	ParentId   string `json:"parentId,omitempty"`
	Workers    int    `json:"workers,omitempty"`
}

// defaultConfigPath returns the XDG-aware path for barfi's config file.
// Uses os.UserConfigDir() which handles $XDG_CONFIG_HOME, macOS, and Windows.
func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "barfi", "config.json"), nil
}

// loadConfig reads the config file at path. If the file does not exist,
// loadConfig returns the zero-value Config and a nil error — missing is
// not an error for this CLI.
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// saveConfig writes cfg to path with 0600 permissions, creating the
// parent directory with 0700 if it does not exist. The token is
// sensitive, hence the restrictive mode.
func saveConfig(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
