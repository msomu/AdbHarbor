package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	reaperInterval = 2 * time.Second
	// orphanBeat: a lease with running commands but no heartbeat for this
	// long is treated as orphaned (shim killed hard) and moved to idle.
	orphanBeat = 60 * time.Second
	// staleWaiter: queued waiters that stopped polling are dropped.
	staleWaiter = 90 * time.Second
	// unclaimedGrace: a lease granted from the queue whose client never
	// picks it up (killed while waiting) is reclaimed after this long.
	unclaimedGrace = 20 * time.Second
)

type Lease struct {
	ID         string    `json:"id"`
	Serial     string    `json:"serial"`
	Session    string    `json:"session"`
	Holder     string    `json:"holder"`
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquired_at"`
	LastActive time.Time `json:"last_active"`
	LastBeat   time.Time `json:"last_beat"`
	Running    int       `json:"running"`
	IdleTTL    time.Duration `json:"idle_ttl"`
	Explicit   bool      `json:"explicit"`
	ExpiresAt  time.Time `json:"expires_at"`
	// Claimed is false for a lease granted from the queue until its owner
	// shows a sign of life (wait pickup, heartbeat, or command end).
	Claimed bool `json:"claimed"`
	// OwnerPID is the agent process the session is named for, when the
	// session key was derived from the process tree. A lease outlives the
	// command that took it, so this is the only handle on whether the agent
	// that owns it still exists.
	OwnerPID int `json:"owner_pid,omitempty"`
	// Baseline is the device's package list at grant time, used by session
	// cleanup to uninstall only what this session installed.
	Baseline []string `json:"pkg_baseline,omitempty"`
}

type Waiter struct {
	ID       string
	Serial   string
	Session  string
	Holder   string
	Command  bool
	IdleTTL  time.Duration
	Enqueued time.Time
	LastPoll time.Time
	ch       chan struct{}
	lease    *Lease
}

type Broker struct {
	mu       sync.Mutex
	cfg      atomic.Pointer[Config] // hot-reloaded; read via b.config()
	leases   map[string]*Lease  // by serial
	queues   map[string][]*Waiter
	waiters  map[string]*Waiter // by waiter ID
	cleaning map[string]bool    // serials in post-session cleanup
	cfgMTime time.Time
	seq      int64
}

func (b *Broker) config() *Config { return b.cfg.Load() }

// RunDaemon starts the broker on the harbor unix socket (foreground).
func RunDaemon() error {
	if err := EnsureDir(); err != nil {
		return err
	}
	log.SetPrefix("[harbor] ")

	// Exclusive daemon lock: released automatically if we die.
	lockF, err := os.OpenFile(LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lockF.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another daemon is already running")
	}

	os.Remove(SocketPath())
	ln, err := net.Listen("unix", SocketPath())
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	os.WriteFile(PIDPath(), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)

	b := &Broker{
		leases:   map[string]*Lease{},
		queues:   map[string][]*Waiter{},
		waiters:  map[string]*Waiter{},
		cleaning: map[string]bool{},
	}
	b.cfg.Store(LoadConfig())
	if info, err := os.Stat(ConfigPath()); err == nil {
		b.cfgMTime = info.ModTime()
	}
	b.loadState()
	go b.reaper()
	if b.config().ProxyEnabled {
		go func() {
			if err := b.runProxy(); err != nil {
				log.Printf("proxy disabled: %v", err)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ping", b.handlePing)
	mux.HandleFunc("/v1/acquire", b.handleAcquire)
	mux.HandleFunc("/v1/acquire-any", b.handleAcquireAny)
	mux.HandleFunc("/v1/wait", b.handleWait)
	mux.HandleFunc("/v1/heartbeat", b.handleHeartbeat)
	mux.HandleFunc("/v1/end", b.handleEnd)
	mux.HandleFunc("/v1/release", b.handleRelease)
	mux.HandleFunc("/v1/state", b.handleState)
	mux.HandleFunc("/v1/devices", b.handleDevices)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		b.mu.Lock()
		b.saveStateLocked()
		b.mu.Unlock()
		os.Remove(SocketPath())
		os.Remove(PIDPath())
		os.Exit(0)
	}()

	log.Printf("daemon %s listening on %s (pid %d)", Version, SocketPath(), os.Getpid())
	return (&http.Server{Handler: mux}).Serve(ln)
}

func (b *Broker) newID(prefix string) string {
	b.seq++
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), b.seq)
}

