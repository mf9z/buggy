// Package config loads and saves buggy's settings file.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const (
	ModeBYOK  = "byok"
	ModeProxy = "proxy"
)

// Config is the on-disk settings file (~/.config/buggy/config.json).
type Config struct {
	Mode      string                     `json:"mode"`
	Provider  string                     `json:"provider"`
	Providers map[string]*ProviderConfig `json:"providers"`
	Proxy     ProxyConfig                `json:"proxy"`
	Audit     AuditConfig                `json:"audit"`

	path string
}

// ProviderConfig holds the per-provider BYOK settings.
type ProviderConfig struct {
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

// ProxyConfig routes requests through a local agent CLI or an HTTP proxy.
type ProxyConfig struct {
	// Backend is one of: claude-code, codex, gemini-cli, command, http.
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	// Command is a shell command used by the "command" backend. It receives
	// the prompt on stdin and must write the reply to stdout.
	Command string `json:"command,omitempty"`
	// URL, Format and APIKey configure the "http" backend (e.g. LiteLLM,
	// claude-code-proxy, a corporate gateway). Format is openai or anthropic.
	URL    string `json:"url,omitempty"`
	Format string `json:"format,omitempty"`
	APIKey string `json:"api_key,omitempty"`
}

// AuditConfig tunes the audit engine.
type AuditConfig struct {
	Concurrency int    `json:"concurrency"`
	MaxFileKB   int    `json:"max_file_kb"`
	BatchKB     int    `json:"batch_kb"`
	Security    bool   `json:"security"`
	Performance bool   `json:"performance"`
	Bugs        bool   `json:"bugs"`
	MinSeverity string `json:"min_severity"`
}

func Default() *Config {
	return &Config{
		Mode:      ModeBYOK,
		Provider:  "anthropic",
		Providers: map[string]*ProviderConfig{},
		Proxy:     ProxyConfig{Backend: "claude-code", Format: "openai"},
		Audit: AuditConfig{
			Concurrency: 4,
			MaxFileKB:   256,
			BatchKB:     96,
			Security:    true,
			Performance: true,
			Bugs:        true,
			MinSeverity: "low",
		},
	}
}

// Path returns the settings file location, honoring $BUGGY_CONFIG.
func Path() (string, error) {
	if p := os.Getenv("BUGGY_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "buggy", "config.json"), nil
}

// Load reads the settings file, returning defaults if it does not exist.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	c := Default()
	c.path = p
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	c.normalize()
	return c, nil
}

func (c *Config) normalize() {
	d := Default()
	if c.Mode != ModeBYOK && c.Mode != ModeProxy {
		c.Mode = d.Mode
	}
	if c.Provider == "" {
		c.Provider = d.Provider
	}
	if c.Providers == nil {
		c.Providers = map[string]*ProviderConfig{}
	}
	if c.Proxy.Backend == "" {
		c.Proxy.Backend = d.Proxy.Backend
	}
	if c.Proxy.Format == "" {
		c.Proxy.Format = d.Proxy.Format
	}
	if c.Audit.Concurrency <= 0 {
		c.Audit.Concurrency = d.Audit.Concurrency
	}
	if c.Audit.MaxFileKB <= 0 {
		c.Audit.MaxFileKB = d.Audit.MaxFileKB
	}
	if c.Audit.BatchKB <= 0 {
		c.Audit.BatchKB = d.Audit.BatchKB
	}
	if !c.Audit.Security && !c.Audit.Performance && !c.Audit.Bugs {
		c.Audit.Security, c.Audit.Performance, c.Audit.Bugs = true, true, true
	}
	switch c.Audit.MinSeverity {
	case "low", "medium", "high":
	default:
		c.Audit.MinSeverity = "low"
	}
}

// Save writes the settings file with owner-only permissions, since it can
// contain API keys.
func (c *Config) Save() error {
	if c.path == "" {
		p, err := Path()
		if err != nil {
			return err
		}
		c.path = p
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func (c *Config) File() string { return c.path }

// ProviderSettings returns (creating if needed) the settings for a provider.
func (c *Config) ProviderSettings(id string) *ProviderConfig {
	pc, ok := c.Providers[id]
	if !ok || pc == nil {
		pc = &ProviderConfig{}
		c.Providers[id] = pc
	}
	return pc
}
