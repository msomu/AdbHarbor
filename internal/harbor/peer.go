package harbor

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// peerPID resolves the process on the other end of a loopback TCP
// connection, via lsof (macOS has no /proc). Returns the pid and process
// name.
//
// The connection is identified by its full address pair, never by the
// client's port alone: `lsof -iTCP:<port>` matches any socket using that
// port number at EITHER end, so an unrelated process holding an ephemeral
// port of the same number would be reported as the caller and get charged
// for someone else's device lease.
func peerPID(local, remote *net.TCPAddr) (int, string, error) {
	return lookupPeer(local, remote, os.Getpid())
}

// lookupPeer is peerPID with the pid to ignore passed in, so tests can
// resolve a connection both ends of which they own.
func lookupPeer(local, remote *net.TCPAddr, self int) (int, string, error) {
	out, err := exec.Command("lsof", "-nP",
		"-iTCP@"+lsofAddr(remote), "-sTCP:ESTABLISHED", "-Fpcn").Output()
	if err != nil {
		return 0, "", fmt.Errorf("lsof for %s: %w", remote, err)
	}
	// Both endpoints of a loopback pair are listed, each naming the
	// connection from its own side: the peer's socket reads remote->local,
	// ours reads local->remote.
	return parseLsofPeer(string(out), lsofAddr(remote)+"->"+lsofAddr(local), self)
}

// lsofAddr renders an address the way `lsof -nP -Fn` does.
func lsofAddr(a *net.TCPAddr) string {
	if ip4 := a.IP.To4(); ip4 != nil {
		return fmt.Sprintf("%s:%d", ip4, a.Port)
	}
	return fmt.Sprintf("[%s]:%d", a.IP, a.Port)
}

// parseLsofPeer scans `lsof -Fpcn` field output for the process owning the
// socket named want. Fields come as one process header (p, c) followed by a
// set per open file (f, n), so pid and name carry forward until the next p.
func parseLsofPeer(out, want string, self int) (int, string, error) {
	pid := 0
	name := ""
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			name = ""
		case 'c':
			name = strings.ToLower(line[1:])
		case 'n':
			if line[1:] == want && pid > 0 && pid != self {
				return pid, name, nil
			}
		}
	}
	return 0, "", fmt.Errorf("no peer process for %s", want)
}
