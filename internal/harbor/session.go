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
	s, _ := DetectSessionObserver(cfg)
	return s
}

// DetectSessionForPID walks the process tree from pid (inclusive) looking
// for a known agent process; used both by the shim (from its parent) and by
// the ADB server proxy (from the connecting client's pid).
func DetectSessionForPID(pid int, cfg *Config) string {
	s, _ := classifyPID(pid, cfg)
	return s
}

// classifyPID resolves the session key for a process and whether it belongs
// to an observer tool (scrcpy and friends). The walk checks each ancestor
// against both lists; the NEAREST match wins, so scrcpy spawned by an agent
// is still an observer.
func classifyPID(pid int, cfg *Config) (session string, observer bool) {
	cur := pid
	for depth := 0; depth < 25 && cur > 1; depth++ {
		name, ppid, ok := psInfo(cur)
		if !ok {
			break
		}
		if isObserverProc(name, cfg.ObserverProcs) {
			return fmt.Sprintf("%s-%d", name, cur), true
		}
		if matchesAgent(name, cfg.AgentProcs) {
			return fmt.Sprintf("%s-%d", name, cur), false
		}
		cur = ppid
	}
	// No agent ancestor: key on the starting process itself, which is
	// stable for its lifetime (a shell, a gradle daemon, ...).
	if name, _, ok := psInfo(pid); ok {
		return fmt.Sprintf("%s-%d", name, pid), false
	}
	return fmt.Sprintf("pid-%d", pid), false
}

// DetectSessionObserver is DetectSession plus the observer flag for the
// calling context (shim): observer commands must not take leases.
func DetectSessionObserver(cfg *Config) (string, bool) {
	if v := os.Getenv("ADB_HARBOR_SESSION"); v != "" {
		return sanitizeKey(v), false
	}
	if v := os.Getenv("CLAUDE_SESSION_ID"); v != "" {
		if len(v) > 8 {
			v = v[:8]
		}
		return "claude-" + sanitizeKey(v), false
	}
	return classifyPID(os.Getppid(), cfg)
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
