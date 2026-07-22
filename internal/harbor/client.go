package harbor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

var ErrWaitTimeout = errors.New("timed out waiting for device")

func unixClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", SocketPath())
			},
		},
	}
}

func call(path string, reqBody, respBody any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var body bytes.Buffer
	if reqBody != nil {
		if err := json.NewEncoder(&body).Encode(reqBody); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+path, &body)
	if err != nil {
		return err
	}
	resp, err := unixClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("broker: %s", resp.Status)
	}
	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}

func Ping() error {
	var p PingResp
	return call("/v1/ping", nil, &p, 500*time.Millisecond)
}

// selfPath returns the resolved adbharbor binary path (never the adb symlink).
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil
	}
	return resolved, nil
}

// EnsureDaemon starts the broker daemon if it isn't running.
func EnsureDaemon() error {
	if Ping() == nil {
		return nil
	}
	if err := EnsureDir(); err != nil {
		return err
	}
	self, err := selfPath()
	if err != nil {
		return err
	}
	logF, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logF.Close()
	cmd := exec.Command(self, "daemon")
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	cmd.Process.Release()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if Ping() == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("daemon did not come up within 3s (see " + LogPath() + ")")
}

// AcquireBlocking acquires a lease, waiting in the queue if the device is
// busy. waitSec <= 0 means wait forever.
func AcquireBlocking(req AcquireReq, waitSec int, progress func(msg string)) (string, error) {
	var deadline time.Time
	if waitSec > 0 {
		deadline = time.Now().Add(time.Duration(waitSec) * time.Second)
	}
	lastMsg := ""
	notify := func(msg string) {
		if progress != nil && msg != lastMsg {
			progress(msg)
			lastMsg = msg
		}
	}
	for {
		var ar AcquireResp
		if err := call("/v1/acquire", req, &ar, 10*time.Second); err != nil {
			return "", err
		}
		if ar.Granted {
			return ar.LeaseID, nil
		}
		notify(fmt.Sprintf("device %s is busy (held by %s); waiting in queue (position %d)",
			req.Serial, ar.HolderDesc, ar.Position))
		for {
			pollMS := 25000
			if !deadline.IsZero() {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return "", ErrWaitTimeout
				}
				if ms := int(remaining.Milliseconds()); ms < pollMS {
					pollMS = ms
				}
			}
			var wr WaitResp
			err := call("/v1/wait", WaitReq{WaiterID: ar.WaiterID, WaitMS: pollMS}, &wr, 35*time.Second)
			if err != nil {
				return "", err
			}
			if wr.Granted {
				return wr.LeaseID, nil
			}
			if wr.Expired {
				break // waiter dropped by broker; re-acquire from scratch
			}
			notify(fmt.Sprintf("still waiting for %s (held by %s, position %d)",
				req.Serial, wr.HolderDesc, wr.Position))
		}
	}
}

func Heartbeat(leaseID string)  { call("/v1/heartbeat", LeaseRef{LeaseID: leaseID}, nil, 5*time.Second) }
func EndCommand(leaseID string) { call("/v1/end", LeaseRef{LeaseID: leaseID}, nil, 5*time.Second) }

func AcquireAny(req AcquireAnyReq) (AcquireAnyResp, error) {
	var resp AcquireAnyResp
	err := call("/v1/acquire-any", req, &resp, 15*time.Second)
	return resp, err
}

func SetETA(req ETAReq) (ETAResp, error) {
	var resp ETAResp
	err := call("/v1/eta", req, &resp, 5*time.Second)
	return resp, err
}

func Release(req ReleaseReq) (ReleaseResp, error) {
	var resp ReleaseResp
	err := call("/v1/release", req, &resp, 10*time.Second)
	return resp, err
}

func FetchState() (StateResp, error) {
	var st StateResp
	err := call("/v1/state", nil, &st, 10*time.Second)
	return st, err
}

func FetchDevices() (DevicesResp, error) {
	var d DevicesResp
	err := call("/v1/devices", nil, &d, 15*time.Second)
	return d, err
}

// CmdStop terminates a running daemon via its pid file.
func CmdStop() error {
	data, err := os.ReadFile(PIDPath())
	if err != nil {
		fmt.Println("daemon not running")
		return nil
	}
	var pid int
	fmt.Sscanf(string(data), "%d", &pid)
	if pid <= 1 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Println("daemon not running")
		return nil
	}
	fmt.Printf("stopped daemon (pid %d)\n", pid)
	return nil
}
