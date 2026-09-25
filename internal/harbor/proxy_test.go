package harbor

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseTransport(t *testing.T) {
	cases := []struct {
		req    string
		serial string
		tport  bool
		ok     bool
	}{
		{"host:transport:ABC123", "ABC123", false, true},
		{"host:tport:serial:ABC123", "ABC123", true, true},
		{"host:transport-any", "", false, true},
		{"host:tport:any", "", true, true},
		{"host:transport-usb", "", false, true},
		{"host:version", "", false, false},
		{"host:devices-l", "", false, false},
		{"host-serial:ABC:features", "", false, false},
		{"shell:ls", "", false, false},
	}
	for _, c := range cases {
		tr, ok := parseTransport(c.req)
		if ok != c.ok || tr.serial != c.serial || tr.tport != c.tport {
			t.Errorf("parseTransport(%q) = {%q %v} ok=%v, want {%q %v} ok=%v",
				c.req, tr.serial, tr.tport, ok, c.serial, c.tport, c.ok)
		}
	}
}

func TestIsExemptService(t *testing.T) {
	prefixes := DefaultConfig().ExemptShell
	cases := map[string]bool{
		"shell:getprop ro.product.model":            true,
		"shell,v2,TERM=xterm-256color:getprop ro.x": true,
		"exec:getprop ro.build.version.sdk":         true,
		"shell:dumpsys battery":                     true,
		"shell:pm list packages":                    true,
		"shell:settings get global adb_enabled":     true,
		"shell:am start -n com.foo/.Main":           false,
		"shell:pm install /data/local/tmp/app.apk":  false,
		"shell:pm uninstall com.foo":                false,
		"shell:input tap 100 200":                   false,
		"shell:":                                    false, // interactive shell
		"shell,v2,pty:":                             false,
		"sync:":                                     false,
		"exec:cmd package install-create":           false,
		"framebuffer:":                              false,
		"root:":                                     false,
		"shell:monkey -p com.foo 1":                 false,
	}
	for svc, want := range cases {
		if got := isExemptService(svc, prefixes); got != want {
			t.Errorf("isExemptService(%q) = %v, want %v", svc, got, want)
		}
	}
}

func TestEnvWithServerPort(t *testing.T) {
	env := []string{"PATH=/bin", "ANDROID_ADB_SERVER_PORT=5037", "HOME=/x"}
	got := envWithServerPort(env, 5038)
	found := false
	for _, e := range got {
		if e == "ANDROID_ADB_SERVER_PORT=5038" {
			found = true
		}
		if e == "ANDROID_ADB_SERVER_PORT=5037" {
			t.Error("old port entry should have been removed")
		}
	}
	if !found {
		t.Error("new port entry missing")
	}
}

func TestProxyWaiterDroppedWhenClientDisconnects(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WaitSec = 30
	b := &Broker{
		leases:   map[string]*Lease{},
		queues:   map[string][]*Waiter{},
		waiters:  map[string]*Waiter{},
		cleaning: map[string]bool{},
	}
	b.cfg.Store(cfg)

	now := time.Now()
	idle := time.Duration(cfg.IdleTTLSec) * time.Second
	holder := b.grantLocked(AcquireReq{
		Serial: "DEV1", Session: "holder-a", Holder: "holder-a", Command: true,
	}, now, idle)

	client, server := net.Pipe()
	defer client.Close()
	abort, stopWatch, _ := watchClientClose(server)
	defer stopWatch()

	done := make(chan error, 1)
	go func() {
		_, err := b.AcquireLocalBlocking(AcquireReq{
			Serial: "DEV1", Session: "waiter-b", Holder: "waiter-b", Command: true,
		}, cfg.WaitSec, abort)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if len(b.waiters) != 1 {
		t.Fatalf("expected 1 waiter, got %d", len(b.waiters))
	}

	client.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error when client disconnects")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireLocalBlocking did not return after client close")
	}

	if len(b.waiters) != 0 {
		t.Fatalf("waiter still queued: %d", len(b.waiters))
	}
	if len(b.queues["DEV1"]) != 0 {
		t.Fatalf("queue not empty: %d", len(b.queues["DEV1"]))
	}

	b.EndLeaseCommand(holder.ID)
	server.Close()
}

type countingConn struct {
	net.Conn
	active atomic.Int32
}

func (c *countingConn) Read(p []byte) (int, error) {
	c.active.Add(1)
	defer c.active.Add(-1)
	return c.Conn.Read(p)
}

func TestWatchClientCloseStopWaitsForReadExit(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	wrapped := &countingConn{Conn: server}
	_, stopWatch, _ := watchClientClose(wrapped)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if wrapped.active.Load() == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if wrapped.active.Load() != 1 {
		t.Fatal("watcher did not enter Read on underlying conn")
	}

	stopDone := make(chan struct{})
	go func() {
		stopWatch()
		close(stopDone)
	}()

	time.Sleep(30 * time.Millisecond)
	if wrapped.active.Load() == 1 {
		select {
		case <-stopDone:
			t.Fatal("stop returned while watcher still inside Read")
		default:
		}
	}

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return")
	}
	if wrapped.active.Load() != 0 {
		t.Fatalf("Read still active after stop: %d", wrapped.active.Load())
	}
}

func TestWatchClientCloseBuffersPayloadDuringWait(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	abort, stopWatch, out := watchClientClose(server)

	if _, err := client.Write([]byte{'X'}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	select {
	case <-abort:
		t.Fatal("payload during queue should not abort wait")
	default:
	}

	stopWatch()

	buf := make([]byte, 1)
	if _, err := out.Read(buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 'X' {
		t.Fatalf("byte=%q want X", buf[0])
	}
}
