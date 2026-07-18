package harbor

import "testing"

func TestParseInvocation(t *testing.T) {
	cases := []struct {
		args    []string
		serial  string
		command string
		needs   bool
	}{
		{[]string{"devices"}, "", "devices", false},
		{[]string{"-s", "ABC", "shell", "ls"}, "ABC", "shell", true},
		{[]string{"-s", "ABC", "install", "app.apk"}, "ABC", "install", true},
		{[]string{"version"}, "", "version", false},
		{[]string{"kill-server"}, "", "kill-server", false},
		{[]string{"-H", "host", "-P", "5037", "-s", "X", "logcat"}, "X", "logcat", true},
		{[]string{"-d", "shell"}, "", "shell", true},
		{[]string{"connect", "192.168.1.5:5555"}, "", "connect", false},
		{[]string{}, "", "", false},
		{[]string{"push", "a", "/sdcard/"}, "", "push", true},
	}
	for _, c := range cases {
		inv := ParseInvocation(c.args)
		if inv.Serial != c.serial || inv.Command != c.command || inv.NeedsDevice() != c.needs {
			t.Errorf("ParseInvocation(%v) = {serial:%q command:%q needs:%v}, want {%q %q %v}",
				c.args, inv.Serial, inv.Command, inv.NeedsDevice(), c.serial, c.command, c.needs)
		}
	}
}

func TestParseInvocationFlags(t *testing.T) {
	inv := ParseInvocation([]string{"-d", "shell"})
	if !inv.UseUSB {
		t.Error("-d should set UseUSB")
	}
	inv = ParseInvocation([]string{"-e", "shell"})
	if !inv.UseEmulator {
		t.Error("-e should set UseEmulator")
	}
	inv = ParseInvocation([]string{"-t", "7", "shell"})
	if inv.TransportID != "7" {
		t.Error("-t should set TransportID")
	}
}

func TestMatchesAgent(t *testing.T) {
	agents := DefaultConfig().AgentProcs
	for name, want := range map[string]bool{
		"claude":             true,
		"claude code helper": true, // substring match for distinctive names
		"node":               true,
		"codex":              true,
		"zsh":                false,
		"bash":               false,
		"nodenv":             false, // "node" is exact-match only
		"terminal":           false,
	} {
		if got := matchesAgent(name, agents); got != want {
			t.Errorf("matchesAgent(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSanitizeKey(t *testing.T) {
	if got := sanitizeKey("my session/1!"); got != "my-session-1-" {
		t.Errorf("sanitizeKey = %q", got)
	}
}
