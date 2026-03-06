// Package config handles global and project-level configuration for snipe.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config represents snipe configuration settings.
type Config struct {
	// Limit is the default result limit
	Limit int `json:"limit,omitempty"`
	// ContextLines is the default context lines around matches
	ContextLines int `json:"context_lines,omitempty"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		Limit:        50,
		ContextLines: 3,
	}
}

// GlobalConfigPath returns the path to the global config file.
func GlobalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "snipe", "config.json"), nil
}

// ProjectConfigPath returns the path to the project config file.
func ProjectConfigPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".snipe.json")
}

// Load loads and merges configuration from global and project sources.
// Project config overrides global config, which overrides defaults.
func Load(projectRoot string) (*Config, error) {
	cfg := DefaultConfig()

	// Load global config if exists
	globalPath, err := GlobalConfigPath()
	if err == nil {
		if globalCfg, err := loadFile(globalPath); err == nil {
			cfg = merge(cfg, globalCfg)
		}
	}

	// Load project config if exists (overrides global)
	if projectRoot != "" {
		projectPath := ProjectConfigPath(projectRoot)
		if projectCfg, err := loadFile(projectPath); err == nil {
			cfg = merge(cfg, projectCfg)
		}
	}

	return cfg, nil
}

// loadFile loads a config from a JSON file. Returns nil if file doesn't exist.
func loadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path from GlobalConfigPath/ProjectConfigPath
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// merge merges src into dst, with src values taking precedence for non-zero values.
func merge(dst, src *Config) *Config {
	if src == nil {
		return dst
	}

	result := *dst

	if src.Limit > 0 {
		result.Limit = src.Limit
	}
	if src.ContextLines > 0 {
		result.ContextLines = src.ContextLines
	}

	return &result
}
