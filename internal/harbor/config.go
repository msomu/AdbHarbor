package harbor

import (
	"encoding/json"
	"os"
)

type Config struct {
	// RealADB is the absolute path to the real adb binary the shim wraps.
	RealADB string `json:"real_adb"`
	// IdleTTLSec is how long a lease lingers after its last command before
	// the device is handed to the next session in the queue. The linger
	// window keeps a device owned across an agent's consecutive commands.
	IdleTTLSec int `json:"idle_ttl_seconds"`
	// WaitSec is the default max time a command waits for a busy device.
	WaitSec int `json:"wait_timeout_seconds"`
	// ExplicitTTLSec is the default TTL for `adbharbor acquire` leases.
	ExplicitTTLSec int `json:"explicit_ttl_seconds"`
	// AgentProcs are process names matched while walking up the process
	// tree to identify which agent a command belongs to. The nearest
	// matching ancestor's "name-pid" becomes the session key.
	AgentProcs []string `json:"agent_process_names"`
}

func DefaultConfig() *Config {
	return &Config{
		IdleTTLSec:     60,
		WaitSec:        600,
		ExplicitTTLSec: 900,
		AgentProcs: []string{
			"claude", "node", "bun", "deno", "codex",
			"gemini", "aider", "goose", "amp", "cursor", "copilot",
		},
	}
}

func LoadConfig() *Config {
	cfg := DefaultConfig()
	if data, err := os.ReadFile(ConfigPath()); err == nil {
		_ = json.Unmarshal(data, cfg)
	}
	if v := os.Getenv("ADB_HARBOR_ADB"); v != "" {
		cfg.RealADB = v
	}
	return cfg
}

func (c *Config) Save() error {
	if err := EnsureDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), append(data, '\n'), 0o600)
}
