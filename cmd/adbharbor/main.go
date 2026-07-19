// adbharbor is a lock broker for shared Android devices. Installed as an
// `adb` shim, it gives each agent session an exclusive lease on a device and
// queues everyone else, so parallel agents never fight over the same phone.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/msomu/AdbHarbor/internal/harbor"
)

const usage = `adbharbor %s — lock broker for shared Android devices

Usage:
  adbharbor install [--no-rc]   Install the adb shim, discover real adb, patch shell rc
  adbharbor uninstall           Remove the shim and shell rc block (keeps data dir)
  adbharbor devices             List devices with lease/queue state
  adbharbor status              Show all leases and queues
  adbharbor who -s SERIAL       Show who holds a device
  adbharbor acquire -s SERIAL [--ttl 15m] [--session NAME]
                                Hold a device explicitly (until release or TTL)
  adbharbor release -s SERIAL [--force]
                                Release a lease (--force releases someone else's)
  adbharbor cleanup [on|off]    Show or toggle uninstall-on-release of
                                session-installed apps (default: off)
  adbharbor doctor              Check shim, real adb, daemon, session detection
  adbharbor daemon              Run the broker in the foreground (usually automatic)
  adbharbor stop                Stop the broker daemon
  adbharbor version             Print version

When invoked as "adb" (via the installed symlink), adbharbor transparently
wraps the real adb: device-targeted commands acquire a lease first and wait
in a FIFO queue while another session holds the device.

Environment:
  ADB_HARBOR_SESSION  Explicit session key (otherwise auto-detected)
  ADB_HARBOR_IDLE     Seconds a lease lingers after the last command (default 60)
  ADB_HARBOR_WAIT     Max seconds to wait for a busy device (default 600)
  ADB_HARBOR_ADB      Path to the real adb binary (overrides config)
  ADB_HARBOR_DIR      Data directory (default ~/.adbharbor)
  ADB_HARBOR_OFF=1    Bypass locking entirely (plain passthrough)
`

func main() {
	// Invoked via the `adb` symlink -> transparent shim mode.
	if filepath.Base(os.Args[0]) == "adb" {
		os.Exit(harbor.RunShim(os.Args[1:]))
	}

	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Printf(usage, harbor.Version)
		return
	}
	var err error
	switch args[0] {
	case "daemon":
		err = harbor.RunDaemon()
	case "stop":
		err = harbor.CmdStop()
	case "install":
		err = harbor.CmdInstall(args[1:])
	case "uninstall":
		err = harbor.CmdUninstall()
	case "devices":
		err = harbor.CmdDevices()
	case "status":
		err = harbor.CmdStatus()
	case "who":
		err = harbor.CmdWho(args[1:])
	case "acquire":
		err = harbor.CmdAcquire(args[1:])
	case "release":
		err = harbor.CmdRelease(args[1:], false)
	case "force-release":
		err = harbor.CmdRelease(args[1:], true)
	case "cleanup":
		err = harbor.CmdCleanup(args[1:])
	case "doctor":
		err = harbor.CmdDoctor()
	case "shim":
		// Debug entry point: run the shim without the argv[0] trick.
		os.Exit(harbor.RunShim(args[1:]))
	case "version", "--version", "-v":
		fmt.Println("adbharbor", harbor.Version)
	case "help", "--help", "-h":
		fmt.Printf(usage, harbor.Version)
	default:
		fmt.Fprintf(os.Stderr, "adbharbor: unknown command %q\n\n", args[0])
		fmt.Printf(usage, harbor.Version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "adbharbor:", err)
		os.Exit(1)
	}
}
