package harbor

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Session cleanup: when enabled, the broker snapshots the device's package
// list as a lease is granted and diffs it when the lease ends. Packages
// that appeared during the session are uninstalled before the device is
// handed to the next waiter. The snapshot-diff approach catches every
// install mechanism (pm install, streamed installs, abb_exec, Maestro) and
// by construction can never remove an app that predates the session.

// listPackages returns the package names installed on a device.
func listPackages(realADB string, serverPort int, serial string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, realADB, "-s", serial, "shell", "pm", "list", "packages")
	if serverPort > 0 {
		cmd.Env = envWithServerPort(os.Environ(), serverPort)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pm list packages: %w", err)
	}
	var pkgs []string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimPrefix(strings.TrimSpace(line), "package:"); p != "" && p != strings.TrimSpace(line) {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, nil
}

// newPackagesToRemove diffs current against the session baseline and drops
// protected prefixes. Pure function, unit-tested.
func newPackagesToRemove(current, baseline, protectedPrefixes []string) []string {
	base := make(map[string]bool, len(baseline))
	for _, p := range baseline {
		base[p] = true
	}
	var out []string
	for _, p := range current {
		if base[p] || isProtectedPackage(p, protectedPrefixes) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func isProtectedPackage(pkg string, prefixes []string) bool {
	for _, pre := range prefixes {
		if strings.HasPrefix(pkg, pre) {
			return true
		}
	}
	return false
}

// captureBaseline records the package list on a freshly granted lease.
// Runs async so grants stay fast; a lease that ends before the snapshot
// lands simply skips cleanup (no baseline = never uninstall anything).
func (b *Broker) captureBaseline(leaseID, serial string) {
	pkgs, err := listPackages(b.config().RealADB, b.config().ClientServerPort(), serial)
	if err != nil {
		log.Printf("cleanup: baseline for %s failed: %v", serial, err)
		return
	}
	b.mu.Lock()
	if l := b.leaseByIDLocked(leaseID); l != nil {
		l.Baseline = pkgs
		b.saveStateLocked()
	}
	b.mu.Unlock()
}

// runCleanup uninstalls session-installed packages, then hands the device
// to the next waiter. Called in its own goroutine; b.cleaning[serial] is
// already set and queues new arrivals meanwhile.
func (b *Broker) runCleanup(l *Lease) {
	removed, failed := []string{}, []string{}
	current, err := listPackages(b.config().RealADB, b.config().ClientServerPort(), l.Serial)
	if err != nil {
		log.Printf("cleanup: %s: %v (skipping)", l.Serial, err)
	} else {
		for _, pkg := range newPackagesToRemove(current, l.Baseline, b.config().ProtectedPackages) {
			if uninstallPackage(b.config().RealADB, b.config().ClientServerPort(), l.Serial, pkg) {
				removed = append(removed, pkg)
				log.Printf("cleanup: uninstalled %s from %s (installed by %s)", pkg, l.Serial, l.Holder)
			} else {
				failed = append(failed, pkg)
				log.Printf("cleanup: FAILED to uninstall %s from %s", pkg, l.Serial)
			}
		}
	}
	if len(removed) > 0 || len(failed) > 0 {
		note := "removed " + strings.Join(removed, ",")
		if len(failed) > 0 {
			note += " failed " + strings.Join(failed, ",")
		}
		b.hist("cleanup", l, note)
	}
	b.mu.Lock()
	delete(b.cleaning, l.Serial)
	b.grantNextLocked(l.Serial, time.Now())
	b.saveStateLocked()
	b.mu.Unlock()
}

func uninstallPackage(realADB string, serverPort int, serial, pkg string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, realADB, "-s", serial, "uninstall", pkg)
	if serverPort > 0 {
		cmd.Env = envWithServerPort(os.Environ(), serverPort)
	}
	out, err := cmd.CombinedOutput()
	return err == nil && strings.Contains(string(out), "Success")
}
