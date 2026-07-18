package harbor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DetectSession derives a stable session key for the calling agent.
//
// Priority: explicit env override, then a walk up the process tree looking
// for a known agent process (each Claude/Codex/etc. session is one
// long-lived process, while the shells it spawns per command are ephemeral).
// The nearest matching ancestor gives "name-pid", which stays stable across
// every command that agent runs and differs between two agents even of the
// same kind. Fallback: the immediate parent shell.
func DetectSession(cfg *Config) string {
	if v := os.Getenv("ADB_HARBOR_SESSION"); v != "" {
		return sanitizeKey(v)
	}
	if v := os.Getenv("CLAUDE_SESSION_ID"); v != "" {
		if len(v) > 8 {
			v = v[:8]
		}
		return "claude-" + sanitizeKey(v)
	}
	pid := os.Getppid()
	for depth := 0; depth < 25 && pid > 1; depth++ {
		name, ppid, ok := psInfo(pid)
		if !ok {
			break
		}
		if matchesAgent(name, cfg.AgentProcs) {
			return fmt.Sprintf("%s-%d", name, pid)
		}
		pid = ppid
	}
	return fmt.Sprintf("shell-%d", os.Getppid())
}

// HolderDesc is the human-readable owner label shown to other sessions.
func HolderDesc(session string) string {
	wd, err := os.Getwd()
	if err != nil {
		return session
	}
	return fmt.Sprintf("%s (%s)", session, filepath.Base(wd))
}

func psInfo(pid int) (name string, ppid int, ok bool) {
	out, err := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", 0, false
	}
	line := strings.TrimSpace(string(out))
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	ppid, err = strconv.Atoi(fields[0])
	if err != nil {
		return "", 0, false
	}
	comm := strings.Join(fields[1:], " ")
	name = strings.ToLower(filepath.Base(comm))
	return name, ppid, true
}

func matchesAgent(name string, agents []string) bool {
	for _, a := range agents {
		if name == a {
			return true
		}
		// Substring match only for distinctive names ("claude" matches
		// "Claude Code Helper"); short generic ones need exact match.
		if len(a) >= 5 && strings.Contains(name, a) {
			return true
		}
	}
	return false
}

func sanitizeKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
}