func leaseDesc(l *Lease) string {
	if l == nil {
		return "free"
	}
	return fmt.Sprintf("%s for %s", l.Holder, time.Since(l.AcquiredAt).Round(time.Second))
}

func (b *Broker) serialDescLocked(serial string) string {
	if l := b.leases[serial]; l != nil {
		return leaseDesc(l)
	}
	if b.cleaning[serial] {
		return "session cleanup"
	}
	return "free"
}

// ---- handlers ----

func (b *Broker) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, PingResp{OK: true, Version: Version, PID: os.Getpid()})
}

func (b *Broker) handleAcquire(w http.ResponseWriter, r *http.Request) {
	var req AcquireReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Serial == "" || req.Session == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	now := time.Now()
	idle := time.Duration(b.config().IdleTTLSec) * time.Second
	if req.IdleTTLSec > 0 {
		idle = time.Duration(req.IdleTTLSec) * time.Second
	}

	b.mu.Lock()
	l, wt := b.acquireLocked(req, now, idle)
	if l != nil {
		b.mu.Unlock()
		writeJSON(w, AcquireResp{Granted: true, LeaseID: l.ID})
		return
	}
	resp := AcquireResp{
		WaiterID:   wt.ID,
		Position:   len(b.queues[req.Serial]),
		HolderDesc: b.serialDescLocked(req.Serial),
	}
	b.mu.Unlock()
	writeJSON(w, resp)
}

// handleAcquireAny atomically picks a free device matching the constraints
// and grants an explicit lease on it — the "route to the next free phone"
// primitive. Never waits: if every matching device is busy, the caller gets
// the holder list and decides whether to queue on one or retry.
func (b *Broker) handleAcquireAny(w http.ResponseWriter, r *http.Request) {
	var req AcquireAnyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Session == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Device inventory comes from adb — do it before taking the lock.
	devs, err := ListDevices(b.config().RealADB, b.config().ClientServerPort())
	if err != nil {
		writeJSON(w, AcquireAnyResp{Message: err.Error()})
		return
	}
	now := time.Now()
	acq := AcquireReq{
		Session: req.Session, Holder: req.Holder, PID: req.PID,
		TTLSec: req.TTLSec, Explicit: true,
	}
	idle := time.Duration(b.config().IdleTTLSec) * time.Second

	b.mu.Lock()
	defer b.mu.Unlock()

	// Sticky: a session that already holds a device keeps getting it.
	for _, l := range b.leases {
		if l.Session == req.Session {
			l.LastActive, l.LastBeat, l.Claimed = now, now, true
			l.Explicit = true
			l.ExpiresAt = now.Add(b.explicitTTL(acq))
			writeJSON(w, AcquireAnyResp{Granted: true, Serial: l.Serial, LeaseID: l.ID})
			return
		}
	}
	var busy []string
	for _, d := range devs {
		if d.State != "device" {
			continue
		}
		isEmu := strings.HasPrefix(d.Serial, "emulator-")
		if req.USB && (isEmu || !d.USB) {
			continue
		}
		if req.Emulator && !isEmu {
			continue
		}
		if l := b.leases[d.Serial]; l != nil {
			busy = append(busy, fmt.Sprintf("%s (held by %s)", d.Serial, l.Holder))
			continue
		}
		if b.cleaning[d.Serial] {
			busy = append(busy, d.Serial+" (session cleanup)")
			continue
		}
		acq.Serial = d.Serial
		l := b.grantLocked(acq, now, idle)
		writeJSON(w, AcquireAnyResp{Granted: true, Serial: d.Serial, LeaseID: l.ID})
		return
	}
	msg := "no matching device connected"
	if len(busy) > 0 {
		msg = "all matching devices busy: " + strings.Join(busy, ", ")
	}
	writeJSON(w, AcquireAnyResp{Message: msg})
}

