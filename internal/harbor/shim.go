package harbor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const heartbeatEvery = 15 * time.Second

// RunShim is the transparent adb wrapper. Device-targeted commands acquire
// a lease first (waiting in the queue if the device is held by another
// session); everything else passes straight through to the real adb.
//
// Fail-open is a hard rule: any harbor-internal problem degrades to plain
// adb so a broken broker can never brick adb on this machine. The only case
// that blocks is a device legitimately held by another session.
func RunShim(args []string) int {
	cfg := LoadConfig()
	real, err := ResolveRealADB(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "adbharbor:", err)
		return 1
	}

	// With the proxy owning the default server port, shim-managed commands
	// talk straight to the real server: their locking happens here via the
	// broker, so a second pass through the proxy would be redundant.
	execEnv := os.Environ()
	if p := cfg.ClientServerPort(); p > 0 && os.Getenv("ANDROID_ADB_SERVER_PORT") == "" {
		execEnv = envWithServerPort(execEnv, p)
	}

	if os.Getenv("ADB_HARBOR_OFF") == "1" {
		return execReal(real, args, execEnv)
	}

	inv := ParseInvocation(args)
	if !inv.NeedsDevice() || inv.IsExemptReadOnly(cfg.ExemptShell) {
		return execReal(real, args, execEnv)
	}

	serial := inv.Serial
	if serial == "" {
		serial = os.Getenv("ANDROID_SERIAL")
	}
	if serial == "" {
		serial = resolveSoleDevice(real, cfg, inv)
	}
	if serial == "" {
		// Can't tell which device this targets (0 or 2+ candidates);
		// let the real adb produce its usual error / behavior.
		return execReal(real, args, execEnv)
	}

	if err := EnsureDaemon(); err != nil {
		fmt.Fprintf(os.Stderr, "adbharbor: broker unavailable (%v); running unlocked\n", err)
		return execReal(real, args, execEnv)
	}

	session, observer := DetectSessionObserver(cfg)
	if observer {
		// scrcpy-style observer tools stream through adb but must not own
		// the device.
		return execReal(real, args, execEnv)
	}
	// Hand the resolved identity down: the environment is inherited, so
	// anything the real adb spawns — and anything still running after this
	// shim exits — keeps reporting the same session to the broker instead
	// of being classified afresh from a process tree we may no longer be in.
	execEnv = envWith(execEnv, "ADB_HARBOR_SESSION", session)
	req := AcquireReq{
		Serial:     serial,
		Session:    session,
		Holder:     HolderDesc(session),
		PID:        os.Getpid(),
		IdleTTLSec: envInt("ADB_HARBOR_IDLE", 0),
		ETASec:     envSeconds("ADB_HARBOR_ETA"),
		ETANote:    os.Getenv("ADB_HARBOR_NOTE"),
		Command:    true,
	}
	waitSec := envInt("ADB_HARBOR_WAIT", cfg.WaitSec)
	leaseID, err := AcquireBlocking(req, waitSec, func(msg string) {
		fmt.Fprintln(os.Stderr, "adbharbor:", msg)
	})
	if err != nil {
		if errors.Is(err, ErrWaitTimeout) {
			fmt.Fprintf(os.Stderr,
				"adbharbor: gave up after %ds waiting for device %s.\n"+
					"adbharbor: check `adbharbor who -s %s`; retry later, use another device, or force-release if the holder is dead.\n",
				waitSec, serial, serial)
			return 75 // EX_TEMPFAIL: device busy, retry later
		}
		fmt.Fprintf(os.Stderr, "adbharbor: lease error (%v); running unlocked\n", err)
		return execReal(real, args, execEnv)
	}

	nudgeIfOverdue(session, serial)
	code := runLeasedCommand(real, args, leaseID, execEnv)
	EndCommand(leaseID)
	return code
}

// nudgeIfOverdue tells a holder that the estimate it published has passed.
// An estimate nobody refreshes is worse than none — a waiter reading it
// makes decisions on a number that stopped being true — and the holder's
// next command is the one moment it is certain to be listening.
func nudgeIfOverdue(session, serial string) {
	st, err := FetchState()
	if err != nil {
		return
	}
	for _, l := range st.Leases {
		if l.Session != session || l.Serial != serial || l.ETA == nil {
			continue
		}
		if over := time.Since(*l.ETA); over > 0 {
			fmt.Fprintf(os.Stderr,
				"adbharbor: you told waiters %s would be free %s ago; update with `adbharbor eta 5m`\n",
				serial, over.Round(time.Second))
		}
		return
	}
}

