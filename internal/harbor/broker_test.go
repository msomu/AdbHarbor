package harbor

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// The handoff loop, end to end, with no device and no sleeping: one agent's
// commands take a lease, another agent's queue behind it, and the device
// moves on when the first agent is done. These are the rules every other
// guarantee rests on, exercised through the same code path the daemon runs.

func testBroker(t *testing.T, cfg *Config) *Broker {
	t.Helper()
	// Keep state.json and history.jsonl out of the real ~/.adbharbor.
	t.Setenv("ADB_HARBOR_DIR", t.TempDir())
	b := &Broker{
		leases:   map[string]*Lease{},
		queues:   map[string][]*Waiter{},
		waiters:  map[string]*Waiter{},
		cleaning: map[string]bool{},
	}
	b.cfg.Store(cfg)
	return b
}

// command is one agent running one device-targeted adb command.
func command(session string) AcquireReq {
	return AcquireReq{Serial: dev, Session: session, Holder: session, Command: true}
}

const dev = "SERIAL1"

// acquire drives the same entry point the shim and proxy use, at a
// controlled instant.
func (b *Broker) acquireAt(req AcquireReq, now time.Time, idle time.Duration) (*Lease, *Waiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acquireLocked(req, now, idle)
}

func TestSecondAgentQueuesBehindTheFirst(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)
	idle := 30 * time.Second

	leaseA, waitA := b.acquireAt(command("agent-a"), t0, idle)
	if leaseA == nil || waitA != nil {
		t.Fatal("first agent did not get the device")
	}
	leaseB, waitB := b.acquireAt(command("agent-b"), t0.Add(time.Second), idle)
	if leaseB != nil {
		t.Fatal("second agent took a device that was already held")
	}
	if waitB == nil || waitB.Session != "agent-b" {
		t.Fatal("second agent was neither granted nor queued")
	}
}

// The same agent's consecutive commands must never queue behind each other.
// When session identity breaks, this is the guarantee that fails: the agent
// waits out its own linger before its next command runs.
func TestSameSessionRunsConcurrently(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)
	idle := 30 * time.Second

	first, _ := b.acquireAt(command("agent-a"), t0, idle)
	second, waiter := b.acquireAt(command("agent-a"), t0.Add(time.Second), idle)
	if waiter != nil {
		t.Fatal("agent queued behind its own lease")
	}
	if second == nil || second.ID != first.ID {
		t.Fatal("second command did not reuse the session's lease")
	}
	if second.Running != 2 {
		t.Errorf("running = %d, want 2 concurrent commands", second.Running)
	}
}

// A finished command does not release the device: the lease lingers for the
// idle TTL so the agent keeps ownership across its next step. The waiting
// agent pays that linger in full.
func TestWaiterGetsDeviceOnlyAfterLingerExpires(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)
	idle := 30 * time.Second

	leaseA, _ := b.acquireAt(command("agent-a"), t0, idle)
	_, waitB := b.acquireAt(command("agent-b"), t0.Add(time.Second), idle)
	if waitB == nil {
		t.Fatal("agent-b was not queued")
	}

	// agent-a's command finishes.
	b.EndLeaseCommand(leaseA.ID)
	b.mu.Lock()
	b.leases[dev].LastActive = t0.Add(2 * time.Second)
	b.mu.Unlock()

	// Mid-linger: the device is idle but still owned.
	b.sweep(t0.Add(20 * time.Second))
	b.mu.Lock()
	stillA := b.leases[dev] != nil && b.leases[dev].Session == "agent-a"
	b.mu.Unlock()
	if !stillA {
		t.Fatal("lease released before the idle TTL elapsed")
	}
	if waitB.lease != nil {
		t.Fatal("agent-b granted the device while agent-a still owned it")
	}

	// Past the linger: the device moves on.
	b.sweep(t0.Add(40 * time.Second))
	b.mu.Lock()
	got := b.leases[dev]
	b.mu.Unlock()
	if got == nil || got.Session != "agent-b" {
		t.Fatalf("device did not hand off to agent-b, got %+v", got)
	}
	if waitB.lease == nil {
		t.Error("agent-b's waiter was not woken with its lease")
	}
}

// The linger is what a waiting agent pays for the previous agent's
// think-time, so the default bounds the cost of every handoff. This test
// states that cost in code: raising it is a deliberate act, not a drift.
func TestDefaultIdleTTLBoundsHandoffCost(t *testing.T) {
	cfg := DefaultConfig()
	idle := time.Duration(cfg.IdleTTLSec) * time.Second
	if idle != 30*time.Second {
		t.Errorf("default idle TTL = %s, want 30s (update this test deliberately)", idle)
	}

	b := testBroker(t, cfg)
	t0 := time.Unix(1700000000, 0)

	leaseA, _ := b.acquireAt(command("agent-a"), t0, idle)
	_, waitB := b.acquireAt(command("agent-b"), t0.Add(time.Second), idle)
	b.EndLeaseCommand(leaseA.ID)
	b.mu.Lock()
	b.leases[dev].LastActive = t0.Add(2 * time.Second)
	b.mu.Unlock()

	// Still owned one second before the linger is up...
	b.sweep(t0.Add(31 * time.Second))
	if got := b.holderAt(); got != "agent-a" {
		t.Fatalf("holder at 31s = %q, want agent-a", got)
	}
	// ...and handed over a second after.
	b.poll(waitB, t0.Add(32*time.Second))
	b.sweep(t0.Add(33 * time.Second))
	if got := b.holderAt(); got != "agent-b" {
		t.Fatalf("holder at 33s = %q, want agent-b", got)
	}
}

