package harbor

import "time"

type AcquireReq struct {
	Serial     string `json:"serial"`
	Session    string `json:"session"`
	Holder     string `json:"holder"`
	PID        int    `json:"pid"`
	IdleTTLSec int    `json:"idle_ttl_sec,omitempty"`
	TTLSec     int    `json:"ttl_sec,omitempty"`
	Explicit   bool   `json:"explicit,omitempty"`
	// Command marks the acquisition as a running shim command: the lease's
	// running counter is incremented and must be balanced by /v1/end.
	Command bool `json:"command,omitempty"`
	// ETASec is how long the caller expects to need the device, and ETANote
	// what for. Advisory: shown to whoever queues behind this lease.
	ETASec  int    `json:"eta_sec,omitempty"`
	ETANote string `json:"eta_note,omitempty"`
}

// ETAReq updates the advertised finish time of the caller's own lease.
// Serial empty means "whichever device my session holds".
type ETAReq struct {
	Session string `json:"session"`
	Serial  string `json:"serial,omitempty"`
	ETASec  int    `json:"eta_sec"`
	Note    string `json:"note,omitempty"`
	Clear   bool   `json:"clear,omitempty"`
}

type ETAResp struct {
	OK      bool   `json:"ok"`
	Serial  string `json:"serial,omitempty"`
	Message string `json:"message,omitempty"`
}

type AcquireResp struct {
	Granted    bool   `json:"granted"`
	LeaseID    string `json:"lease_id,omitempty"`
	WaiterID   string `json:"waiter_id,omitempty"`
	Position   int    `json:"position,omitempty"`
	HolderDesc string `json:"holder_desc,omitempty"`
}

type WaitReq struct {
	WaiterID string `json:"waiter_id"`
	WaitMS   int    `json:"wait_ms"`
}

type WaitResp struct {
	Granted    bool   `json:"granted"`
	LeaseID    string `json:"lease_id,omitempty"`
	Position   int    `json:"position,omitempty"`
	HolderDesc string `json:"holder_desc,omitempty"`
	// Expired means the waiter is no longer known; the client must re-acquire.
	Expired bool `json:"expired,omitempty"`
}

type LeaseRef struct {
	LeaseID string `json:"lease_id"`
}

// AcquireAnyReq asks the broker to atomically pick and lease any free
// device matching the constraints.
type AcquireAnyReq struct {
	Session  string `json:"session"`
	Holder   string `json:"holder"`
	PID      int    `json:"pid"`
	TTLSec   int    `json:"ttl_sec,omitempty"`
	ETASec   int    `json:"eta_sec,omitempty"`
	ETANote  string `json:"eta_note,omitempty"`
	USB      bool   `json:"usb,omitempty"`
	Emulator bool   `json:"emulator,omitempty"`
}

type AcquireAnyResp struct {
	Granted bool   `json:"granted"`
	Serial  string `json:"serial,omitempty"`
	LeaseID string `json:"lease_id,omitempty"`
	Message string `json:"message,omitempty"`
}

type ReleaseReq struct {
	Serial  string `json:"serial,omitempty"`
	LeaseID string `json:"lease_id,omitempty"`
	Session string `json:"session,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

type ReleaseResp struct {
	Released bool   `json:"released"`
	Message  string `json:"message,omitempty"`
}

type LeaseInfo struct {
	ID         string     `json:"id"`
	Serial     string     `json:"serial"`
	Session    string     `json:"session"`
	Holder     string     `json:"holder"`
	PID        int        `json:"pid"`
	AcquiredAt time.Time  `json:"acquired_at"`
	LastActive time.Time  `json:"last_active"`
	Running    int        `json:"running"`
	IdleTTLSec int        `json:"idle_ttl_sec"`
	Explicit   bool       `json:"explicit"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	ETA        *time.Time `json:"eta,omitempty"`
	ETANote    string     `json:"eta_note,omitempty"`
}

type WaiterInfo struct {
	Session  string    `json:"session"`
	Holder   string    `json:"holder"`
	Enqueued time.Time `json:"enqueued"`
}

type StateResp struct {
	Leases []LeaseInfo             `json:"leases"`
	Queues map[string][]WaiterInfo `json:"queues"`
}

type DeviceInfo struct {
	Serial   string     `json:"serial"`
	State    string     `json:"state"`
	Model    string     `json:"model,omitempty"`
	Lease    *LeaseInfo `json:"lease,omitempty"`
	Waiting  int        `json:"waiting"`
	Cleaning bool       `json:"cleaning,omitempty"`
}

type DevicesResp struct {
	Devices []DeviceInfo `json:"devices"`
	Error   string       `json:"error,omitempty"`
}

type PingResp struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}
