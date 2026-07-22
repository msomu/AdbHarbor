package harbor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/msomu/AdbHarbor/skills"
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
	noSkill := false
	forceSkill := false
	for _, a := range args {
		switch a {
		case "--no-rc":
			noRC = true
		case "--no-skill":
			noSkill = true
		case "--force-skill":
			forceSkill = true
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

	skillPath, skillNote := "", ""
	if !noSkill {
		skillPath, skillNote = installSkill(forceSkill)
	}

	fmt.Printf("adbharbor %s installed\n", Version)
	fmt.Printf("  shim:      %s -> adbharbor\n", link)
	fmt.Printf("  real adb:  %s\n", real)
	fmt.Printf("  config:    %s\n", ConfigPath())
	if rcPath != "" {
		fmt.Printf("  PATH:      patched %s (open a new shell or `exec zsh` to activate)\n", rcPath)
	}
	if skillPath != "" {
		fmt.Printf("  skill:     %s\n", skillPath)
	}
	if skillNote != "" {
		fmt.Printf("  skill:     %s\n", skillNote)
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
	if removed := uninstallSkill(); removed != "" {
		fmt.Println("removed skill", removed)
	}
	fmt.Println("adbharbor shim removed; data dir kept at", Dir())
	return nil
}

// skillStamp marks a skill directory as installed by us, so uninstall and
// upgrade only ever touch our own copy and never a hand-written one.
const skillStamp = ".adbharbor-version"

func skillDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills", "adbharbor")
}

// installSkill places the bundled agent skill in ~/.claude/skills.
//
// This has to happen here rather than in packaging: Homebrew runs formula
// installs in a sandbox that forbids writing to $HOME, so `brew install`
// can never deliver a skill. `adbharbor install` is already a required
// second step, which makes it the one place a default install can teach
// agents what a queued adb command means.
//
// Never fatal: a missing or unwritable ~/.claude just means this machine
// runs no Claude Code, which is no reason to fail the shim install.
func installSkill(force bool) (path, note string) {
	dir := skillDir()
	if dir == "" {
		return "", ""
	}
	// Only for machines that already run Claude Code — don't conjure a
	// config directory for someone who has none.
	claude := filepath.Dir(filepath.Dir(dir))
	if _, err := os.Stat(claude); err != nil {
		return "", ""
	}
	dst := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(dst); err == nil && !force {
		if _, err := os.Stat(filepath.Join(dir, skillStamp)); err != nil {
			return "", dst + " exists and was not installed by us — left alone (--force-skill to replace)"
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "could not create " + dir + ": " + err.Error()
	}
	if err := os.WriteFile(dst, skills.ClaudeSkill, 0o644); err != nil {
		return "", "could not write " + dst + ": " + err.Error()
	}
	if err := os.WriteFile(filepath.Join(dir, skillStamp), []byte(Version+"\n"), 0o644); err != nil {
		return dst, "installed, but could not stamp version: " + err.Error()
	}
	return dst, ""
}

// uninstallSkill removes the skill only when our stamp is present.
func uninstallSkill() string {
	dir := skillDir()
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, skillStamp)); err != nil {
		return ""
	}
	if err := os.RemoveAll(dir); err != nil {
		return ""
	}
	return dir
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
