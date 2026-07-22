package harbor

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

var proxyDebug = os.Getenv("ADB_HARBOR_DEBUG") == "1"

// The ADB server proxy: the harbor daemon listens on the default ADB server
// port (5037) and the real adb server is parked on cfg.AdbServerPort. Every
// ADB client on the machine — adb CLIs at any path, Maestro/dadb, ddmlib —
// connects to 5037 by default, so every device-mutating service passes
// through the broker with zero client configuration. Machines without
// AdbHarbor simply have a normal adb server on 5037: clients stay agnostic.
//
// Wire protocol (smart socket): requests are 4-hex-digit length + payload.
// A client first issues host:* requests (version, devices, transport, ...).
// After a transport request succeeds, the next request names the service
// (shell:..., sync:, exec:, ...) and the stream goes raw. The proxy relays
// the transport handshake, classifies the service, acquires a lease for
// device-mutating services, then splices bytes.

func (b *Broker) runProxy() error {
	addr := fmt.Sprintf("127.0.0.1:%d", b.config().ProxyPort)
	var ln net.Listener
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		// Port busy: a classic adb server owns it. Evict it (it will be
		// re-homed on AdbServerPort) and retry.
		log.Printf("proxy: %s busy, evicting classic adb server", addr)
		killCmd := exec.Command(b.config().RealADB, "kill-server")
		killCmd.Env = envWithServerPort(os.Environ(), b.config().ProxyPort)
		_ = killCmd.Run()
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("could not take over ADB server port: %w", err)
	}
	b.ensureRealServer()
	log.Printf("proxy: brokering ADB server port %d (real server on %d)", b.config().ProxyPort, b.config().AdbServerPort)
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go b.proxyConn(c)
	}
}

// ensureRealServer makes sure the real adb server is up on AdbServerPort.
// The adb CLI auto-spawns its server, so running any command against that
// port is sufficient.
func (b *Broker) ensureRealServer() {
	if b.dialReal(200*time.Millisecond) == nil {
		return
	}
	cmd := exec.Command(b.config().RealADB, "start-server")
	cmd.Env = envWithServerPort(os.Environ(), b.config().AdbServerPort)
	if err := cmd.Run(); err != nil {
		log.Printf("proxy: starting real adb server: %v", err)
		return
	}
	for i := 0; i < 30; i++ {
		if b.dialReal(200*time.Millisecond) == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("proxy: real adb server did not come up on %d", b.config().AdbServerPort)
}

func (b *Broker) dialReal(timeout time.Duration) error {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", b.config().AdbServerPort), timeout)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

func (b *Broker) dialUpstream() (net.Conn, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", b.config().AdbServerPort)
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		return c, nil
	}
	b.ensureRealServer()
	return net.DialTimeout("tcp", addr, 2*time.Second)
}

func (b *Broker) proxyConn(c net.Conn) {
	defer c.Close()
	var up net.Conn
	defer func() {
		if up != nil {
			up.Close()
		}
	}()

	transported := false
	serial := ""
	for {
		req, err := readMsg(c)
		if err != nil {
			return
		}
		if proxyDebug {
			log.Printf("proxy: <- %q (transported=%v)", firstLine(req), transported)
		}
		if !transported {
			if t, ok := parseTransport(req); ok {
				if up == nil {
					if up, err = b.dialUpstream(); err != nil {
						writeFail(c, "adbharbor: real adb server unavailable")
						return
					}
				}
				if err := writeMsg(up, req); err != nil {
					return
				}
				if !relayTransportStatus(up, c, t.tport) {
					return
				}
				serial = t.serial
				if serial == "" {
					serial = b.soleOnlineSerial()
				}
				transported = true
				continue
			}
			// Plain host:* request (version/devices/track-devices/kill/
			// host-serial:*): pure relay.
			if up == nil {
				if up, err = b.dialUpstream(); err != nil {
					writeFail(c, "adbharbor: real adb server unavailable")
					return
				}
			}
			if err := writeMsg(up, req); err != nil {
				return
			}
			splice(c, up)
			return
		}

		// After transport: req is the device service.
		svc := req
		if serial == "" || isExemptService(svc, b.config().ExemptShell) {
			if err := writeMsg(up, svc); err != nil {
				return
			}
			splice(c, up)
			return
		}

		session, procName, owner, observer := b.sessionForConn(c)
		if observer {
			// Passive tools (screen mirrors) stream forever but must not
			// own the device.
			if err := writeMsg(up, svc); err != nil {
				return
			}
			splice(c, up)
			return
		}
		lease, err := b.AcquireLocalBlocking(AcquireReq{
			Serial:   serial,
			Session:  session,
			Holder:   fmt.Sprintf("%s (%s)", session, procName),
			OwnerPID: owner,
			Command:  true,
		}, b.config().WaitSec, nil)
		if err != nil {
			log.Printf("proxy: %s denied %s on %s: %v", session, firstLine(svc), serial, err)
			writeFail(c, fmt.Sprintf("adbharbor: device %s is busy (%v); see `adbharbor who -s %s`", serial, err, serial))
			return
		}
		if err := writeMsg(up, svc); err != nil {
			b.EndLeaseCommand(lease.ID)
			return
		}
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(heartbeatEvery)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					b.TouchLease(lease.ID)
				case <-stop:
					return
				}
			}
		}()
		splice(c, up)
		close(stop)
		b.EndLeaseCommand(lease.ID)
		return
	}
}