// holderAt reports which session owns the device, or "" if it is free.
func (b *Broker) holderAt() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if l := b.leases[dev]; l != nil {
		return l.Session
	}
	return ""
}

// poll stands in for a queued client's long-poll: a waiter that goes quiet
// for staleWaiter loses its place, so a live client keeps its LastPoll
// current while it waits.
func (b *Broker) poll(wt *Waiter, now time.Time) {
	b.mu.Lock()
	wt.LastPoll = now
	b.mu.Unlock()
}

func TestQueueIsFIFOAcrossAgents(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)
	idle := 30 * time.Second

	leaseA, _ := b.acquireAt(command("agent-a"), t0, idle)
	_, waitB := b.acquireAt(command("agent-b"), t0.Add(time.Second), idle)
	_, waitC := b.acquireAt(command("agent-c"), t0.Add(2*time.Second), idle)
	if waitB == nil || waitC == nil {
		t.Fatal("agent-b and agent-c should both be queued")
	}

	b.EndLeaseCommand(leaseA.ID)
	b.mu.Lock()
	b.leases[dev].LastActive = t0.Add(3 * time.Second)
	b.mu.Unlock()

	b.sweep(t0.Add(time.Minute))
	if got := b.holderAt(); got != "agent-b" {
		t.Fatalf("queue head was %q, want agent-b (FIFO)", got)
	}

	// agent-b takes its turn and finishes; agent-c is still waiting and
	// still polling.
	b.mu.Lock()
	leaseB := b.leases[dev]
	leaseB.Claimed, leaseB.Running = true, 0
	leaseB.LastActive = t0.Add(time.Minute)
	b.mu.Unlock()
	b.poll(waitC, t0.Add(time.Minute))

	b.sweep(t0.Add(2 * time.Minute))
	if got := b.holderAt(); got != "agent-c" {
		t.Fatalf("second handoff went to %q, want agent-c", got)
	}
}

// A queued client that stops polling — killed, or its shell torn down — must
// not hold the queue behind it.
func TestSilentWaiterLosesItsPlace(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)
	idle := 30 * time.Second

	leaseA, _ := b.acquireAt(command("agent-a"), t0, idle)
	_, gone := b.acquireAt(command("agent-gone"), t0.Add(time.Second), idle)
	_, waitC := b.acquireAt(command("agent-c"), t0.Add(2*time.Second), idle)
	if gone == nil || waitC == nil {
		t.Fatal("both agents should be queued")
	}

	b.EndLeaseCommand(leaseA.ID)
	b.mu.Lock()
	b.leases[dev].LastActive = t0.Add(3 * time.Second)
	b.mu.Unlock()

	// agent-c keeps polling; agent-gone does not.
	handoff := t0.Add(5 * time.Minute)
	b.poll(waitC, handoff)
	b.sweep(handoff)

	if got := b.holderAt(); got != "agent-c" {
		t.Fatalf("device went to %q, want agent-c — a dead waiter blocked the queue", got)
	}
}

// Queued commands from the same session piggyback on one lease rather than
// serialising: an agent with two commands waiting gets both when its turn
// comes.
func TestSameSessionWaitersShareTheGrantedLease(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)
	idle := 30 * time.Second

	leaseA, _ := b.acquireAt(command("agent-a"), t0, idle)
	_, w1 := b.acquireAt(command("agent-b"), t0.Add(time.Second), idle)
	_, w2 := b.acquireAt(command("agent-b"), t0.Add(2*time.Second), idle)
	if w1 == nil || w2 == nil {
		t.Fatal("agent-b commands were not queued")
	}

	b.EndLeaseCommand(leaseA.ID)
	b.mu.Lock()
	b.leases[dev].LastActive = t0.Add(3 * time.Second)
	b.mu.Unlock()
	b.sweep(t0.Add(time.Minute))

	if w1.lease == nil || w2.lease == nil {
		t.Fatal("not every queued command of the winning session was woken")
	}
	if w1.lease.ID != w2.lease.ID {
		t.Error("same-session waiters got different leases")
	}
	if w1.lease.Running != 2 {
		t.Errorf("running = %d, want both commands counted", w1.lease.Running)
	}
}

// A lease granted from the queue to an agent that has since died must not
// strand the device.
func TestUnclaimedLeaseIsReclaimed(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	b.mu.Lock()
	l := b.grantLocked(command("agent-a"), t0, 30*time.Second)
	l.Claimed = false // granted from the queue, never picked up
	l.Running = 0
	b.mu.Unlock()

	b.sweep(t0.Add(unclaimedGrace + time.Second))
	b.mu.Lock()
	_, still := b.leases[dev]
	b.mu.Unlock()
	if still {
		t.Error("unclaimed lease was not reclaimed")
	}
}

