package harbor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const rcBegin = "# >>> adbharbor >>>"
const rcEnd = "# <<< adbharbor <<<"

// CmdInstall sets up the transparent shim:
//  1. copy this binary to ~/.adbharbor/bin/adbharbor
//  2. symlink ~/.adbharbor/bin/adb -> adbharbor
//  3. discover and pin the real adb in config
//  4. prepend ~/.adbharbor/bin to PATH via the shell rc
func CmdInstall(args []string) error {
	noRC := false
	for _, a := range args {
		if a == "--no-rc" {
			noRC = true
		}
	}
	if err := EnsureDir(); err != nil {
		return err
	}

	// Stop any running daemon so the next command starts the new binary.
	_ = stopQuietly()

	self, err := selfPath()
	if err != nil {
		return err
	}
	dst := filepath.Join(BinDir(), "adbharbor")
	if self != dst {
		if err := copyFile(self, dst, 0o755); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
	}
	link := filepath.Join(BinDir(), "adb")
	os.Remove(link)
	if err := os.Symlink("adbharbor", link); err != nil {
		return fmt.Errorf("symlink adb: %w", err)
	}

	// Keep an already-pinned real adb (a reinstall must not silently switch
	// binaries); discover only when unset or gone.
	cfg := LoadConfig()
	real := cfg.RealADB
	if real == "" || !isExecutable(real) || isSelf(real) {
		var err error
		real, err = DiscoverRealADB()
		if err != nil {
			return fmt.Errorf("%w\ninstall the Android platform-tools first, then re-run `adbharbor install`", err)
		}
	}
	cfg.RealADB = real
	if err := cfg.Save(); err != nil {
		return err
	}

	rcPath := ""
	if !noRC {
		rcPath, err = patchRC()
		if err != nil {
			return err
		}
	}

	fmt.Printf("adbharbor %s installed\n", Version)
	fmt.Printf("  shim:      %s -> adbharbor\n", link)
	fmt.Printf("  real adb:  %s\n", real)
	fmt.Printf("  config:    %s\n", ConfigPath())
	if rcPath != "" {
		fmt.Printf("  PATH:      patched %s (open a new shell or `exec zsh` to activate)\n", rcPath)
	}
	fmt.Println("\nEvery `adb` command now goes through the harbor: device-targeted")
	fmt.Println("commands take an exclusive per-session lease and queue when busy.")
	fmt.Println("Callers using an absolute adb path bypass the shim — point them at")
	fmt.Println("plain `adb` or " + link + ".")
	return nil
}

func CmdUninstall() error {
	_ = stopQuietly()
	os.Remove(filepath.Join(BinDir(), "adb"))
	os.Remove(filepath.Join(BinDir(), "adbharbor"))
	if rc := rcFile(); rc != "" {
		if err := stripRC(rc); err != nil {
			return err
		}
	}
	fmt.Println("adbharbor shim removed; data dir kept at", Dir())
	return nil
}

func stopQuietly() error {
	data, err := os.ReadFile(PIDPath())
	if err != nil {
		return nil
	}
	var pid int
	fmt.Sscanf(string(data), "%d", &pid)
	if pid > 1 {
		if p, err := os.FindProcess(pid); err == nil {
			p.Signal(os.Interrupt)
		}
	}
	return nil
}

func rcFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	shell := filepath.Base(os.Getenv("SHELL"))
	switch shell {
	case "bash":
		return filepath.Join(home, ".bashrc")
	default:
		return filepath.Join(home, ".zshrc")
	}
}

func patchRC() (string, error) {
	rc := rcFile()
	if rc == "" {
		return "", nil
	}
	data, _ := os.ReadFile(rc)
	if strings.Contains(string(data), rcBegin) {
		return rc, nil // already installed
	}
	block := fmt.Sprintf("\n%s\nexport PATH=\"%s:$PATH\"\n%s\n", rcBegin, BinDir(), rcEnd)
	f, err := os.OpenFile(rc, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		return "", err
	}
	return rc, nil
}

func stripRC(rc string) error {
	data, err := os.ReadFile(rc)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	var out []string
	skip := false
	for _, ln := range lines {
		switch {
		case strings.TrimSpace(ln) == rcBegin:
			skip = true
		case strings.TrimSpace(ln) == rcEnd:
			skip = false
		case !skip:
			out = append(out, ln)
		}
	}
	return os.WriteFile(rc, []byte(strings.Join(out, "\n")), 0o644)
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
