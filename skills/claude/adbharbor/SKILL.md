---
name: adbharbor
description: Use when running adb commands, Android device automation, or app install/launch/test loops on this machine - adb is wrapped by AdbHarbor, which serializes device access between concurrent agents; explains waiting behavior, exit code 75, and how to pick a free device or run in parallel.
---

# AdbHarbor: adb is brokered on this machine

ALL device access on this machine goes through AdbHarbor, a lock broker
that gives each agent session exclusive access to one device at a time.
The harbor owns the ADB server port (5037), so every client is brokered —
`adb` at any path, Maestro, Android Studio, CI runners — not just shell
commands. You do not need to do anything special — run `adb` normally.

Read-only commands (`getprop`, `dumpsys`, `pm list`, `settings get`, ...)
are lease-exempt: they always run instantly, even on a busy device. Only
device-mutating commands (install, am/input, push, logcat, shell scripts)
take the lease.

## What you'll observe

- **Your commands never fight another agent.** If another session holds the
  device, your command prints `adbharbor: device X is busy (held by …)` to
  stderr and waits in a FIFO queue (default up to 10 minutes), then runs.
- **Exit code 75 means "device busy, gave up waiting"** — NOT an app or
  device failure. Do not debug your app, do not kill other apps, do not
  retry in a tight loop. Either wait and retry once later, or switch to a
  free device.
- All commands from YOUR session share one lease: your install → launch →
  test sequence cannot be interleaved by another agent. The lease lingers
  ~60s after your last adb command, then the device passes to the next
  session in the queue.
- Automation daemons (e.g. DroidRunner CI jobs) are brokered too: while a
  job runs its device shows as held (session like `bun-...`) and your
  commands queue behind it — this is normal, wait or pick another device.

## Commands

```bash
adbharbor devices          # serials + FREE/held-by-whom + queue depth
adbharbor who -s SERIAL    # who holds a device right now
adbharbor status           # all leases and queues
adbharbor acquire -s SERIAL --ttl 30m   # reserve a device for a long task
adbharbor release -s SERIAL             # release your own lease early
```

## Rules

1. **Prefer a free device.** Before a long task on a shared machine, run
   `adbharbor devices` and pick a serial that shows `free` (respecting any
   device policy in your instructions). Pin it with `adb -s SERIAL …`.
2. **Never `adbharbor release --force`** a device that another session
   holds unless its holder is provably dead — force-release yanks the
   device mid-command from the other agent. Crashed holders are reclaimed
   automatically within ~1–2 minutes; you rarely need to intervene.
3. For an exclusive multi-minute run (e.g. instrumented test suite), take
   `adbharbor acquire -s SERIAL --ttl 30m` first and `release` when done —
   this prevents your device from rotating away during quiet gaps longer
   than the idle linger.
4. If adb prints `running unlocked` warnings, the broker is down —
   locking is bypassed (fail-open). `adbharbor doctor` diagnoses.
5. If `adbharbor cleanup` reports ENABLED, apps you install are
   auto-uninstalled when your session's lease ends — reinstall on the next
   session instead of assuming state persists, and finish install→test
   sequences without long idle gaps.
