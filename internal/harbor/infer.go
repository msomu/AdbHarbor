package harbor

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"
	"time"
)

// How much a session's own past predicts its next hold. Below minHoldSamples
// the median is noise and no estimate is offered at all — a guess presented
// with false confidence is worse than admitting we don't know. Only the most
// recent maxHoldSamples count, so a session's behaviour can change without
// its ancient history dragging the estimate.
const (
	minHoldSamples = 3
	maxHoldSamples = 20
)

// A history record, as written by hist(). Only the fields inference needs.
type histRecord struct {
	TS      string `json:"ts"`
	Event   string `json:"event"`
	Session string `json:"session"`
	LeaseID string `json:"lease_id"`
}

// parseHolds pairs each lease's grant with its release and returns, per
// session, how long that lease was actually held — in chronological order,
// so a caller can keep the most recent. It is deliberately forgiving: a line
// that will not parse, a release with no matching grant (log truncated
// underneath us), or a clock that ran backwards is skipped, never fatal.
// Inference is a convenience; a malformed history file must never break the
// broker.
func parseHolds(r io.Reader) map[string][]time.Duration {
	type open struct {
		session string
		start   time.Time
	}
	grants := map[string]open{}   // lease_id -> its grant
	holds := map[string][]time.Duration{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec histRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.LeaseID == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, rec.TS)
		if err != nil {
			continue
		}
		switch rec.Event {
		case "grant":
			grants[rec.LeaseID] = open{rec.Session, ts}
		case "cleanup":
			// Not a lease boundary; ignore.
		default:
			// Any other event terminates the lease.
			g, ok := grants[rec.LeaseID]
			if !ok {
				continue
			}
			delete(grants, rec.LeaseID)
			if d := ts.Sub(g.start); d >= 0 {
				holds[g.session] = append(holds[g.session], d)
			}
		}
	}
	return holds
}

// loadHoldHistory seeds the broker's per-session hold record from disk at
// startup, so a freshly restarted daemon can offer estimates immediately
// instead of relearning from scratch. Missing or unreadable history just
// means no seed.
func loadHoldHistory() map[string][]time.Duration {
	f, err := os.Open(HistoryPath())
	if err != nil {
		return map[string][]time.Duration{}
	}
	defer f.Close()
	holds := parseHolds(f)
	for s, ds := range holds {
		holds[s] = lastN(ds, maxHoldSamples)
	}
	return holds
}

// typicalHold is the median of a session's recent holds, and whether there
// is enough history to mean anything. The median, not the mean: one agent
// that walked away and let a lease sit for an hour should not drag every
// estimate upward.
func typicalHold(samples []time.Duration) (time.Duration, bool) {
	if len(samples) < minHoldSamples {
		return 0, false
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid], true
	}
	return (sorted[mid-1] + sorted[mid]) / 2, true
}

func lastN(s []time.Duration, n int) []time.Duration {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// inferredDesc renders a typical-hold estimate for a waiter. It is worded to
// read as a guess, never as the "free in ~Xm" a holder actually promised,
// and it never becomes OVERDUE — nothing was promised, so nothing can be
// broken. This is the whole point of inferring rather than declaring: a
// waiter gets a hint where it would otherwise be blind, without a made-up
// number ever hardening into a commitment.
func inferredDesc(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return "usually ~" + roundedDur(d)
}
