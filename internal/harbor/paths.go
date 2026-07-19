package harbor

import (
	"os"
	"path/filepath"
)

const Version = "0.3.1"

// Dir is the harbor data directory (config, socket, state, logs, shim bin).
func Dir() string {
	if d := os.Getenv("ADB_HARBOR_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".adbharbor"
	}
	return filepath.Join(home, ".adbharbor")
}

func BinDir() string       { return filepath.Join(Dir(), "bin") }
func SocketPath() string   { return filepath.Join(Dir(), "harbor.sock") }
func ConfigPath() string   { return filepath.Join(Dir(), "config.json") }
func StatePath() string    { return filepath.Join(Dir(), "state.json") }
func LogPath() string      { return filepath.Join(Dir(), "daemon.log") }
func HistoryPath() string  { return filepath.Join(Dir(), "history.jsonl") }
func PIDPath() string      { return filepath.Join(Dir(), "daemon.pid") }
func LockPath() string     { return filepath.Join(Dir(), "daemon.lock") }

func EnsureDir() error {
	return os.MkdirAll(BinDir(), 0o700)
}
