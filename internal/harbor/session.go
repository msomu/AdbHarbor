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
	if key, _ := envSessionKey(os.Getenv); key != "" {
		return key, false
	}
	return classifyPID(os.Getppid(), cfg)
}

// DetectSessionSource is DetectSession plus where the key came from, for
// `adbharbor whoami`.
func DetectSessionSource(cfg *Config) (key, source string) {
	if key, src := envSessionKey(os.Getenv); key != "" {
		return key, src
	}
	key, _ = classifyPID(os.Getppid(), cfg)
	return key, "process tree"
}

// envSessionKey derives a session key from an explicit environment
// override. Every entry point goes through it — the shim reading its own
// environment, the proxy reading a client's, the CLI reporting identity —
// so one agent resolves to one key whichever path its commands take. Two
// spellings of the same key would silently split an agent in two: its
// second command would queue behind its own first one.
func envSessionKey(lookup func(string) string) (key, source string) {
	if v := lookup("ADB_HARBOR_SESSION"); v != "" {
		return sanitizeKey(v), "ADB_HARBOR_SESSION"
	}
	if v := lookup("CLAUDE_SESSION_ID"); v != "" {
		if len(v) > 8 {
			v = v[:8]
		}
		return "claude-" + sanitizeKey(v), "CLAUDE_SESSION_ID"
	}
	return "", ""
}

// classifyClient resolves the session for a process connecting to the
// proxy. Observer status is settled first: a screen mirror must never take
// a lease, whatever its environment says. An explicit key in the client's
// environment then wins over the process tree, because the environment is
// inherited and survives the spawning shell exiting — which is exactly when
// the tree walk degrades to a per-command key and an agent starts queueing
// behind its own lingering lease.
func classifyClient(pid int, cfg *Config) (session, source string, observer bool) {
	tree, observer := classifyPID(pid, cfg)
	if observer {
		return tree, "observer", true
	}
	if key, src := sessionFromProcEnv(pid); key != "" {
		return key, src, false
	}
	return tree, "process tree", false
}

// sessionFromProcEnv reads another process's environment. macOS has no
// /proc; `ps -E` exposes the environment of same-user processes (platform
// binaries are redacted under SIP, but adb and the tools that embed it are
// not). Empty means "nothing explicit set" — never an error worth failing
// a command over.
func sessionFromProcEnv(pid int) (key, source string) {
	out, err := exec.Command("ps", "-Eww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", ""
	}
	env := parseProcEnv(string(out))
	return envSessionKey(func(k string) string { return env[k] })
}

// parseProcEnv pulls KEY=VALUE tokens out of `ps -E` output, where the
// environment trails the command line. A value containing a space is
// truncated at the space and an argument shaped like an assignment is taken
// as env; neither matters for session keys, which are single tokens.
func parseProcEnv(s string) map[string]string {
	env := map[string]string{}
	for _, tok := range strings.Fields(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok || v == "" || !isEnvName(k) {
			continue
		}
		if _, dup := env[k]; !dup {
			env[k] = v
		}
	}
	return env
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
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
