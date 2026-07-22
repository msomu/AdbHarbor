package harbor

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func lookupFrom(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestEnvSessionKey(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		key    string
		source string
	}{
		{"explicit wins over claude",
			map[string]string{"ADB_HARBOR_SESSION": "ci-lane-3", "CLAUDE_SESSION_ID": "abcdef1234"},
			"ci-lane-3", "ADB_HARBOR_SESSION"},
		{"claude id truncated to 8",
			map[string]string{"CLAUDE_SESSION_ID": "abcdef1234567890"},
			"claude-abcdef12", "CLAUDE_SESSION_ID"},
		{"unsafe characters sanitized",
			map[string]string{"ADB_HARBOR_SESSION": "agent 6/run:2"},
			"agent-6-run-2", "ADB_HARBOR_SESSION"},
		{"nothing set", map[string]string{}, "", ""},
	}
	for _, c := range cases {
		key, source := envSessionKey(lookupFrom(c.env))
		if key != c.key || source != c.source {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, key, source, c.key, c.source)
		}
	}
}

// The shim reads its own environment and the proxy reads the client's out of
// `ps -E`. Both must land on the same key: two spellings would split one
// agent in two and make its second command queue behind its own first.
func TestShimAndProxyAgreeOnKey(t *testing.T) {
	const psOutput = `/path/to/adb -s RZGL41JKGFT install app.apk USER=msomu ADB_HARBOR_SESSION=claude-9f3a1c HOME=/Users/msomu`
	fromProc, srcProc := envSessionKey(lookupFrom(parseProcEnv(psOutput)))
	fromShim, srcShim := envSessionKey(lookupFrom(map[string]string{"ADB_HARBOR_SESSION": "claude-9f3a1c"}))
	if fromProc != fromShim || srcProc != srcShim {
		t.Errorf("proxy got (%q, %q), shim got (%q, %q); keys must match",
			fromProc, srcProc, fromShim, srcShim)
	}
	if fromProc != "claude-9f3a1c" {
		t.Errorf("got %q, want claude-9f3a1c", fromProc)
	}
}

func TestParseProcEnv(t *testing.T) {
	env := parseProcEnv(`adb shell am start -n com.foo/.Main PATH=/usr/bin:/bin ADB_HARBOR_SESSION=agent-6 EMPTY= LC_ALL=en_US.UTF-8`)
	if got := env["ADB_HARBOR_SESSION"]; got != "agent-6" {
		t.Errorf("ADB_HARBOR_SESSION = %q, want agent-6", got)
	}
	if got := env["PATH"]; got != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want /usr/bin:/bin", got)
	}
	if _, ok := env["EMPTY"]; ok {
		t.Error("empty value should be dropped")
	}
	if _, ok := env["com.foo/.Main"]; ok {
		t.Error("command arguments must not be read as env")
	}
}

// startHelper spawns this test binary as an idle child carrying env, giving
// classifyClient a real pid to inspect. A shell built-in would not do: macOS
// redacts the environment of platform binaries under SIP, so the process has
// to be one we built.
func startHelper(t *testing.T, env ...string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperIdle")
	cmd.Env = append(append(os.Environ(), "HARBOR_TEST_HELPER=1"), env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Wait until ps can see the child: with no env of our own to look for,
	// visibility in the process table is the readiness signal.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := psInfo(cmd.Process.Pid); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cmd.Process.Pid
}

func TestHelperIdle(t *testing.T) {
	if os.Getenv("HARBOR_TEST_HELPER") != "1" {
		t.Skip("helper process for TestClassifyClient*")
	}
	time.Sleep(10 * time.Second)
}

func TestClassifyClientPrefersEnvOverProcessTree(t *testing.T) {
	pid := startHelper(t, "ADB_HARBOR_SESSION=agent-6")
	session, source, observer := classifyClient(pid, DefaultConfig())
	if observer {
		t.Fatal("helper classified as an observer")
	}
	if session != "agent-6" || source != "ADB_HARBOR_SESSION" {
		t.Errorf("got (%q, %q), want (agent-6, ADB_HARBOR_SESSION)", session, source)
	}
}

// An explicit key must never promote a passive tool into a lease holder: a
// screen mirror carrying an agent's environment is still a screen mirror.
func TestClassifyClientObserverBeatsEnv(t *testing.T) {
	pid := startHelper(t, "ADB_HARBOR_SESSION=agent-6")
	name, _, ok := psInfo(pid)
	if !ok {
		t.Fatalf("psInfo(%d) failed", pid)
	}
	cfg := DefaultConfig()
	cfg.ObserverProcs = []string{name}
	_, _, observer := classifyClient(pid, cfg)
	if !observer {
		t.Errorf("process %q with an observer config was not treated as an observer", name)
	}
}

func TestClassifyClientFallsBackToProcessTree(t *testing.T) {
	pid := startHelper(t)
	session, source, _ := classifyClient(pid, DefaultConfig())
	if source != "process tree" {
		t.Errorf("source = %q, want process tree", source)
	}
	if session == "" {
		t.Error("empty session key")
	}
}

func TestYouSuffix(t *testing.T) {
	if got := youSuffix("agent-6", "agent-6"); got != " <- you" {
		t.Errorf("own lease got %q", got)
	}
	if got := youSuffix("agent-6", "agent-7"); got != "" {
		t.Errorf("other session got %q, want empty", got)
	}
	// An unresolved caller key must not claim every row.
	if got := youSuffix("", ""); got != "" {
		t.Errorf("empty session got %q, want empty", got)
	}
}
