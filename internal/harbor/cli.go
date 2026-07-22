package harbor

import (
	"errors"
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
	me := DetectSession(LoadConfig())
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERIAL\tSTATE\tMODEL\tLEASE\tQUEUE")
	sort.Slice(resp.Devices, func(i, j int) bool { return resp.Devices[i].Serial < resp.Devices[j].Serial })
	for _, d := range resp.Devices {
		lease := "free"
		if d.Cleaning {
			lease = "session cleanup"
		}
		if d.Lease != nil {
			lease = fmt.Sprintf("%s (%s%s)%s%s", d.Lease.Holder,
				time.Since(d.Lease.AcquiredAt).Round(time.Second), runningSuffix(d.Lease.Running),
				etaSuffix(d.Lease), youSuffix(d.Lease.Session, me))
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

// sharedRuntimes are process names that commonly host or wrap more than one
// agent. Keying an identity on one of them is the silent failure mode of
// auto-detection: every agent underneath collapses into a single session, so
// they share one lease and stop being isolated from each other.
var sharedRuntimes = []string{"node", "bun", "deno", "java", "gradle", "python", "python3"}

// sharedIdentityWarning flags an identity that is probably shared with other
// agents. It cannot be certain — one bun process may well host exactly one
// agent — so it reports what the key is pinned to and how to make it
// explicit, rather than claiming a fault.
func sharedIdentityWarning(session, source string) string {
	if source != "process tree" {
		return ""
	}
	name := session
	if i := strings.LastIndex(session, "-"); i > 0 {
		name = session[:i]
	}
	for _, r := range sharedRuntimes {
		if name == r {
			return fmt.Sprintf("note: keyed on a shared `%s` process — every agent under it\n"+
				"                shares one lease. Export ADB_HARBOR_SESSION to separate them.", name)
		}
	}
	return ""
}

// etaSuffix renders a lease's advertised finish time, if it advertised one.
func etaSuffix(l *LeaseInfo) string {
	if l == nil || l.ETA == nil {
		return ""
	}
	if d := ETADesc(*l.ETA, l.ETANote, time.Now()); d != "" {
		return " " + d
	}
	return ""
}

// splitLeadingArg peels off a leading positional argument so flags placed
// after it are still parsed. Go's flag package stops at the first
// positional, which would make the natural `eta 25m -note "..."` silently
// drop the note — an estimate updated without its reason is worse than one
// that errors.
func splitLeadingArg(args []string) (first string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// CmdETA sets how long the caller expects to keep the device it holds.
// Advisory: it changes nothing about when the lease ends, it only tells
// whoever is queued behind it what to expect.
func CmdETA(args []string) error {
	fs := flag.NewFlagSet("eta", flag.ContinueOnError)
	serial := fs.String("s", "", "device serial (default: whichever device you hold)")
	note := fs.String("note", "", "what you are doing, shown to whoever is waiting")
	clear := fs.Bool("clear", false, "withdraw the estimate")
	durArg, rest := splitLeadingArg(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if durArg == "" {
		durArg = fs.Arg(0)
	}
	req := ETAReq{Serial: *serial, Note: *note, Clear: *clear}
	if !*clear {
		if durArg == "" {
			return errors.New("usage: adbharbor eta DURATION [-note \"...\"] | adbharbor eta --clear")
		}
		d, err := time.ParseDuration(durArg)
		if err != nil {
			return fmt.Errorf("bad duration %q: %w", durArg, err)
		}
		if d < time.Second {
			// Estimates are carried in whole seconds; anything shorter would
			// truncate to zero, which the broker reads as "said nothing" and
			// would leave a previous estimate silently in force.
			return errors.New("estimate must be at least 1s; use --clear to withdraw one")
		}
		req.ETASec = int(d.Seconds())
	}
	if err := EnsureDaemon(); err != nil {
		return err
	}
	req.Session = DetectSession(LoadConfig())
	resp, err := SetETA(req)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	if *clear {
		fmt.Printf("withdrew the estimate on %s\n", resp.Serial)
		return nil
	}
	fmt.Printf("%s: telling waiters you expect to be done in %s\n",
		resp.Serial, time.Duration(req.ETASec)*time.Second)
	return nil
}

// youSuffix marks the rows belonging to the caller. Without it a session
// reading this output cannot tell its own lease from a stranger's, and an
// agent blocked behind what is actually its own lingering lease has no way
// to see that.
func youSuffix(session, me string) string {
	if me != "" && session == me {
		return " <- you"
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
	me := DetectSession(LoadConfig())
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
		if eta := etaSuffix(&l); eta != "" {
			fmt.Print(" ", strings.TrimSpace(eta))
		}
		fmt.Println(youSuffix(l.Session, me))
		for i, wt := range st.Queues[l.Serial] {
			fmt.Printf("    queue[%d]: %s (waiting %s)%s\n", i+1, wt.Holder,
				time.Since(wt.Enqueued).Round(time.Second), youSuffix(wt.Session, me))
		}
	}
	return nil
}

// CmdWhoami answers "which of these leases is mine?" — the question every
// other command left an agent guessing at.
func CmdWhoami() error {
	cfg := LoadConfig()
	session, source := DetectSessionSource(cfg)
	fmt.Printf("session  %s\n", session)
	fmt.Printf("source   %s\n", source)

	if err := EnsureDaemon(); err != nil {
		return err
	}
	st, err := FetchState()
	if err != nil {
		return err
	}
	held := []string{}
	for _, l := range st.Leases {
		if l.Session == session {
			held = append(held, fmt.Sprintf("%s (%s%s)%s", l.Serial,
				time.Since(l.AcquiredAt).Round(time.Second), runningSuffix(l.Running),
				etaSuffix(&l)))
		}
	}
	fmt.Printf("holding  %s\n", orDash(strings.Join(held, ", ")))

	waiting := []string{}
	for serial, q := range st.Queues {
		for i, wt := range q {
			if wt.Session == session {
				waiting = append(waiting, fmt.Sprintf("%s (position %d, %s)",
					serial, i+1, time.Since(wt.Enqueued).Round(time.Second)))
			}
		}
	}
	sort.Strings(waiting)
	fmt.Printf("waiting  %s\n", orDash(strings.Join(waiting, ", ")))
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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
	me := DetectSession(LoadConfig())
	for _, l := range st.Leases {
		if l.Serial == serial {
			holder := l.Holder
			if l.Session == me {
				holder = "you, " + holder
			}
			fmt.Printf("%s is held by %s (session %s) since %s, %d command(s) running%s\n",
				serial, holder, l.Session, l.AcquiredAt.Format(time.Kitchen), l.Running, etaSuffix(&l))
			for i, wt := range st.Queues[serial] {
				fmt.Printf("  queue[%d]: %s%s\n", i+1, wt.Holder, youSuffix(wt.Session, me))
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
	eta := fs.Duration("eta", 0, "how long you expect to need it — advisory, shown to waiters (min 1s)")
	note := fs.String("note", "", "what you are doing, shown to whoever is waiting")
	sessionFlag := fs.String("session", "", "session key (default: auto-detected)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *any {
		return cmdAcquireAny(*usb, *emulator, *ttl, *eta, *note, *sessionFlag)
	}
	if *serial == "" {
		return fmt.Errorf("acquire: -s SERIAL or --any is required")
	}
	if *eta != 0 && *eta < time.Second {
		return errors.New("acquire: --eta must be at least 1s")
	}
	if err := EnsureDaemon(); err != nil {
		return err
	}
	cfg := LoadConfig()
	session, owner := *sessionFlag, 0
	if session == "" {
		session, owner, _ = DetectSessionOwner(cfg)
	}
	req := AcquireReq{
		Serial:   *serial,
		Session:  session,
		Holder:   HolderDesc(session),
		PID:      os.Getpid(),
		OwnerPID: owner,
		TTLSec:   int(ttl.Seconds()),
		ETASec:   int(eta.Seconds()),
		ETANote:  *note,
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
func cmdAcquireAny(usb, emulator bool, ttl, eta time.Duration, note, sessionFlag string) error {
	if err := EnsureDaemon(); err != nil {
		return err
	}
	cfg := LoadConfig()
	session, owner := sessionFlag, 0
	if session == "" {
		session, owner, _ = DetectSessionOwner(cfg)
	}
	resp, err := AcquireAny(AcquireAnyReq{
		Session:  session,
		Holder:   HolderDesc(session),
		PID:      os.Getpid(),
		OwnerPID: owner,
		TTLSec:   int(ttl.Seconds()),
		ETASec:   int(eta.Seconds()),
		ETANote:  note,
		USB:      usb,
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

	session, source := DetectSessionSource(cfg)
	fmt.Printf("  session:     %s (%s)\n", session, source)
	if warn := sharedIdentityWarning(session, source); warn != "" {
		fmt.Println("               ", warn)
	}

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
