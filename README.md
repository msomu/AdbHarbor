# AdbHarbor

**A lock broker for shared Android devices.** AdbHarbor gives coding agents, scripts, and CI jobs exclusive, queue-based access to the phones connected to your machine — so two agents never trample each other's ADB sessions.

## The problem

Run multiple coding agents (Claude Code, Codex, Gemini, …) on one machine with a couple of phones attached, and they will eventually target the same device at the same time. One agent force-stops or uninstalls the app another agent just installed and launched; both burn tokens diagnosing "mysterious" device state. `adb -s SERIAL` selects a device but provides no ownership, blocking, or queueing — nothing stops a second client from talking to the same phone through the shared ADB server.

## How it works

AdbHarbor brokers at **two layers**, so no agent, script, or tool needs to change anything:

**1. The ADB server port (universal).** The harbor daemon takes over TCP port **5037** — the port every ADB client on earth defaults to — and parks the real adb server on 5038. The daemon speaks the ADB smart-socket protocol: it relays the transport handshake, sees which device a connection targets, identifies the *calling process* from the TCP peer (walking its process tree to the owning agent), and gates device-mutating services on a lease before splicing bytes through. This catches **everything**: adb CLIs at any path, Maestro/dadb, ddmlib/Android Studio, custom scripts. Tools stay 100% harbor-agnostic — on machines without AdbHarbor they hit a normal adb server; here they hit the broker without knowing it.

**2. The PATH shim (better UX).** An `adb` shim ahead of the real one adds what the silent port layer can't: "waiting for device (held by X)" progress on stderr, exit code 75 on wait timeout, and per-command env overrides (`ADB_HARBOR_SESSION`, `ADB_HARBOR_IDLE`, `ADB_HARBOR_WAIT`). Shim-locked commands talk straight to the real server — their lease is already held.

Shared semantics at both layers:

- One lease per device per **session** (auto-detected: every command from the same Claude/Codex/terminal/daemon process tree shares one lease). Same-session commands run concurrently.
- Other sessions **wait in a FIFO queue**; after a session's last command the lease lingers (default 30s) so an agent keeps ownership across consecutive commands and think-time gaps.
- **Read-only commands are lease-exempt** (`getprop`, `dumpsys`, `pm list`, `settings get`, …): device-inventory heartbeats from tools like DroidRunner never squat a device and never stall behind a busy one.
- Crash-safe: heartbeats, orphan detection, and unclaimed-grant reclaim clean up after killed clients automatically. Any broker failure **fails open** to plain adb.

```
agent A ──adb────────▶ shim ─lease─▶ real adb server:5038 ──▶ Pixel
agent B ──adb────────▶ shim ─queued…granted─▶ :5038 ────────▶ Pixel
CI job ──Maestro/dadb──▶ :5037 harbor proxy ─lease─▶ :5038 ─▶ other phone
studio ──ddmlib────────▶ :5037 harbor proxy ─(exempt/lease)─▶ :5038
```

## Install

Requires the Android platform-tools.

```bash
brew install msomu/tap/adbharbor
adbharbor install
```

Or with Go 1.22+: `go install github.com/msomu/AdbHarbor/cmd/adbharbor@latest && adbharbor install`

Or from a clone: `go build -o adbharbor ./cmd/adbharbor && ./adbharbor install`

`install` copies the binary to `~/.adbharbor/bin`, symlinks `adb` to it, pins the real adb path in `~/.adbharbor/config.json`, and prepends `~/.adbharbor/bin` to `PATH` in your shell rc. Open a new shell and verify with `adbharbor doctor`.

## CLI

