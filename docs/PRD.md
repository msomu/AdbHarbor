# AdbHarbor — Product Requirements

**Status:** Draft · **Target:** v1 (single-host MVP)

## Overview

AdbHarbor provides exclusive, queue-based access to shared Android devices for coding agents, local scripts, and CI jobs. It solves the collision problem where two or more clients issue ADB commands to the same phone at the same time — force-stopping, reinstalling, or uninstalling each other's app sessions.

ADB uses a client–server architecture on the host: a single ADB server (port 5037) mediates between every local client and the device-side daemon. Any process on the machine can talk to any connected device, so device access needs a coordination layer that ADB itself does not provide.

AdbHarbor is that layer: a lock broker that grants one client an exclusive lease on a device, queues competing requests until the device is free, and guarantees cleanup when the lease ends — including uninstalling apps that were installed during the session.

## Problem

When multiple agents (Claude Code, Codex, and others) work on different Android projects on one machine, they can target the same connected phone at nearly the same time. One agent closes or uninstalls the app another agent just installed and launched, and the device becomes an unreliable execution target.

Device selection alone does not fix this. `adb -s SERIAL` tells ADB *which* device to address; it provides no ownership, blocking, or queueing semantics. The missing capability is a shared-resource manager for physical devices — the same problem CI systems solve with lockable resources, where a job waits until an external resource becomes available.

## Goals

- Exclusive access to one Android device per active session.
- Competing clients wait in a queue instead of interfering.
- Per-device locking keyed on serial numbers from `adb devices`.
- Multiple ADB commands grouped into one atomic session, not locked command-by-command.
- Apps installed during a session are tracked and uninstalled on release when configured.
- Safe recovery when a client crashes, times out, or disconnects mid-session.
- Local-first, with a simple path to CI and remote workers.
- Easy adoption: all ADB usage flows through one wrapper, CLI, or broker API.

## Non-Goals

- Replacing ADB.
- Managing iOS devices.
- Running a full cloud device farm in v1.
- Deep UI-automation features (gesture recording, screenshot diffing, test authoring).
- Cleaning up apps that predate the session, unless explicitly marked session-owned.

## Users

**Primary:** AI coding agents working on Android apps in separate repositories; developers running multiple agent sessions on one machine; CI pipelines sharing a small pool of physical devices.

**Secondary:** teams building internal Android automation; device-lab maintainers who need a lightweight broker before a full device farm.

## Core Use Cases

1. **Two agents, one phone.** Agent A leases `SERIAL_1`. Agent B requests the same device and waits in the queue until A releases or loses the lease.
2. **Install, launch, test, uninstall.** An agent acquires the device, installs its debug APK, launches and validates the app, then releases. The broker uninstalls the session-installed package, leaving the phone clean.
3. **Crash recovery.** An agent acquires a lease and crashes. The broker detects the missing heartbeat, expires the lease, runs cleanup, and frees the device.
4. **Multiple devices.** With several phones connected, the broker assigns a free device automatically or honors a preferred serial. Requests for busy devices wait; requests for "any free device" schedule faster.

## Scope

The repository contains:

- Broker service
- CLI client
- SDK / HTTP API for agents
- Session-aware ADB wrapper
- Cleanup and uninstall policies
- Logs and audit trail
- Sample integrations: agent, shell, CI

## Functional Requirements

### 1. Device Discovery

Maintain a live registry of connected devices: serial, connection state, and optional labels (model, tags).

- Read devices from `adb devices -l`.
- Track state per device: `free`, `leased`, `offline`, `cleanup`, `error`.
- Refresh on a configurable interval.
- Expose state via CLI and API.

### 2. Lock and Lease Management

Provide exclusive leases per device and queue conflicting requests.

- Acquire by exact serial, or by constraints (`usb`, `model`, `tag`).
- FIFO queue by default.
- Return a lease ID on acquisition.
- Support blocking, polling, or streaming wait status.
- Timeouts for waiting requests.
- Lease TTL with heartbeat renewal.
- Auto-release expired leases.

### 3. Session Model

Treat a device interaction as one session, not isolated commands.

- A session starts at lease acquisition.
- Every ADB command in the session targets the leased serial automatically.
- Commands against a device with no active lease are rejected.
- Session metadata: project, agent, repo path, package names, start time, cleanup policy.

### 4. Command Execution

Provide safe command execution through the broker or wrapper.

- Support `shell`, `install`, `uninstall`, `push`, `pull`, `logcat`, and `am`-style commands.
- Automatically prefix every command with the leased serial.
- Record stdout, stderr, exit code, and timestamps per command.
- Two modes: direct passthrough wrapper, or broker-executed command.
- Prevent raw parallel access for clients using the official CLI.

### 5. Install Tracking and Cleanup

Uninstall session-installed apps when the lease ends.

- Track package names installed during the lease.
- Install sources: APK path, split APK set, caller-declared package name, or parsed APK metadata.
- Cleanup policies: `none`, `uninstall-installed-apps`, `uninstall-explicit-packages`, `force-stop-only`.
- On release, uninstall tracked packages per policy; log and surface failures.
- Allowlist to protect packages that must never be uninstalled.

### 6. Release Flow

Cleanup hooks run in a fixed order:

1. Stop active test or command stream.
2. Optionally capture final logs.
3. Force-stop tracked packages (if configured).
4. Uninstall tracked packages (if configured).
5. Clear temp files pushed by the broker.
6. Release the lock and wake the next queued request.

### 7. Crash Safety