// acquireLocked grants (or renews) a lease, or enqueues a waiter when the
// device is held by another session. Exactly one return value is non-nil.
func (b *Broker) acquireLocked(req AcquireReq, now time.Time, idle time.Duration) (*Lease, *Waiter) {
	if b.cleaning[req.Serial] {
		wt := &Waiter{
			ID: b.newID("w"), Serial: req.Serial, Session: req.Session,
			Holder: req.Holder, Command: req.Command, IdleTTL: idle,
			Enqueued: now, LastPoll: now, ch: make(chan struct{}),
		}
		b.queues[req.Serial] = append(b.queues[req.Serial], wt)
		b.waiters[wt.ID] = wt
		return nil, wt
	}
	l := b.leases[req.Serial]
	if l != nil && l.Session == req.Session {
		// Same session: renew the existing lease.
		l.LastActive, l.LastBeat = now, now
		l.IdleTTL = idle
		l.Claimed = true
		if req.Command {
			l.Running++
		}
		if req.Explicit {
			l.Explicit = true
			l.ExpiresAt = now.Add(b.explicitTTL(req))
		}
		return l, nil
	}
	if l == nil {
		return b.grantLocked(req, now, idle), nil
	}
	wt := &Waiter{
		ID: b.newID("w"), Serial: req.Serial, Session: req.Session,
		Holder: req.Holder, Command: req.Command, IdleTTL: idle,
		Enqueued: now, LastPoll: now, ch: make(chan struct{}),
	}
	b.queues[req.Serial] = append(b.queues[req.Serial], wt)
	b.waiters[wt.ID] = wt
	log.Printf("queued %s for %s (held by %s, %d waiting)", req.Holder, req.Serial, l.Holder, len(b.queues[req.Serial]))
	return nil, wt
}

// AcquireLocalBlocking is the in-process equivalent of the HTTP acquire +
// wait long-poll, used by the ADB server proxy. It blocks until the lease
// is granted, waitSec elapses, or abort closes.
func (b *Broker) AcquireLocalBlocking(req AcquireReq, waitSec int, abort <-chan struct{}) (*Lease, error) {
	var deadline time.Time
	if waitSec > 0 {
		deadline = time.Now().Add(time.Duration(waitSec) * time.Second)
	}
	idle := time.Duration(b.config().IdleTTLSec) * time.Second
	if req.IdleTTLSec > 0 {
		idle = time.Duration(req.IdleTTLSec) * time.Second
	}
	for {
		b.mu.Lock()
		l, wt := b.acquireLocked(req, time.Now(), idle)
		b.mu.Unlock()
		if l != nil {
			return l, nil
		}
		granted, err := b.waitLocal(wt, deadline, abort)
		if err != nil {
			return nil, err
		}
		if granted != nil {
			return granted, nil
		}
		// Waiter was dropped without a lease; re-acquire.
	}
}

func (b *Broker) waitLocal(wt *Waiter, deadline time.Time, abort <-chan struct{}) (*Lease, error) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var expire <-chan time.Time
	if !deadline.IsZero() {
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		expire = t.C
	}
	for {
		select {
		case <-wt.ch:
			b.mu.Lock()
			l := wt.lease
			if l != nil {
				l.Claimed = true
			}
			delete(b.waiters, wt.ID)
			b.mu.Unlock()
			return l, nil
		case <-tick.C:
			// Local waiters "poll" by construction — keep them fresh so
			// the reaper doesn't drop them.
			b.mu.Lock()
			wt.LastPoll = time.Now()
			b.mu.Unlock()
		case <-expire:
			b.dropWaiter(wt)
			return nil, ErrWaitTimeout
		case <-abort:
			b.dropWaiter(wt)
			return nil, errors.New("client went away while queued")
		}
	}
}

