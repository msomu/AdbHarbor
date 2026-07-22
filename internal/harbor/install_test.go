package harbor

import (
	"github.com/msomu/AdbHarbor/skills"
	"os"
	"path/filepath"
	"testing"
)

func skillPaths(home string) (dir, md, stamp string) {
	dir = filepath.Join(home, ".claude", "skills", "adbharbor")
	return dir, filepath.Join(dir, "SKILL.md"), filepath.Join(dir, skillStamp)
}

// A machine that does not run Claude Code gets no ~/.claude conjured for it.
func TestInstallSkillSkipsMachinesWithoutClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, note := installSkill(false)
	if path != "" || note != "" {
		t.Errorf("got (%q, %q), want no action", path, note)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Error("installer created ~/.claude on a machine that had none")
	}
}

func TestInstallSkillWritesSkillAndStamp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	path, note := installSkill(false)
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}
	_, md, stamp := skillPaths(home)
	if path != md {
		t.Errorf("path = %q, want %q", path, md)
	}
	got, err := os.ReadFile(md)
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if string(got) != string(skills.ClaudeSkill) {
		t.Error("installed skill does not match the embedded copy")
	}
	if v, err := os.ReadFile(stamp); err != nil || string(v) != Version+"\n" {
		t.Errorf("stamp = %q (err %v), want %q", v, err, Version+"\n")
	}
}

// A skill the user wrote or manages themselves is never overwritten by an
// install or an upgrade — only by an explicit --force-skill.
func TestInstallSkillLeavesUnstampedSkillAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, md, _ := skillPaths(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := []byte("---\nname: adbharbor\n---\nmy own notes\n")
	if err := os.WriteFile(md, mine, 0o644); err != nil {
		t.Fatal(err)
	}

	path, note := installSkill(false)
	if path != "" || note == "" {
		t.Errorf("got (%q, %q), want a skip with an explanation", path, note)
	}
	got, _ := os.ReadFile(md)
	if string(got) != string(mine) {
		t.Fatal("hand-written skill was overwritten")
	}

	// Explicit force replaces it and takes ownership.
	if path, note := installSkill(true); path != md || note != "" {
		t.Fatalf("force install got (%q, %q)", path, note)
	}
	got, _ = os.ReadFile(md)
	if string(got) == string(mine) {
		t.Error("--force-skill did not replace the file")
	}
}

func TestUninstallSkillOnlyRemovesOurOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, md, _ := skillPaths(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(md, []byte("hand written\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if removed := uninstallSkill(); removed != "" {
		t.Errorf("removed %q, want to leave an unstamped skill alone", removed)
	}
	if _, err := os.Stat(md); err != nil {
		t.Error("unstamped skill was deleted")
	}

	// Once we own it, uninstall cleans it up.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, note := installSkill(true); note != "" {
		t.Fatalf("force install: %s", note)
	}
	if removed := uninstallSkill(); removed != dir {
		t.Errorf("removed %q, want %q", removed, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("skill directory survived uninstall")
	}
}