// runLeasedCommand runs the real adb as a child (so we can report command
// completion afterwards), heartbeating the lease while it runs and
// forwarding signals.
func runLeasedCommand(real string, args []string, leaseID string, env []string) int {
	cmd := exec.Command(real, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "adbharbor:", err)
		return 127
	}

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				Heartbeat(leaseID)
			case <-stop:
				return
			}
		}
	}()

	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for s := range sigc {
			if cmd.Process != nil {
				cmd.Process.Signal(s)
			}
		}
	}()

	err := cmd.Wait()
	close(stop)
	signal.Stop(sigc)
	close(sigc)

	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "adbharbor:", err)
	return 127
}

// execReal replaces this process with the real adb (pure passthrough).
func execReal(real string, args []string, env []string) int {
	argv := append([]string{real}, args...)
	err := syscall.Exec(real, argv, env)
	fmt.Fprintln(os.Stderr, "adbharbor: exec real adb:", err)
	return 127
}

// resolveSoleDevice returns the serial iff exactly one connected device
// matches the invocation (so we know what to lock without -s).
func resolveSoleDevice(real string, cfg *Config, inv ADBInvocation) string {
	devs, err := ListDevices(real, cfg.ClientServerPort())
	if err != nil {
		return ""
	}
	var candidates []Device
	for _, d := range devs {
		if d.State != "device" {
			continue
		}
		if inv.UseUSB && !d.USB {
			continue
		}
		if inv.UseEmulator && d.USB {
			continue
		}
		if inv.TransportID != "" && d.TransportID != inv.TransportID {
			continue
		}
		candidates = append(candidates, d)
	}
	if len(candidates) == 1 {
		return candidates[0].Serial
	}
	return ""
}

// ResolveRealADB finds the wrapped adb binary: config first, then PATH scan
// skipping ourselves.
func ResolveRealADB(cfg *Config) (string, error) {
	if cfg.RealADB != "" {
		if isSelf(cfg.RealADB) {
			return "", errors.New("config real_adb points back at the adbharbor shim; fix " + ConfigPath())
		}
		if _, err := os.Stat(cfg.RealADB); err == nil {
			return cfg.RealADB, nil
		}
	}
	if p, err := DiscoverRealADB(); err == nil {
		return p, nil
	}
	return "", errors.New("real adb not found; run `adbharbor install` or set ADB_HARBOR_ADB")
}

// DiscoverRealADB scans PATH (skipping the harbor bin dir and any adbharbor
// binary) and common SDK locations for the genuine adb.
func DiscoverRealADB() (string, error) {
	seen := map[string]bool{}
	var candidates []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || dir == BinDir() {
			continue
		}
		candidates = append(candidates, filepath.Join(dir, "adb"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Library", "Android", "sdk", "platform-tools", "adb"))
	}
	if sdk := os.Getenv("ANDROID_HOME"); sdk != "" {
		candidates = append(candidates, filepath.Join(sdk, "platform-tools", "adb"))
	}
	if sdk := os.Getenv("ANDROID_SDK_ROOT"); sdk != "" {
		candidates = append(candidates, filepath.Join(sdk, "platform-tools", "adb"))
	}
	for _, p := range candidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if isSelf(p) {
			continue
		}
		return p, nil
	}
	return "", errors.New("no adb found on PATH or in the Android SDK")
}

// isSelf reports whether path resolves to an adbharbor binary (the shim
// itself or the installed copy), guarding against recursive wrapping.
func isSelf(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	if filepath.Base(resolved) == "adbharbor" {
		return true
	}
	if self, err := selfPath(); err == nil && resolved == self {
		return true
	}
	dir, err := filepath.EvalSymlinks(Dir())
	if err == nil {
		if rel, err := filepath.Rel(dir, resolved); err == nil && !filepath.IsAbs(rel) && rel != ".." && !hasDotDotPrefix(rel) {
			return true
		}
	}
	return false
}

func hasDotDotPrefix(rel string) bool {
	return rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

// envSeconds reads a duration like "8m" from the environment, in seconds.
func envSeconds(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return int(d.Seconds())
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}