// dropWaiter removes a waiter that is giving up; if a lease was granted in
// the race window, it is released so the queue keeps moving.
func (b *Broker) dropWaiter(wt *Waiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.waiters, wt.ID)
	q := b.queues[wt.Serial]
	for i, x := range q {
		if x == wt {
			b.queues[wt.Serial] = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(b.queues[wt.Serial]) == 0 {
		delete(b.queues, wt.Serial)
	}
	if wt.lease != nil && b.leases[wt.Serial] == wt.lease {
		b.expireLocked(wt.lease, time.Now(), "abandoned")
	}
}

// TouchLease and EndLeaseCommand are in-process lease upkeep for the proxy.
func (b *Broker) TouchLease(id string) {
	b.mu.Lock()
	if l := b.leaseByIDLocked(id); l != nil {
		now := time.Now()
		l.LastBeat, l.LastActive, l.Claimed = now, now, true
	}
	b.mu.Unlock()
}

func (b *Broker) EndLeaseCommand(id string) {
	b.mu.Lock()
	if l := b.leaseByIDLocked(id); l != nil {
		now := time.Now()
		if l.Running > 0 {
			l.Running--
		}
		l.LastBeat, l.LastActive, l.Claimed = now, now, true
	}
	b.mu.Unlock()
}

func (b *Broker) explicitTTL(req AcquireReq) time.Duration {
	if req.TTLSec > 0 {
		return time.Duration(req.TTLSec) * time.Second
	}
	return time.Duration(b.config().ExplicitTTLSec) * time.Second
}

// grantLocked creates a fresh lease for an acquire request.
func (b *Broker) grantLocked(req AcquireReq, now time.Time, idle time.Duration) *Lease {
	l := &Lease{
		ID: b.newID("lease"), Serial: req.Serial, Session: req.Session,
		Holder: req.Holder, PID: req.PID, OwnerPID: OwnerPIDFromSession(req.Session),
		AcquiredAt: now, LastActive: now, LastBeat: now,
		IdleTTL: idle, Explicit: req.Explicit, Claimed: true,
	}
	if req.Command {
		l.Running = 1
	}
	if req.Explicit {
		l.ExpiresAt = now.Add(b.explicitTTL(req))
	}
	b.leases[req.Serial] = l
	b.hist("grant", l, "")
	log.Printf("granted %s to %s", l.Serial, l.Holder)
	if b.config().CleanupEnabled {
		go b.captureBaseline(l.ID, l.Serial)
	}
	b.saveStateLocked()
	return l
}

func (b *Broker) handleWait(w http.ResponseWriter, r *http.Request) {
	var req WaitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	waitFor := time.Duration(req.WaitMS) * time.Millisecond
	if waitFor <= 0 || waitFor > 25*time.Second {
		waitFor = 25 * time.Second
	}

	b.mu.Lock()
	wt := b.waiters[req.WaiterID]
	if wt == nil {
		b.mu.Unlock()
		writeJSON(w, WaitResp{Expired: true})
		return
	}
	wt.LastPoll = time.Now()
	if wt.lease != nil {
		l := wt.lease
		l.Claimed = true
		delete(b.waiters, wt.ID)
		b.mu.Unlock()
		writeJSON(w, WaitResp{Granted: true, LeaseID: l.ID})
		return
	}
	ch := wt.ch
	b.mu.Unlock()

	select {
	case <-ch:
		b.mu.Lock()
		l := wt.lease
		if l != nil {
			l.Claimed = true
		}
		delete(b.waiters, wt.ID)
		b.mu.Unlock()
		if l == nil {
			writeJSON(w, WaitResp{Expired: true})
			return
		}
		writeJSON(w, WaitResp{Granted: true, LeaseID: l.ID})
	case <-time.After(waitFor):
		b.mu.Lock()
		wt.LastPoll = time.Now()
		pos := b.positionLocked(wt)
		desc := b.serialDescLocked(wt.Serial)
		b.mu.Unlock()
		writeJSON(w, WaitResp{Position: pos, HolderDesc: desc})
	case <-r.Context().Done():
	}
}

func (b *Broker) positionLocked(wt *Waiter) int {
	for i, q := range b.queues[wt.Serial] {
		if q == wt {
			return i + 1
		}
	}
	return 0
}

func (b *Broker) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req LeaseRef
	json.NewDecoder(r.Body).Decode(&req)
	b.mu.Lock()
	if l := b.leaseByIDLocked(req.LeaseID); l != nil {
		now := time.Now()
		l.LastBeat, l.LastActive = now, now
		l.Claimed = true
	}
	b.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
}

func (b *Broker) handleEnd(w http.ResponseWriter, r *http.Request) {
	var req LeaseRef
	json.NewDecoder(r.Body).Decode(&req)
	b.mu.Lock()
	if l := b.leaseByIDLocked(req.LeaseID); l != nil {
		now := time.Now()
		if l.Running > 0 {
			l.Running--
		}
		l.LastBeat, l.LastActive = now, now
		l.Claimed = true
	}
	b.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
}

func (b *Broker) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req ReleaseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	var l *Lease
	if req.LeaseID != "" {
		l = b.leaseByIDLocked(req.LeaseID)
	} else if req.Serial != "" {
		l = b.leases[req.Serial]
	}
	if l == nil {
		writeJSON(w, ReleaseResp{Released: true, Message: "no active lease"})
		return
	}
	if !req.Force && req.LeaseID == "" && l.Session != req.Session {
		writeJSON(w, ReleaseResp{Released: false,
			Message: fmt.Sprintf("device held by %s (session %s); use --force to override", l.Holder, l.Session)})
		return
	}
	reason := "released"
	if req.Force && l.Session != req.Session {
		reason = "force-released"
	}
	b.expireLocked(l, time.Now(), reason)
	writeJSON(w, ReleaseResp{Released: true})
}