- Client-to-broker lease heartbeat.
- Lease expiry on missed heartbeats.
- Idempotent cleanup on repeated release attempts.
- Stale-lease recovery after broker restart, from persisted state.
- Manual admin override (`force-release`).

### 8. Audit and Observability

- `who-has-device <serial>`.
- Session history with timestamps.
- Per-device queue visibility.
- Cleanup outcome per session.
- Structured JSON logs.
- Optional metrics: queue time, session duration.

### 9. Policy Controls

- User/agent identity attached to each session.
- Max lease duration and max queue wait.
- Protected devices reserved by label.
- Admin-only force release.
- Optional package allowlist / denylist.

## CLI

```bash
adb-lock devices
adb-lock acquire --serial SERIAL_1 --project app-one --agent claude-a --cleanup uninstall-installed-apps
adb-lock run --lease LEASE_ID -- adb shell pm list packages
adb-lock install --lease LEASE_ID app-debug.apk --package com.example.app
adb-lock release --lease LEASE_ID
adb-lock status --lease LEASE_ID
adb-lock who-has-device --serial SERIAL_1
adb-lock force-release --serial SERIAL_1
```

## HTTP API

| Method | Endpoint | Purpose |
|---|---|---|
| POST | `/leases` | Request a lease for a device or matching device set |
| GET | `/leases/{id}` | Read lease status |
| POST | `/leases/{id}/heartbeat` | Renew lease |
| POST | `/leases/{id}/commands` | Run an ADB command under the lease |
| POST | `/leases/{id}/installs` | Register an installed package or APK |
| POST | `/leases/{id}/release` | Release lease and trigger cleanup |
| GET | `/devices` | List device inventory |
| GET | `/devices/{serial}` | View device state and queue |
| POST | `/devices/{serial}/force-release` | Admin override |

## Architecture

A small central broker process owns leases, queue state, cleanup logic, and audit records. Clients never call raw `adb` for shared devices; they go through the broker CLI or API so every action is tied to a lease.

| Component | Responsibility |
|---|---|
| Broker service | Lease management, queueing, state, cleanup orchestration |
| Device adapter | Talks to `adb`: discovery and command execution |
| CLI | Developer and agent entry point |
| State store | Persists leases, queue, session metadata |
| Cleanup engine | Install tracking, force-stop, uninstall |
| Audit logger | Structured event logs and metrics |

**State store:** SQLite for v1 (local, durable, zero infra). Redis or Postgres are candidates for a distributed v2. In-memory-only is ruled out — state must survive broker restarts.

**Locking:** the logical lease is owned by the broker. A host-level file lock may be used inside the broker or wrapper as an extra guard on single-machine setups.

## Key Flows

**Lease acquisition:** client requests by serial or label → broker checks device state → if free, create lease and return ID; if busy, enqueue → client waits (block / poll / stream) → on release, next queued request activates.

**Session cleanup:** release request or lease expiry → broker locks cleanup state → reads tracked packages → force-stops and uninstalls per policy → records outcome → releases device → activates next request.

**Crash recovery:** heartbeat misses threshold → lease marked stale → recovery cleanup runs → lock released → next queued request wakes.

## Acceptance Criteria

**Must have**

- Two clients requesting the same device cannot execute overlapping sessions.
- The second client waits until the first lease ends.
- A lease covers install, launch, test, and release as one unit.
- Session-installed packages are uninstalled automatically on release.
- Stale leases are auto-recovered.
- Device state and queue are visible from the CLI.
- Cleanup failures are visible in logs and status.

**Nice to have**

- Auto-select least-busy free device.
- Web dashboard.
- Slack / webhook notifications.
- Per-project cleanup presets.
- Video recording and screenshots per session.

## Edge Cases

- Device disconnects during an active lease.
- Multiple packages installed in one session.
- Install succeeds but package metadata is missing.
- Manual changes on the phone during a lease.
- A pre-existing package is upgraded during the session.
- Cleanup fails because the device is offline.
- Broker restarts mid-cleanup.

## Security

- Restrict admin operations such as force release.
- Validate all CLI and API inputs before execution.
- Use structured subprocess calls; never interpolate into shell strings.
- Keep an audit log of who used which device and what was installed or removed.
- Token-based auth if exposed beyond localhost.

## Operational Model

**v1 — local-first:** the broker runs on the machine where the phones are connected, matching the host-local nature of the ADB server.

**Later — remote mode:** one host exposes the broker over HTTP so remote agents can lease devices through the machine that owns the ADB server.

## Metrics

Lease success rate · average wait time · average session duration · cleanup success rate · stale-lease recoveries · device utilization by serial · uninstall failures.

## Rollout

| Phase | Scope |
|---|---|
| 1 — MVP | Single host, USB devices, serial locking, CLI acquire/release, install tracking, auto-uninstall, SQLite |
| 2 — Team | HTTP API, heartbeats, queue inspection, admin force release, labels and scheduling |
| 3 — Scale | Remote workers, multi-device pools, dashboard, CI templates, Redis/Postgres backend |

## Open Questions

- Should the broker execute all ADB commands itself, or return a scoped token and rely on a guarded wrapper?
- Should uninstall be the default cleanup behavior or opt-in?
- How are packages identified for split-APK installs when the caller doesn't declare them?
- Should a lease survive a short broker restart window?
- Should sessions support snapshot / baseline reset for dedicated test phones?

## Recommendation

Ship v1 as a local broker with a CLI and SQLite-backed queue, built around one contract: **no ADB access without a lease, and every lease ends with deterministic cleanup.** This is far lighter than a device farm while solving the same shared-resource problem that lockable CI resources solve for other external hardware.
