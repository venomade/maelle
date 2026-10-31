package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// config is the REPL's persisted state (model, stream mode, sampling params,
// client-side system prompt). It lives outside the conversation history so
// /save and /load stay portable.
type config struct {
	Model     string   `json:"model"`
	Stream    bool     `json:"stream"`
	Temp      *float64 `json:"temperature,omitempty"`
	MaxTokens *int     `json:"max_tokens,omitempty"`
	System    string   `json:"system,omitempty"`
	Reasoning string   `json:"reasoning_effort,omitempty"`
}

func defaultConfig() config {
	return config{Model: getenv("MODEL", "mistral-small-latest"), Stream: true}
}

func configPath() string {
	if p := os.Getenv("MAELLE_CONFIG"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "maelle", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "maelle", "config.json")
}

func loadConfig() config {
	cfg := defaultConfig()
	p := configPath()
	if p == "" {
		return cfg
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cfg
	}
	var saved config
	if json.Unmarshal(b, &saved) != nil {
		return cfg
	}
	if saved.Model != "" {
		cfg.Model = saved.Model
	}
	cfg.Stream = saved.Stream
	cfg.Temp = saved.Temp
	cfg.MaxTokens = saved.MaxTokens
	if saved.System != "" {
		cfg.System = saved.System
	}
	if saved.Reasoning != "" {
		cfg.Reasoning = saved.Reasoning
	}
	return cfg
}

func (c config) save() {
	p := configPath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(p, b, 0o600)
}

// historyPath is the persistent line-history file for the raw-mode editor.
func historyPath() string {
	if p := os.Getenv("MAELLE_HISTORY"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "maelle", "history")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "maelle", "history")
}
