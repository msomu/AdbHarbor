package harbor

import "strings"

// ADBInvocation is the result of parsing an adb command line's global flags.
type ADBInvocation struct {
	Serial      string
	TransportID string
	UseUSB      bool
	UseEmulator bool
	Command     string
}

// Global adb flags that consume a value.
var valueFlags = map[string]bool{
	"-s": true, "-t": true, "-H": true, "-P": true, "-L": true,
	"--one-device": true,
}

// Commands that never target a specific device and pass straight through.
var devicelessCmds = map[string]bool{
	"": true, "help": true, "--help": true, "version": true, "--version": true,
	"devices": true, "start-server": true, "kill-server": true,
	"connect": true, "disconnect": true, "pair": true, "mdns": true,
	"keygen": true, "reconnect": true, "host-features": true,
}

func ParseInvocation(args []string) ADBInvocation {
	inv := ADBInvocation{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			inv.Command = a
			break
		}
		switch {
		case a == "-s" && i+1 < len(args):
			inv.Serial = args[i+1]
			i++
		case a == "-t" && i+1 < len(args):
			inv.TransportID = args[i+1]
			i++
		case a == "-d":
			inv.UseUSB = true
		case a == "-e":
			inv.UseEmulator = true
		case valueFlags[a] && i+1 < len(args):
			i++
		}
	}
	return inv
}

func (inv ADBInvocation) NeedsDevice() bool {
	return !devicelessCmds[inv.Command]
}