func (b *Broker) handleState(w http.ResponseWriter, _ *http.Request) {
	b.mu.Lock()
	resp := StateResp{Queues: map[string][]WaiterInfo{}}
	for _, l := range b.leases {
		resp.Leases = append(resp.Leases, leaseInfo(l))
	}
	for serial, q := range b.queues {
		for _, wt := range q {
			resp.Queues[serial] = append(resp.Queues[serial],
				WaiterInfo{Session: wt.Session, Holder: wt.Holder, Enqueued: wt.Enqueued})
		}
	}
	b.mu.Unlock()
	writeJSON(w, resp)
}

func (b *Broker) handleDevices(w http.ResponseWriter, _ *http.Request) {
	devs, err := ListDevices(b.config().RealADB, b.config().ClientServerPort())
	resp := DevicesResp{}
	if err != nil {
		resp.Error = err.Error()
	}
	b.mu.Lock()
	for _, d := range devs {
		di := DeviceInfo{Serial: d.Serial, State: d.State, Model: d.Model}
		if l := b.leases[d.Serial]; l != nil {
			info := leaseInfo(l)
			di.Lease = &info
		}
		di.Waiting = len(b.queues[d.Serial])
		di.Cleaning = b.cleaning[d.Serial]
		resp.Devices = append(resp.Devices, di)
	}
	// Leases for devices that disappeared still matter.
	for serial, l := range b.leases {
		found := false
		for _, d := range devs {
			if d.Serial == serial {
				found = true
				break
			}
		}
		if !found {
			info := leaseInfo(l)
			resp.Devices = append(resp.Devices, DeviceInfo{
				Serial: serial, State: "disconnected", Lease: &info,
				Waiting: len(b.queues[serial]),
			})
		}
	}
	b.mu.Unlock()
	writeJSON(w, resp)
}

func leaseInfo(l *Lease) LeaseInfo {
	info := LeaseInfo{
		ID: l.ID, Serial: l.Serial, Session: l.Session, Holder: l.Holder,
		PID: l.PID, AcquiredAt: l.AcquiredAt, LastActive: l.LastActive,
		Running: l.Running, IdleTTLSec: int(l.IdleTTL.Seconds()), Explicit: l.Explicit,
	}
	if l.Explicit {
		t := l.ExpiresAt
		info.ExpiresAt = &t
	}
	return info
}

func (b *Broker) leaseByIDLocked(id string) *Lease {
	for _, l := range b.leases {
		if l.ID == id {
			return l
		}
	}
	return nil
}

// expireLocked removes a lease and hands the device to the next session —
// via a cleanup pass first when enabled and a baseline exists.
func (b *Broker) expireLocked(l *Lease, now time.Time, reason string) {
	delete(b.leases, l.Serial)
	b.hist(reason, l, fmt.Sprintf("held %s", time.Since(l.AcquiredAt).Round(time.Second)))
	log.Printf("%s %s (was %s)", reason, l.Serial, l.Holder)
	if b.config().CleanupEnabled && len(l.Baseline) > 0 && !b.cleaning[l.Serial] {
		b.cleaning[l.Serial] = true
		go b.runCleanup(l)
		b.saveStateLocked()
		return
	}
	b.grantNextLocked(l.Serial, now)
	b.saveStateLocked()
}

// grantNextLocked pops the queue head for serial and grants it a lease.
// Other queued waiters from the same session piggyback on the same lease.
func (b *Broker) grantNextLocked(serial string, now time.Time) {
	q := b.queues[serial]
	// Drop waiters whose client stopped polling.
	alive := q[:0]
	for _, wt := range q {
		if wt.lease == nil && now.Sub(wt.LastPoll) > staleWaiter {
			delete(b.waiters, wt.ID)
			close(wt.ch) // wake any blocked local waiter; lease==nil signals the drop
			log.Printf("dropped stale waiter %s for %s", wt.Holder, serial)
			continue
		}
		alive = append(alive, wt)
	}
	if len(alive) == 0 {
		delete(b.queues, serial)
		return
	}
	head := alive[0]
	idle := head.IdleTTL
	if idle <= 0 {
		idle = time.Duration(b.config().IdleTTLSec) * time.Second
	}
	l := &Lease{
		ID: b.newID("lease"), Serial: serial, Session: head.Session,
		Holder: head.Holder, OwnerPID: OwnerPIDFromSession(head.Session),
		AcquiredAt: now, LastActive: now, LastBeat: now,
		IdleTTL: idle,
	}
	if head.Command {
		l.Running = 1
	}
	b.leases[serial] = l
	head.lease = l
	close(head.ch)

	rest := alive[1:]
	remaining := []*Waiter{}
	for _, wt := range rest {
		if wt.Session == head.Session {
			wt.lease = l
			if wt.Command {
				l.Running++
			}
			close(wt.ch)
		} else {
			remaining = append(remaining, wt)
		}
	}
	if len(remaining) == 0 {
		delete(b.queues, serial)
	} else {
		b.queues[serial] = remaining
	}
	b.hist("grant", l, "from queue")
	log.Printf("granted %s to %s (from queue, %d still waiting)", serial, l.Holder, len(remaining))
	if b.config().CleanupEnabled {
		go b.captureBaseline(l.ID, serial)
	}
}

