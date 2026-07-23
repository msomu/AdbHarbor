package harbor

import (
	"strings"
	"testing"
	"time"
)

// A small history: two completed leases for agent-a (2m and 4m), one still
// open (no release), one for agent-b, and a cleanup line that is not a lease
// boundary.
const histSample = `
{"ts":"2026-07-23T10:00:00+00:00","event":"grant","session":"agent-a","lease_id":"l1"}
{"ts":"2026-07-23T10:02:00+00:00","event":"idle-released","session":"agent-a","lease_id":"l1","note":"held 2m0s"}
{"ts":"2026-07-23T10:05:00+00:00","event":"grant","session":"agent-a","lease_id":"l2"}
{"ts":"2026-07-23T10:09:00+00:00","event":"released","session":"agent-a","lease_id":"l2"}
{"ts":"2026-07-23T10:10:00+00:00","event":"grant","session":"agent-a","lease_id":"l3"}
{"ts":"2026-07-23T10:11:00+00:00","event":"cleanup","session":"agent-a","lease_id":"l3","note":"uninstalled x"}
{"ts":"2026-07-23T11:00:00+00:00","event":"grant","session":"agent-b","lease_id":"l9"}
{"ts":"2026-07-23T11:00:30+00:00","event":"expired","session":"agent-b","lease_id":"l9"}
`

func TestParseHoldsPairsGrantWithRelease(t *testing.T) {
	holds := parseHolds(strings.NewReader(histSample))
	a := holds["agent-a"]
	if len(a) != 2 {
		t.Fatalf("agent-a holds = %v, want two completed leases", a)
	}
	if a[0] != 2*time.Minute || a[1] != 4*time.Minute {
		t.Errorf("agent-a holds = %v, want [2m 4m] in order", a)
	}
	// l3 has a grant but only a cleanup after it — not a completed hold.
	if b := holds["agent-b"]; len(b) != 1 || b[0] != 30*time.Second {
		t.Errorf("agent-b holds = %v, want [30s]", b)
	}
}

// Malformed input must never panic or abort the parse — inference is a
// convenience and a corrupt history file cannot be allowed to break it.
func TestParseHoldsSurvivesGarbage(t *testing.T) {
	junk := `not json
{"ts":"bad-timestamp","event":"grant","session":"s","lease_id":"x"}
{"event":"grant","session":"s","lease_id":"y"}
{"ts":"2026-07-23T10:00:00+00:00","event":"released","session":"s","lease_id":"never-granted"}
{"ts":"2026-07-23T10:05:00+00:00","event":"grant","session":"s","lease_id":"z"}
{"ts":"2026-07-23T10:04:00+00:00","event":"released","session":"s","lease_id":"z"}
`
	holds := parseHolds(strings.NewReader(junk))
	// z's release predates its grant (clock skew) -> dropped, not negative.
	if len(holds["s"]) != 0 {
		t.Errorf("holds = %v, want none from unusable records", holds["s"])
	}
}

func TestTypicalHoldNeedsEnoughSamples(t *testing.T) {
	if _, ok := typicalHold([]time.Duration{time.Minute, time.Minute}); ok {
		t.Error("two samples should be too few to infer from")
	}
	d, ok := typicalHold([]time.Duration{1 * time.Minute, 3 * time.Minute, 2 * time.Minute})
	if !ok || d != 2*time.Minute {
		t.Errorf("median = %v (ok=%v), want 2m", d, ok)
	}
}

// The median must resist a single outlier: one walk-away lease should not
// pull every future estimate up.
func TestTypicalHoldIgnoresAnOutlier(t *testing.T) {
	d, ok := typicalHold([]time.Duration{
		2 * time.Minute, 3 * time.Minute, 2 * time.Minute, 3 * time.Minute, time.Hour,
	})
	if !ok || d != 3*time.Minute {
		t.Errorf("median = %v, want 3m despite the 1h outlier", d)
	}
}

func TestInferredDescReadsAsAGuess(t *testing.T) {
	if s := inferredDesc(4 * time.Minute); s != "usually ~4m0s" {
		t.Errorf("got %q, want a hedged guess", s)
	}
	if s := inferredDesc(0); s != "" {
		t.Errorf("got %q, want nothing for a zero estimate", s)
	}
	// It must not borrow the declared vocabulary ("free in", "OVERDUE").
	s := inferredDesc(90 * time.Second)
	if strings.Contains(s, "free in") || strings.Contains(s, "OVERDUE") {
		t.Errorf("inferred text %q reuses declared wording", s)
	}
}
