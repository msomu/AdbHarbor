package harbor

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

func CmdDevices() error {
	if err := EnsureDaemon(); err != nil {
		return err
	}
	resp, err := FetchDevices()
	if err != nil {
		return err
	}
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, "adbharbor:", resp.Error)
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERIAL\tSTATE\tMODEL\tLEASE\tQUEUE")
	sort.Slice(resp.Devices, func(i, j int) bool { return resp.Devices[i].Serial < resp.Devices[j].Serial })
	for _, d := range resp.Devices {
		lease := "free"
		if d.Cleaning {
			lease = "session cleanup"
		}
		if d.Lease != nil {
			lease = fmt.Sprintf("%s (%s%s)", d.Lease.Holder,
				time.Since(d.Lease.AcquiredAt).Round(time.Second), runningSuffix(d.Lease.Running))
		}
		queue := "-"
		if d.Waiting > 0 {
			queue = fmt.Sprintf("%d waiting", d.Waiting)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Serial, d.State, d.Model, lease, queue)
	}
	return tw.Flush()
}

func runningSuffix(n int) string {
	if n > 0 {
		return fmt.Sprintf(", %d running", n)
	}
	return ""
}

func CmdStatus() error {
	if err := EnsureDaemon(); err != nil {
		return err
	}
	st, err := FetchState()
	if err != nil {
		return err
	}
	if len(st.Leases) == 0 {
		fmt.Println("no active leases")
	}
	for _, l := range st.Leases {
		fmt.Printf("%s  held by %s  session=%s  age=%s  running=%d",
			l.Serial, l.Holder, l.Session, time.Since(l.AcquiredAt).Round(time.Second), l.Running)
		if l.Explicit && l.ExpiresAt != nil {
			fmt.Printf("  expires=%s", time.Until(*l.ExpiresAt).Round(time.Second))
		} else {
			fmt.Printf("  idle-ttl=%ds", l.IdleTTLSec)
		}
		fmt.Println()
		for i, wt := range st.Queues[l.Serial] {
			fmt.Printf("    queue[%d]: %s (waiting %s)\n", i+1, wt.Holder, time.Since(wt.Enqueued).Round(time.Second))
		}
	}
	return nil
}

func CmdWho(args []string) error {
	serial, err := serialArg(args, "who")
	if err != nil {
		return err
	}
	if err := EnsureDaemon(); err != nil {
		return err
	}
	st, err := FetchState()
	if err != nil {
		return err
	}
	for _, l := range st.Leases {
		if l.Serial == serial {
			fmt.Printf("%s is held by %s (session %s) since %s, %d command(s) running\n",
				serial, l.Holder, l.Session, l.AcquiredAt.Format(time.Kitchen), l.Running)
			for i, wt := range st.Queues[serial] {
				fmt.Printf("  queue[%d]: %s\n", i+1, wt.Holder)
			}
			return nil
		}
	}
	fmt.Printf("%s is free\n", serial)
	return nil
}

func CmdAcquire(args []string) error {
	fs := flag.NewFlagSet("acquire", flag.ContinueOnError)
	serial := fs.String("s", "", "device serial")
	any := fs.Bool("any", false, "lease any free device (prints its serial on stdout)")
	usb := fs.Bool("usb", false, "with --any: only USB devices")
	emulator := fs.Bool("emulator", false, "with --any: only emulators")
	ttl := fs.Duration("ttl", 0, "how long to hold the lease (default from config)")
	sessionFlag := fs.String("session", "", "session key (default: auto-detected)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *any {
		return cmdAcquireAny(*usb, *emulator, *ttl, *sessionFlag)
	}
	if *serial == "" {
		return fmt.Errorf("acquire: -s SERIAL or --any is required")
	}
	if err := EnsureDaemon(); err != nil {
		return err
	}
	cfg := LoadConfig()
	session := *sessionFlag
	if session == "" {
		session = DetectSession(cfg)
	}
	req := AcquireReq{
		Serial:  *serial,
		Session: session,
		Holder:  HolderDesc(session),
		PID:     os.Getpid(),
		TTLSec:  int(ttl.Seconds()),
		Explicit: true,
	}
	leaseID, err := AcquireBlocking(req, envInt("ADB_HARBOR_WAIT", cfg.WaitSec), func(msg string) {
		fmt.Fprintln(os.Stderr, "adbharbor:", msg)
	})
	if err != nil {
		return err
	}
	ttlSec := int(ttl.Seconds())
	if ttlSec == 0 {
		ttlSec = cfg.ExplicitTTLSec
	}
	fmt.Printf("acquired %s for session %s (lease %s, expires in %s)\n",
		*serial, session, leaseID, (time.Duration(ttlSec) * time.Second))
	fmt.Printf("release with: adbharbor release -s %s\n", *serial)
	return nil
}

