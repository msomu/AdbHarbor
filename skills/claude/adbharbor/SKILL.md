---
name: adbharbor
description: Use when running adb commands, Android device automation, or app install/launch/test loops on this machine - adb is wrapped by AdbHarbor, which serializes device access between concurrent agents; explains waiting behavior, exit code 75, how to tell your own lease from another agent's, and how to pick a free device or run in parallel.
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
  When the holder has published an estimate, that line says how long:
  `busy (held by agent-6 for 2m, free in ~6m (maestro smoke))`.
- **Exit code 75 means "device busy, gave up waiting"** — NOT an app or
  device failure. Do not debug your app, do not kill other apps, do not
  retry in a tight loop. Either wait and retry once later, or switch to a
  free device.
- All commands from YOUR session share one lease: your install → launch →
  test sequence cannot be interleaved by another agent. The lease lingers
  ~30 seconds after your last adb command, then the device passes to the
  next session in the queue.
- Automation daemons (e.g. DroidRunner CI jobs) are brokered too: while a
  job runs its device shows as held (session like `bun-...`) and your
  commands queue behind it — this is normal, wait or pick another device.

## Commands

```bash
adbharbor devices          # serials + FREE/held-by-whom + queue depth
adbharbor whoami           # YOUR session key, your leases, your queue spots
adbharbor who -s SERIAL    # who holds a device right now
adbharbor status           # all leases and queues
adbharbor eta 10m -note "maestro smoke"   # tell waiters when you'll be done
adbharbor eta --clear                     # withdraw that estimate
adbharbor acquire -s SERIAL --ttl 30m     # reserve a device for a long task
adbharbor release -s SERIAL               # release your own lease early
```

`devices`, `status` and `who` mark your own rows `<- you`. If you are ever
unsure whether a device is held by you or by someone else, run
`adbharbor whoami` — do not guess from the holder string.

## Rules

1. **Prefer a free device — atomically.** Before a long task, run
   `S=$(adbharbor acquire --any --ttl 20m)` — the broker picks a free
   device, leases it to you, and prints its serial (stdout only). Pin every
   command with `adb -s $S …` and `adbharbor release -s $S` when done.
   Exit 75 = the whole fleet is busy. It's sticky: asking again returns
   the device you already hold. (Manual alternative: `adbharbor devices`,
   pick a `free` serial — but two agents can race; `--any` cannot.)
2. **Never `adbharbor release --force`** a device that another session
   holds unless its holder is provably dead — force-release yanks the
   device mid-command from the other agent. You almost never need to: if
   the owning agent's process is gone the broker reclaims the device
   within a couple of seconds, and a holder killed mid-command is cleaned
   up within about a minute.
3. **Say how long you'll be** when you take a device for more than a
   moment: `adbharbor eta 10m -note "install + maestro flow"`. It is
   advisory — nothing expires because of it, and overrunning only shows as
   `OVERDUE` — but it is the only thing a blocked agent can use to decide
   between waiting and switching devices. Revise it with another
   `adbharbor eta` rather than letting it lapse.
4. **When blocked, read the estimate before deciding.** `free in ~2m` is
   worth waiting for; `free in ~40m`, `OVERDUE by 20m`, or no estimate at
   all is a reason to `acquire --any` a different device instead of sitting
   in the queue.
5. For an exclusive multi-minute run (e.g. instrumented test suite), take
   `adbharbor acquire -s SERIAL --ttl 30m` first and `release` when done —
   this prevents your device from rotating away during quiet gaps longer
   than the idle linger.
6. If adb prints `running unlocked` warnings, the broker is down —
   locking is bypassed (fail-open). `adbharbor doctor` diagnoses.
7. If `adbharbor cleanup` reports ENABLED, apps you install are
   auto-uninstalled when your session's lease ends — reinstall on the next
   session instead of assuming state persists, and finish install→test
   sequences without long idle gaps.

## When your identity is wrong

Your session key is normally inferred from your process tree
(`claude-97333`), and every command you run resolves to the same key. Two
cases break that, and `adbharbor doctor` warns about both:

- **Keyed on a shared runtime** (`bun-49286`, `java-…`, `gradle-…`): several
  agents share one process, so they share one identity and one lease and
  can take the device from each other.
- **Keyed on `launchd`**, or a different key for every command: your shell
  exited before the command was identified, so nothing links your commands
  to each other and you can end up queued behind your own lingering lease.

The fix for both is to name yourself explicitly:

```bash
export ADB_HARBOR_SESSION=my-agent-name
```

Set it once for the whole run, not per command — two spellings split you in
two. Note that an explicit key names no process, so the broker cannot tell
whether you are still alive; leases you take are released by their TTL or
idle linger rather than reclaimed the moment you exit. Prefer `--ttl` values
that match the work.