// A hard-killed client leaves its running count stuck above zero; without
// the orphan sweep the device would never reach idle and never hand off.
func TestOrphanedCommandsStopPinningTheDevice(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	b.mu.Lock()
	l := b.grantLocked(command("agent-a"), t0, 30*time.Second)
	l.Running = 1 // client killed mid-command: never balanced by /v1/end
	b.mu.Unlock()

	b.sweep(t0.Add(orphanBeat + time.Second))
	b.mu.Lock()
	running := 0
	if cur := b.leases[dev]; cur != nil {
		running = cur.Running
	}
	b.mu.Unlock()
	if running != 0 {
		t.Fatalf("running = %d, want 0 after the orphan sweep", running)
	}

	// With the count cleared the idle clock runs and the device frees up.
	b.sweep(t0.Add(orphanBeat + 2*time.Minute))
	b.mu.Lock()
	_, still := b.leases[dev]
	b.mu.Unlock()
	if still {
		t.Error("device stayed pinned by a dead client")
	}
}

// An agent that dies never releases. Nothing in the agent can be relied on
// to notice — a killed process runs no exit hook — so the broker checks
// whether the owning process still exists.

func TestLeaseReclaimedWhenOwningAgentDies(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	dead := startHelper(t)
	killHelper(t, dead)

	req := command(fmt.Sprintf("claude-%d", dead))
	req.Explicit, req.TTLSec = true, 3600 // a long explicit lease
	b.mu.Lock()
	l := b.grantLocked(req, t0, 30*time.Second)
	l.Running = 0
	b.mu.Unlock()

	if l.OwnerPID != dead {
		t.Fatalf("OwnerPID = %d, want %d", l.OwnerPID, dead)
	}
	// Far inside the explicit TTL: only the dead owner can free this.
	b.sweep(t0.Add(time.Minute))
	if got := b.holderAt(); got != "" {
		t.Fatalf("device still held by %q after its agent died", got)
	}
}

func TestLeaseSurvivesWhileOwnerIsAlive(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	alive := fmt.Sprintf("claude-%d", os.Getpid())
	b.mu.Lock()
	l := b.grantLocked(command(alive), t0, 30*time.Second)
	l.Running, l.LastActive = 0, t0
	b.mu.Unlock()

	b.sweep(t0.Add(10 * time.Second))
	if got := b.holderAt(); got != alive {
		t.Fatalf("holder = %q, want %q — a live agent lost its lease", got, alive)
	}
}

// A dead owner must not pull the device out from under a command that is
// still running against it.
func TestDeadOwnerDoesNotInterruptRunningCommand(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	dead := startHelper(t)
	killHelper(t, dead)

	b.mu.Lock()
	l := b.grantLocked(command(fmt.Sprintf("claude-%d", dead)), t0, 30*time.Second)
	l.Running, l.LastBeat = 1, t0
	b.mu.Unlock()

	b.sweep(t0.Add(10 * time.Second))
	if b.holderAt() == "" {
		t.Fatal("device released while a command was still running on it")
	}
}

// An explicit ADB_HARBOR_SESSION carries no pid, so there is no liveness
// signal and the TTL stays in charge.
func TestExplicitKeyHasNoLivenessSignal(t *testing.T) {
	b := testBroker(t, DefaultConfig())
	t0 := time.Unix(1700000000, 0)

	b.mu.Lock()
	l := b.grantLocked(command("ci-lane"), t0, 30*time.Second)
	l.Running, l.LastActive = 0, t0
	b.mu.Unlock()

	if l.OwnerPID != 0 {
		t.Errorf("OwnerPID = %d, want 0 for a pid-less key", l.OwnerPID)
	}
	b.sweep(t0.Add(10 * time.Second))
	if b.holderAt() != "ci-lane" {
		t.Error("pid-less lease was reclaimed before its idle TTL")
	}
}

func TestOwnerPIDFromSession(t *testing.T) {
	cases := map[string]int{
		"claude-97333":   97333,
		"bun-49286":      49286,
		"pid-1":          0, // init is never an agent
		"ci-lane-3":      3, // digits after the last dash are indistinguishable
		"ci-lane":        0,
		"claude":         0,
		"claude-abc1234": 0,
	}
	for session, want := range cases {
		if got := OwnerPIDFromSession(session); got != want {
			t.Errorf("OwnerPIDFromSession(%q) = %d, want %d", session, got, want)
		}
	}
}

func TestSharedIdentityWarning(t *testing.T) {
	if w := sharedIdentityWarning("bun-49286", "process tree"); w == "" {
		t.Error("a bun-keyed identity should warn about sharing")
	}
	if w := sharedIdentityWarning("claude-97333", "process tree"); w != "" {
		t.Errorf("a claude-keyed identity should not warn, got %q", w)
	}
	// An explicit key is deliberate, whatever it is named after.
	if w := sharedIdentityWarning("node-1", "ADB_HARBOR_SESSION"); w != "" {
		t.Errorf("explicit key warned: %q", w)
	}
}
