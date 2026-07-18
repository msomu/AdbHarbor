# AdbHarbor

**A lock broker for shared Android devices.** AdbHarbor gives coding agents, scripts, and CI jobs exclusive, queue-based access to the phones connected to your machine — so two agents never trample each other's ADB sessions.

## The problem

Run multiple coding agents (Claude Code, Codex, Gemini, …) on one machine with a couple of phones attached, and they will eventually target the same device at the same time. One agent force-stops or uninstalls the app another agent just installed and launched; both burn tokens diagnosing "mysterious" device state. `adb -s SERIAL` selects a device but provides no ownership, blocking, or queueing — nothing stops a second client from talking to the same phone through the shared ADB server.

## How it works

AdbHarbor installs a transparent `adb` shim ahead of the real adb on your `PATH`. **No agent, script, or tool needs to change anything** — they keep running plain `adb` commands:

1. The shim parses the command. Device-targeted commands (`shell`, `install`, `logcat`, …) must hold a **lease** on that serial; deviceless ones (`devices`, `version`, `connect`, …) pass straight through.
2. A tiny broker daemon (auto-started on first use, Unix socket, zero config) grants one lease per device per **session**. A session is detected automatically by walking up the process tree to the owning agent process — every command from the same Claude/Codex/terminal session shares one lease.
3. If another session holds the device, the command **waits in a FIFO queue**, printing who holds it. Same-session commands run concurrently and never block each other.
4. After a session's last command, the lease lingers briefly (default 60s) so an agent keeps ownership across consecutive commands, then the next queued session gets the device.
5. Crash-safe by construction: leases have heartbeats; killed clients, orphaned commands, and never-claimed grants are all reclaimed automatically. Any broker failure **fails open** to plain adb — the harbor can never brick your adb.

```
agent A ──adb──▶ shim ──lease ok──▶ real adb ──▶ Pixel
agent B ──adb──▶ shim ──queued… (held by A)…granted──▶ real adb ──▶ Pixel
agent C ──adb──▶ shim ──lease ok──▶ real adb ──▶ other phone   (parallel)
```

## Install

Requires Go 1.22+ and the Android platform-tools.

```bash
go install github.com/msomu/AdbHarbor/cmd/adbharbor@latest
adbharbor install
```

Or from a clone: `go build -o adbharbor ./cmd/adbharbor && ./adbharbor install`

`install` copies the binary to `~/.adbharbor/bin`, symlinks `adb` to it, pins the real adb path in `~/.adbharbor/config.json`, and prepends `~/.adbharbor/bin` to `PATH` in your shell rc. Open a new shell and verify with `adbharbor doctor`.

## CLI

```bash
adbharbor devices              # devices + who holds them + queue depth
adbharbor status               # all leases and queues
adbharbor who -s SERIAL        # who holds one device
adbharbor acquire -s SERIAL --ttl 30m   # hold a device explicitly
adbharbor release -s SERIAL [--force]
adbharbor doctor               # check shim, real adb, daemon, session detection
adbharbor stop | daemon | uninstall
```

## Behavior reference

| Situation | What happens |
|---|---|
| Device free | Lease granted instantly, command runs |
| Device held by your own session | Runs immediately (concurrent commands OK) |
| Device held by another session | Waits in FIFO queue, progress on stderr |
| Wait exceeds `ADB_HARBOR_WAIT` (600s) | Exits **75** (busy — retry later, not a device error) |
| Holder crashes / is killed | Lease reclaimed automatically (heartbeat + claim tracking) |
| Broker unreachable / broken | Warns and runs unlocked (fail-open) |
| Command can't be tied to one device | Passes through untouched |

Environment overrides: `ADB_HARBOR_SESSION` (explicit session key), `ADB_HARBOR_IDLE` (lease linger seconds), `ADB_HARBOR_WAIT` (max queue wait seconds), `ADB_HARBOR_ADB` (real adb path), `ADB_HARBOR_OFF=1` (bypass locking).

Config lives in `~/.adbharbor/config.json` (idle TTL, wait timeout, agent process names for session detection). Lease events append to `~/.adbharbor/history.jsonl`; daemon logs to `~/.adbharbor/daemon.log`.

## Agent integration

Agents need zero changes to *work* — the shim is transparent. To make them *smart* about it (interpret exit 75, pick a free device instead of waiting, never force-release a live session), install the included skill:

- **Claude Code**: copy [`skills/claude/adbharbor/`](skills/claude/adbharbor/) to `~/.claude/skills/adbharbor/`.
- **Other agents** (Codex, etc.): paste [`examples/AGENTS-snippet.md`](examples/AGENTS-snippet.md) into your global agent instructions.

## Limitations

- Tools that invoke adb by **absolute path** (or their own bundled copy, e.g. Android Studio / Gradle's ddmlib installs) bypass the shim. Point them at plain `adb` where possible.
- Single host only in v1 — the broker and phones live on the same machine (matching ADB's host-local server). See [docs/PRD.md](docs/PRD.md) for the HTTP/remote roadmap.
- Cleanup policies (auto-uninstall of session-installed apps) are specced in the PRD but not yet implemented; lease events are already recorded in the history log.

## License

MIT