// ---- background reaper ----

func (b *Broker) reaper() {
	for range time.Tick(reaperInterval) {
		b.reloadConfigIfChanged()
		b.sweep(time.Now())
	}
}

// sweep is one reaper pass at instant now: expire what is due and hand each
// freed device to its queue. Taking the instant as an argument keeps the
// handoff rules testable without sleeping through real TTLs.
func (b *Broker) sweep(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.leases {
		if !l.Claimed && now.Sub(l.AcquiredAt) > unclaimedGrace {
			b.expireLocked(l, now, "unclaimed")
			continue
		}
		// An agent that died never gets to release. Waiting out its TTL
		// strands the device for up to explicit_ttl_seconds — so once no
		// command is in flight and the owning process is gone, take the
		// device back. This is what makes an explicit `acquire` safe to
		// give a long TTL, and it needs nothing from the agent: no exit
		// hook, which a killed process would not run anyway.
		if l.Running == 0 && !processAlive(l.OwnerPID) {
			b.expireLocked(l, now, "owner-gone")
			continue
		}
		if l.Running > 0 && now.Sub(l.LastBeat) > orphanBeat {
			log.Printf("orphaned commands on %s (%s): resetting running count", l.Serial, l.Holder)
			l.Running = 0
			l.LastActive = now
		}
		switch {
		case l.Explicit:
			if l.Running == 0 && now.After(l.ExpiresAt) {
				b.expireLocked(l, now, "expired")
			}
		default:
			if l.Running == 0 && now.Sub(l.LastActive) > l.IdleTTL {
				b.expireLocked(l, now, "idle-released")
			}
		}
	}
	// GC waiters that were granted but never picked up their lease.
	for id, wt := range b.waiters {
		if wt.lease != nil && now.Sub(wt.LastPoll) > staleWaiter {
			delete(b.waiters, id)
		}
	}
}

// reloadConfigIfChanged picks up config edits (e.g. `adbharbor cleanup on`)
// without a daemon restart.
func (b *Broker) reloadConfigIfChanged() {
	info, err := os.Stat(ConfigPath())
	if err != nil || info.ModTime().Equal(b.cfgMTime) {
		return
	}
	cfg := LoadConfig()
	was := b.config().CleanupEnabled
	b.cfg.Store(cfg)
	b.cfgMTime = info.ModTime()
	if was != cfg.CleanupEnabled {
		log.Printf("config reloaded: cleanup %s", map[bool]string{true: "ENABLED", false: "disabled"}[cfg.CleanupEnabled])
	} else {
		log.Printf("config reloaded")
	}
}

// ---- persistence & history ----

type persistedState struct {
	Leases []*Lease `json:"leases"`
}

func (b *Broker) saveStateLocked() {
	st := persistedState{}
	for _, l := range b.leases {
		st.Leases = append(st.Leases, l)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := StatePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		os.Rename(tmp, StatePath())
	}
}

func (b *Broker) loadState() {
	data, err := os.ReadFile(StatePath())
	if err != nil {
		return
	}
	var st persistedState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	b.mu.Lock()
	for _, l := range st.Leases {
		// Restored leases have no live commands; idle expiry takes over.
		l.Running = 0
		l.Claimed = true
		l.LastBeat = time.Now()
		b.leases[l.Serial] = l
		log.Printf("restored lease on %s for %s", l.Serial, l.Holder)
	}
	b.mu.Unlock()
}

func (b *Broker) hist(event string, l *Lease, note string) {
	f, err := os.OpenFile(HistoryPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec := map[string]any{
		"ts": time.Now().Format(time.RFC3339), "event": event,
		"serial": l.Serial, "session": l.Session, "holder": l.Holder, "lease_id": l.ID,
	}
	if note != "" {
		rec["note"] = note
	}
	data, _ := json.Marshal(rec)
	f.Write(append(data, '\n'))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