// cmdAcquireAny leases any free matching device. The chosen serial is the
// ONLY thing printed to stdout, so scripts and agents can do:
//
//	S=$(adbharbor acquire --any) && adb -s "$S" ...
func cmdAcquireAny(usb, emulator bool, ttl time.Duration, sessionFlag string) error {
	if err := EnsureDaemon(); err != nil {
		return err
	}
	cfg := LoadConfig()
	session := sessionFlag
	if session == "" {
		session = DetectSession(cfg)
	}
	resp, err := AcquireAny(AcquireAnyReq{
		Session: session,
		Holder:  HolderDesc(session),
		PID:     os.Getpid(),
		TTLSec:  int(ttl.Seconds()),
		USB:     usb,
		Emulator: emulator,
	})
	if err != nil {
		return err
	}
	if !resp.Granted {
		fmt.Fprintf(os.Stderr, "adbharbor: %s\n", resp.Message)
		fmt.Fprintln(os.Stderr, "adbharbor: retry later, or queue on a specific device with plain `adb -s SERIAL ...`")
		os.Exit(75)
	}
	ttlSec := int(ttl.Seconds())
	if ttlSec == 0 {
		ttlSec = cfg.ExplicitTTLSec
	}
	fmt.Fprintf(os.Stderr, "adbharbor: leased %s for session %s (expires in %s; release with: adbharbor release -s %s)\n",
		resp.Serial, session, time.Duration(ttlSec)*time.Second, resp.Serial)
	fmt.Println(resp.Serial)
	return nil
}

func CmdRelease(args []string, force bool) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	serial := fs.String("s", "", "device serial (required)")
	forceFlag := fs.Bool("force", false, "release even if another session holds the lease")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *serial == "" {
		return fmt.Errorf("release: -s SERIAL is required")
	}
	if err := EnsureDaemon(); err != nil {
		return err
	}
	cfg := LoadConfig()
	resp, err := Release(ReleaseReq{
		Serial:  *serial,
		Session: DetectSession(cfg),
		Force:   force || *forceFlag,
	})
	if err != nil {
		return err
	}
	if !resp.Released {
		return fmt.Errorf("%s", resp.Message)
	}
	fmt.Printf("released %s\n", *serial)
	return nil
}

func serialArg(args []string, cmd string) (string, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	serial := fs.String("s", "", "device serial")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *serial == "" && fs.NArg() > 0 {
		return fs.Arg(0), nil
	}
	if *serial == "" {
		return "", fmt.Errorf("%s: -s SERIAL is required", cmd)
	}
	return *serial, nil
}

// CmdCleanup shows or toggles session cleanup (uninstall-on-release).
func CmdCleanup(args []string) error {
	cfg := LoadRawConfig()
	if len(args) == 0 {
		state := "disabled"
		if cfg.CleanupEnabled {
			state = "ENABLED"
		}
		fmt.Printf("session cleanup: %s\n", state)
		fmt.Println("  when enabled, packages installed during a session are uninstalled")
		fmt.Println("  when its lease ends (snapshot diff; pre-existing apps are never touched)")
		fmt.Printf("  protected prefixes: %s\n", strings.Join(cfg.ProtectedPackages, ", "))
		fmt.Println("  toggle with: adbharbor cleanup on | adbharbor cleanup off")
		return nil
	}
	switch args[0] {
	case "on", "enable":
		cfg.CleanupEnabled = true
	case "off", "disable":
		cfg.CleanupEnabled = false
	default:
		return fmt.Errorf("cleanup: use `on`, `off`, or no argument for status")
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	state := "disabled"
	if cfg.CleanupEnabled {
		state = "enabled"
	}
	fmt.Printf("session cleanup %s (a running daemon picks this up within seconds)\n", state)
	return nil
}

func CmdDoctor() error {
	cfg := LoadConfig()
	fmt.Println("adbharbor", Version)
	fmt.Println("  data dir:   ", Dir())

	real, err := ResolveRealADB(cfg)
	if err != nil {
		fmt.Println("  real adb:    MISSING —", err)
	} else {
		fmt.Println("  real adb:   ", real)
	}

	// What does `adb` resolve to on PATH right now?
	found := ""
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, "adb")
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			found = p
			break
		}
	}
	switch {
	case found == "":
		fmt.Println("  PATH adb:    none found")
	case isSelf(found):
		fmt.Println("  PATH adb:   ", found, "(harbor shim — good)")
	default:
		fmt.Println("  PATH adb:   ", found, "(NOT the shim — open a new shell or check PATH order)")
	}

	if err := EnsureDaemon(); err != nil {
		fmt.Println("  daemon:      NOT RUNNING —", err)
	} else {
		fmt.Println("  daemon:      running on", SocketPath())
	}

	if cfg.ProxyEnabled {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.ProxyPort), time.Second); err == nil {
			c.Close()
			fmt.Printf("  adb port:    %d brokered by harbor (real server on %d)\n", cfg.ProxyPort, cfg.AdbServerPort)
		} else {
			fmt.Printf("  adb port:    %d NOT listening — proxy down, clients bypass the broker\n", cfg.ProxyPort)
		}
	} else {
		fmt.Println("  adb port:    proxy disabled (only PATH-shim commands are brokered)")
	}

	fmt.Println("  session:    ", DetectSession(cfg))

	if real != "" {
		if devs, err := ListDevices(real, cfg.ClientServerPort()); err == nil {
			fmt.Printf("  devices:     %d connected\n", len(devs))
		}
	}

	// Warn if something re-resolved adb inside common build tools.
	if _, err := exec.LookPath("adb"); err == nil && found != "" && !isSelf(found) && !strings.Contains(found, BinDir()) {
		fmt.Println("\nnote: this shell resolves adb outside the harbor; `exec zsh` to reload PATH")
	}
	return nil
}
