package harbor

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// peerPID resolves the process on the other end of a loopback TCP
// connection given its source port, via lsof (macOS has no /proc). Returns
// the pid and process name.
func peerPID(srcPort int) (int, string, error) {
	out, err := exec.Command("lsof", "-nP",
		fmt.Sprintf("-iTCP:%d", srcPort), "-sTCP:ESTABLISHED", "-Fpc").Output()
	if err != nil {
		return 0, "", err
	}
	self := os.Getpid()
	pid := 0
	name := ""
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			name = ""
		case 'c':
			name = line[1:]
			// Both endpoints of the loopback pair show up; skip ourselves.
			if pid != 0 && pid != self {
				return pid, strings.ToLower(name), nil
			}
		}
	}
	return 0, "", fmt.Errorf("no peer process found for port %d", srcPort)
}