```bash
adbharbor devices              # devices + who holds them + queue depth
adbharbor status               # all leases and queues
adbharbor who -s SERIAL        # who holds one device
adbharbor whoami               # your session key, your leases, your queue spots
adbharbor eta 10m -note "..."  # tell waiters when you expect to be done
adbharbor eta --clear          # withdraw the estimate
adbharbor acquire -s SERIAL --ttl 30m   # hold a device explicitly
adbharbor acquire --any [--usb|--emulator] --ttl 20m
                               # lease ANY free device — prints its serial on
                               # stdout (exit 75 if all busy); atomic, so two
                               # agents asking simultaneously get different devices
adbharbor release -s SERIAL [--force]
adbharbor cleanup [on|off]     # uninstall-on-release of session apps (default: off)
adbharbor doctor               # check shim, real adb, daemon, session detection
adbharbor stop | daemon | uninstall
```

## Session cleanup (opt-in)

`adbharbor cleanup on` makes every lease end with a cleanup pass: the broker snapshots the device's package list when a session's lease is granted and diffs it when the lease ends — anything that appeared during the session is uninstalled before the device goes to the next waiter (shown as `session cleanup` in `adbharbor devices`).

The snapshot-diff design means it catches every install mechanism (`adb install`, streamed installs, `pm install`, Maestro-driven installs) without parsing anything, and **can never remove an app that predates the session** — pre-existing apps that get *upgraded* during a session also survive. Package prefixes in `protected_package_prefixes` (`android`, `com.android.`, `com.google.`, …) are never uninstalled regardless. Cleanup results land in `~/.adbharbor/history.jsonl`; failures are logged and never block the device handoff. A running daemon picks up the toggle within seconds — no restart needed.

## Behavior reference

| Situation | What happens |
|---|---|
| Device free | Lease granted instantly, command runs |
| Device held by your own session | Runs immediately (concurrent commands OK) |
| Device held by another session | Waits in FIFO queue (shim: progress on stderr; port layer: silent wait) |
| Read-only command (`getprop`, `dumpsys`, `pm list`, …) | Runs immediately, no lease needed |
| Wait exceeds `ADB_HARBOR_WAIT` / config (600s) | Shim exits **75**; port layer returns an adb FAIL naming the holder |
| Holder crashes / is killed | Lease reclaimed automatically (heartbeat + claim tracking) |
| `adb kill-server` | Real server dies and is restarted on demand |
| Broker unreachable / broken | Warns and runs unlocked (fail-open); clients spawn a classic server |
| Command can't be tied to one device | Passes through untouched |

Environment overrides: `ADB_HARBOR_SESSION` (explicit session key), `ADB_HARBOR_ETA` / `ADB_HARBOR_NOTE` (advertise an expected finish time on an ordinary adb command), `ADB_HARBOR_IDLE` (lease linger seconds), `ADB_HARBOR_WAIT` (max queue wait seconds), `ADB_HARBOR_ADB` (real adb path), `ADB_HARBOR_OFF=1` (bypass locking).

Config lives in `~/.adbharbor/config.json` (idle TTL, wait timeout, agent process names for session detection). Lease events append to `~/.adbharbor/history.jsonl`; daemon logs to `~/.adbharbor/daemon.log`.

## Agent integration

Agents need zero changes to *work* — the shim is transparent. To make them *smart* about it (interpret exit 75, pick a free device instead of waiting, never force-release a live session), install the included skill:

- **Claude Code**: copy [`skills/claude/adbharbor/`](skills/claude/adbharbor/) to `~/.claude/skills/adbharbor/`.
- **Other agents** (Codex, etc.): paste [`examples/AGENTS-snippet.md`](examples/AGENTS-snippet.md) into your global agent instructions.

## Limitations

- Clients that pin `ANDROID_ADB_SERVER_PORT` to a non-default port connect around the proxy.
- A client that chains a plain `host:*` query and a transport on one connection is relayed raw after the first query (rare; standard adb CLI and dadb open fresh connections).
- Single host only — the broker and phones live on the same machine (matching ADB's host-local server). See [docs/PRD.md](docs/PRD.md) for the HTTP/remote roadmap.

## License

MIT