// soleOnlineSerial resolves transport-any style requests: if exactly one
// device is online we can still broker; otherwise stay hands-off.
func (b *Broker) soleOnlineSerial() string {
	devs, err := ListDevices(b.config().RealADB, b.config().AdbServerPort)
	if err != nil {
		return ""
	}
	sole := ""
	for _, d := range devs {
		if d.State != "device" {
			continue
		}
		if sole != "" {
			return ""
		}
		sole = d.Serial
	}
	return sole
}

func (b *Broker) sessionForConn(c net.Conn) (session, procName string, owner int, observer bool) {
	remote, ok := c.RemoteAddr().(*net.TCPAddr)
	local, okLocal := c.LocalAddr().(*net.TCPAddr)
	if !ok || !okLocal {
		return "unknown", "unknown", 0, false
	}
	pid, name, err := peerPID(local, remote)
	if err != nil || pid <= 0 {
		return fmt.Sprintf("port-%d", remote.Port), "unknown", 0, false
	}
	cfg := b.config()
	if isObserverProc(name, cfg.ObserverProcs) {
		return name, name, pid, true
	}
	session, _, owner, observer = classifyClient(pid, cfg)
	return session, name, owner, observer
}

// ---- transport request parsing ----

type transportReq struct {
	serial string // "" = server-chosen (any/usb/local)
	tport  bool   // response carries an 8-byte transport id after OKAY
}

func parseTransport(req string) (transportReq, bool) {
	switch {
	case strings.HasPrefix(req, "host:transport:"):
		return transportReq{serial: strings.TrimPrefix(req, "host:transport:")}, true
	case strings.HasPrefix(req, "host:tport:serial:"):
		return transportReq{serial: strings.TrimPrefix(req, "host:tport:serial:"), tport: true}, true
	case req == "host:transport-any", req == "host:transport-usb", req == "host:transport-local":
		return transportReq{}, true
	case req == "host:tport:any", req == "host:tport:usb", req == "host:tport:local":
		return transportReq{tport: true}, true
	}
	return transportReq{}, false
}

func isObserverProc(name string, observers []string) bool {
	for _, o := range observers {
		if name == o || (len(o) >= 5 && strings.Contains(name, o)) {
			return true
		}
	}
	return false
}

// isExemptService reports whether a device service is read-only enough to
// run without a lease. Only shell/exec command content is considered.
func isExemptService(svc string, prefixes []string) bool {
	head, cmd, found := strings.Cut(svc, ":")
	if !found {
		return false
	}
	family, _, _ := strings.Cut(head, ",")
	if family != "shell" && family != "exec" {
		return false
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false // interactive shell
	}
	for _, p := range prefixes {
		if strings.HasPrefix(cmd, p) {
			return true
		}
	}
	return false
}

// ---- wire helpers ----

func readMsg(c net.Conn) (string, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		return "", err
	}
	var n int
	if _, err := fmt.Sscanf(string(lenBuf[:]), "%04x", &n); err != nil {
		return "", fmt.Errorf("bad length %q", lenBuf)
	}
	if n < 0 || n > 1<<20 {
		return "", fmt.Errorf("unreasonable length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func writeMsg(c net.Conn, msg string) error {
	_, err := fmt.Fprintf(c, "%04x%s", len(msg), msg)
	return err
}

func writeFail(c net.Conn, msg string) {
	fmt.Fprintf(c, "FAIL%04x%s", len(msg), msg)
}

// relayTransportStatus forwards the OKAY/FAIL for a transport request from
// the real server to the client. Returns false when the conversation ended.
func relayTransportStatus(up, c net.Conn, tport bool) bool {
	var status [4]byte
	if _, err := io.ReadFull(up, status[:]); err != nil {
		return false
	}
	if _, err := c.Write(status[:]); err != nil {
		return false
	}
	switch string(status[:]) {
	case "OKAY":
		if tport {
			var id [8]byte
			if _, err := io.ReadFull(up, id[:]); err != nil {
				return false
			}
			if _, err := c.Write(id[:]); err != nil {
				return false
			}
		}
		return true
	default: // FAIL: relay hex4+message, then the conversation is over.
		var lenBuf [4]byte
		if _, err := io.ReadFull(up, lenBuf[:]); err != nil {
			return false
		}
		c.Write(lenBuf[:])
		var n int
		if _, err := fmt.Sscanf(string(lenBuf[:]), "%04x", &n); err == nil && n > 0 && n <= 1<<20 {
			io.CopyN(c, up, int64(n))
		}
		return false
	}
}

// splice pipes bytes both ways. Client-side EOF half-closes upstream so the
// server sees stdin end; the session finishes when upstream closes.
func splice(c, up net.Conn) {
	go func() {
		io.Copy(up, c)
		if t, ok := up.(*net.TCPConn); ok {
			t.CloseWrite()
		}
	}()
	io.Copy(c, up)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func envWithServerPort(env []string, port int) []string {
	return envWith(env, "ANDROID_ADB_SERVER_PORT", fmt.Sprintf("%d", port))
}

func envWith(env []string, key, value string) []string {
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, key+"="+value)
}
