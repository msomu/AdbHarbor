package harbor

import (
	"strings"
	"testing"
	"time"
)

// An estimate is a promise, not a deadline: it tells a waiting agent when to
// expect the device and changes nothing about when the lease actually ends.
// Every test here exists to keep those two ideas apart.

func TestETAIsAdvisoryAndNeverExpiresALease(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	req := command("agent-a")
	req.ETASec, req.ETANote = 60, "maestro smoke"
	b.mu.Lock()
	l := b.grantLocked(req, t0, time.Hour)
	l.Running = 1 // still working, well past its own estimate
	b.mu.Unlock()

	if !l.ETA.Equal(t0.Add(time.Minute)) {
		t.Fatalf("ETA = %v, want %v", l.ETA, t0.Add(time.Minute))
	}
	b.sweep(t0.Add(30 * time.Minute))
	if got := b.holderAt(); got != "agent-a" {
		t.Fatalf("holder = %q — an overdue estimate must not end a lease", got)
	}
}

// A command that says nothing about timing must not silently erase an
// estimate the agent set deliberately.
func TestSilentCommandKeepsTheExistingETA(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	first := command("agent-a")
	first.ETASec, first.ETANote = 300, "install + flow"
	b.acquireAt(first, t0, 30*time.Second)

	b.acquireAt(command("agent-a"), t0.Add(time.Second), 30*time.Second)

	b.mu.Lock()
	l := b.leases[dev]
	b.mu.Unlock()
	if !l.ETA.Equal(t0.Add(5 * time.Minute)) {
		t.Errorf("ETA = %v, want it untouched at %v", l.ETA, t0.Add(5*time.Minute))
	}
	if l.ETANote != "install + flow" {
		t.Errorf("note = %q, want it preserved", l.ETANote)
	}
}

func TestLaterCommandRefreshesTheETA(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	first := command("agent-a")
	first.ETASec = 60
	b.acquireAt(first, t0, 30*time.Second)

	later := command("agent-a")
	later.ETASec, later.ETANote = 120, "taking longer"
	b.acquireAt(later, t0.Add(30*time.Second), 30*time.Second)

	b.mu.Lock()
	l := b.leases[dev]
	b.mu.Unlock()
	want := t0.Add(30 * time.Second).Add(2 * time.Minute)
	if !l.ETA.Equal(want) {
		t.Errorf("ETA = %v, want %v (measured from the update, not the grant)", l.ETA, want)
	}
	if l.ETANote != "taking longer" {
		t.Errorf("note = %q, want the refreshed one", l.ETANote)
	}
}

// What a queued agent actually reads. An estimate that has passed must not
// render as a negative countdown — the waiter needs to know it is stale.
func TestETADesc(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []struct {
		name string
		eta  time.Time
		note string
		want string
	}{
		{"none", time.Time{}, "", ""},
		{"minutes ahead", now.Add(6 * time.Minute), "", "free in ~6m0s"},
		{"seconds ahead", now.Add(20 * time.Second), "", "free in ~20s"},
		{"with a note", now.Add(6 * time.Minute), "maestro smoke", "free in ~6m0s (maestro smoke)"},
		{"passed", now.Add(-3 * time.Minute), "", "OVERDUE by 3m0s"},
		{"passed with note", now.Add(-3 * time.Minute), "flow", "OVERDUE by 3m0s (flow)"},
	}
	for _, c := range cases {
		if got := ETADesc(c.eta, c.note, now); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The estimate has to reach the string a blocked agent is shown, or the
// whole feature is invisible at the one moment it matters.
func TestBusyDescriptionCarriesTheETA(t *testing.T) {
	b := testBroker(t, DefaultConfig())

	req := command("agent-a")
	req.ETASec, req.ETANote = 600, "maestro smoke"
	b.mu.Lock()
	b.grantLocked(req, time.Now(), 30*time.Second)
	desc := b.serialDescLocked(dev)
	b.mu.Unlock()

	if !strings.Contains(desc, "free in ~10m") || !strings.Contains(desc, "maestro smoke") {
		t.Errorf("busy description %q does not tell a waiter what to expect", desc)
	}
}

// Flags after the duration must still be parsed: `eta 25m -note "..."` is
// the order anyone writes it in.
func TestSplitLeadingArg(t *testing.T) {
	cases := []struct {
		args  []string
		first string
		rest  []string
	}{
		{[]string{"25m", "-note", "flow is retrying"}, "25m", []string{"-note", "flow is retrying"}},
		{[]string{"-note", "x", "25m"}, "", []string{"-note", "x", "25m"}},
		{[]string{"--clear"}, "", []string{"--clear"}},
		{nil, "", nil},
	}
	for _, c := range cases {
		first, rest := splitLeadingArg(c.args)
		if first != c.first || len(rest) != len(c.rest) {
			t.Errorf("splitLeadingArg(%v) = (%q, %v), want (%q, %v)", c.args, first, rest, c.first, c.rest)
		}
	}
}
