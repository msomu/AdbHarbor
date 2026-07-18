# AdbHarbor

**A lock broker for shared Android devices.** AdbHarbor gives coding agents, scripts, and CI jobs exclusive, queue-based access to the phones connected to your machine — so two agents never trample each other's ADB sessions.

## The problem

Run multiple coding agents (Claude Code, Codex, Pi, …) on one machine with one or two phones attached, and they will eventually target the same device at the same time. One agent force-stops or uninstalls the app another agent just installed and launched. `adb -s SERIAL` selects a device, but it doesn't provide ownership, blocking, or queueing — nothing stops a second client from talking to the same phone through the shared ADB server.

## What AdbHarbor does

- **Exclusive leases per device.** An agent acquires a lease on a serial (or any free device matching constraints) before touching it. Everyone else waits in a FIFO queue.
- **Sessions, not commands.** Install → launch → test → release is one atomic session. All ADB commands in a session are automatically pinned to the leased serial.
- **Deterministic cleanup.** Packages installed during a session are tracked and can be force-stopped and uninstalled on release, leaving the device clean for the next job.
- **Crash safety.** Leases have TTLs and heartbeats. If an agent dies, the broker expires the lease, runs cleanup, and wakes the next request in the queue.
- **Visibility.** `who-has-device`, per-device queues, session history, and structured logs.

## CLI sketch

```bash
adb-lock devices
adb-lock acquire --serial SERIAL_1 --project app-one --agent claude-a --cleanup uninstall-installed-apps
adb-lock run --lease LEASE_ID -- adb shell pm list packages
adb-lock install --lease LEASE_ID app-debug.apk --package com.example.app
adb-lock release --lease LEASE_ID
adb-lock who-has-device --serial SERIAL_1
```

## Status

Early — the [PRD](docs/PRD.md) is written, implementation is starting. v1 targets a single host: local broker process, CLI, SQLite-backed state, serial-based locking, install tracking, and auto-uninstall on release. HTTP API, heartbeats, and multi-device scheduling follow.

One contract drives the design: **no ADB access without a lease, and every lease ends with deterministic cleanup.**

## License

MIT
