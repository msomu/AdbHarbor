package harbor

import (
	"net"
	"os"
	"os/exec"
	"testing"
)

// Realistic `lsof -nP -iTCP@127.0.0.1:56936 -sTCP:ESTABLISHED -Fpcn` output:
// the adb client we want, the harbor daemon's own end of the same
// connection, and a Continuity daemon that merely happens to hold an
// unrelated socket numbered 56936.
const lsofSample = `p385
crapportd
f24
n192.168.1.7:56936->17.248.150.6:443
p27260
cadbharbor
f10
n127.0.0.1:5037->127.0.0.1:56936
p99856
cadb
f3
n127.0.0.1:56936->127.0.0.1:5037
`

func TestParseLsofPeerPicksTheRightConnection(t *testing.T) {
	pid, name, err := parseLsofPeer(lsofSample, "127.0.0.1:56936->127.0.0.1:5037", 27260)
	if err != nil {
		t.Fatalf("parseLsofPeer: %v", err)
	}
	if pid != 99856 || name != "adb" {
		t.Errorf("got pid=%d name=%q, want 99856 adb", pid, name)
	}
}

func TestParseLsofPeerSkipsSelf(t *testing.T) {
	// The daemon's own socket for this connection, seen from its side.
	if _, _, err := parseLsofPeer(lsofSample, "127.0.0.1:5037->127.0.0.1:56936", 27260); err == nil {
		t.Error("matched our own socket, want error")
	}
}

func TestParseLsofPeerNoMatch(t *testing.T) {
	if _, _, err := parseLsofPeer(lsofSample, "127.0.0.1:1->127.0.0.1:2", 0); err == nil {
		t.Error("want error for an address pair that is not present")
	}
}

func TestLsofAddr(t *testing.T) {
	cases := []struct {
		addr *net.TCPAddr
		want string
	}{
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5037}, "127.0.0.1:5037"},
		{&net.TCPAddr{IP: net.ParseIP("::1"), Port: 5037}, "[::1]:5037"},
	}
	for _, c := range cases {
		if got := lsofAddr(c.addr); got != c.want {
			t.Errorf("lsofAddr(%v) = %q, want %q", c.addr, got, c.want)
		}
	}
}

// TestLookupPeerLiveSocket resolves a real loopback connection end to end.
// Both ends belong to this test binary, so self is 0 to disable the
// skip-ourselves guard; the answer must be this process.
func TestLookupPeerLiveSocket(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not available")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	dialed := make(chan net.Conn, 1)
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			dialed <- nil
			return
		}
		dialed <- c
	}()

	srv, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer srv.Close()
	client := <-dialed
	if client == nil {
		t.Fatal("dial failed")
	}
	defer client.Close()

	pid, name, err := lookupPeer(srv.LocalAddr().(*net.TCPAddr), srv.RemoteAddr().(*net.TCPAddr), 0)
	if err != nil {
		t.Fatalf("lookupPeer: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("got pid=%d (%s), want this process %d", pid, name, os.Getpid())
	}
}
