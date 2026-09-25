package harbor

import "testing"

func TestClassifyPIDSkipsEphemeralAdbClient(t *testing.T) {
	cfg := DefaultConfig()
	psInfoHook = func(pid int) (string, int, bool) {
		switch pid {
		case 500:
			return "adb", 400, true
		case 400:
			return "java", 1, true
		default:
			return "", 0, false
		}
	}
	defer func() { psInfoHook = nil }()

	session, observer := classifyPID(500, cfg)
	if observer {
		t.Fatal("expected non-observer")
	}
	if session != "java-400" {
		t.Fatalf("session=%q want java-400", session)
	}
}

func TestClassifyPIDDifferentParentsDoNotShareSession(t *testing.T) {
	cfg := DefaultConfig()
	psInfoHook = func(pid int) (string, int, bool) {
		switch pid {
		case 501:
			return "adb", 410, true
		case 502:
			return "adb", 420, true
		case 410:
			return "gradle", 1, true
		case 420:
			return "maestro", 1, true
		default:
			return "", 0, false
		}
	}
	defer func() { psInfoHook = nil }()

	s1, _ := classifyPID(501, cfg)
	s2, _ := classifyPID(502, cfg)
	if s1 == s2 {
		t.Fatalf("sessions should differ: both %q", s1)
	}
	if s1 != "gradle-410" || s2 != "maestro-420" {
		t.Fatalf("got %q and %q", s1, s2)
	}
}

func TestClassifyPIDOnlyLaunchdAncestorIsUnique(t *testing.T) {
	cfg := DefaultConfig()
	psInfoHook = func(pid int) (string, int, bool) {
		switch pid {
		case 600:
			return "adb", 1, true
		case 1:
			return "launchd", 0, true
		default:
			return "", 0, false
		}
	}
	defer func() { psInfoHook = nil }()

	s1, _ := classifyPID(600, cfg)
	s2, _ := classifyPID(601, cfg)
	if s1 != "pid-600" {
		t.Fatalf("session=%q want pid-600", s1)
	}
	psInfoHook = func(pid int) (string, int, bool) {
		switch pid {
		case 601:
			return "adb", 1, true
		case 1:
			return "launchd", 0, true
		default:
			return "", 0, false
		}
	}
	s2, _ = classifyPID(601, cfg)
	if s2 != "pid-601" {
		t.Fatalf("session=%q want pid-601", s2)
	}
	if s1 == s2 {
		t.Fatal("unrelated adb clients should not share launchd session")
	}
}

func TestClassifyPIDNearestAgentWins(t *testing.T) {
	cfg := DefaultConfig()
	psInfoHook = func(pid int) (string, int, bool) {
		switch pid {
		case 700:
			return "adb", 710, true
		case 710:
			return "sh", 720, true
		case 720:
			return "node", 1, true
		default:
			return "", 0, false
		}
	}
	defer func() { psInfoHook = nil }()

	session, _ := classifyPID(700, cfg)
	if session != "node-720" {
		t.Fatalf("session=%q want node-720", session)
	}
}
