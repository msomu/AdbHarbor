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
	// ProxyEnabled makes the daemon take over the default ADB server port
	// (5037) and broker EVERY adb client — CLIs at any path, Maestro/dadb,
	// ddmlib — with the real adb server moved to AdbServerPort. This is
	// what makes third-party tools harbor-agnostic: on machines without
	// AdbHarbor they hit a normal adb server, here they hit the broker.
	ProxyEnabled bool `json:"proxy_enabled"`
	// ProxyPort is where the harbor proxy listens (the ADB default, 5037).
	ProxyPort int `json:"proxy_port"`
	// AdbServerPort is where the real adb server is parked.
	AdbServerPort int `json:"adb_server_port"`
	// ExemptShell lists shell/exec command prefixes that are read-only and
	// run without a lease (so device-inventory heartbeats from tools like
	// DroidRunner never squat a device or stall behind a busy one).
	ExemptShell []string `json:"exempt_shell_prefixes"`
	// CleanupEnabled uninstalls packages that appeared on a device during a
	// session when that session's lease ends (snapshot diff), leaving the
	// device clean for the next job. Off by default; toggle with
	// `adbharbor cleanup on|off`.
	CleanupEnabled bool `json:"cleanup_enabled"`
	// ProtectedPackages are package-name prefixes cleanup never uninstalls.
	ProtectedPackages []string `json:"protected_package_prefixes"`
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
		ProxyEnabled:  true,
		ProxyPort:     5037,
		AdbServerPort: 5038,
		ExemptShell: []string{
			"getprop", "dumpsys", "pm list", "pm path", "settings get",
			"wm size", "wm density", "cmd package list", "ime list",
			"getenforce", "echo", "uptime",
		},
		CleanupEnabled: false,
		ProtectedPackages: []string{
			"android", "com.android.", "com.google.", "com.samsung.", "com.sec.",
		},
	}
}

// LoadRawConfig reads the config file without environment overrides — use
// this when mutating and re-saving so env vars don't get persisted.
func LoadRawConfig() *Config {
	cfg := DefaultConfig()
	if data, err := os.ReadFile(ConfigPath()); err == nil {
		_ = json.Unmarshal(data, cfg)
	}
	return cfg
}

// ClientServerPort is the ANDROID_ADB_SERVER_PORT the shim should hand the
// real adb CLI: with the proxy owning 5037, shim-managed commands go
// straight to the real server (their lease is already held).
// 0 means "leave the environment alone".
func (c *Config) ClientServerPort() int {
	if c.ProxyEnabled {
		return c.AdbServerPort
	}
	return 0
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
