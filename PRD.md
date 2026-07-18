# PRD: ADB Device Lock Broker for Multi-Agent Android Development

## Overview

This product is a standalone GitHub repository that provides exclusive, queue-based access to shared Android devices for multiple coding agents, local scripts, and CI jobs. It solves the collision problem where two or more agents issue ADB commands to the same phone at the same time, causing app state conflicts such as force-stopping, launching, installing, or uninstalling each other’s app sessions.

ADB uses a client-server architecture on the host machine, and the ADB server manages communication between local ADB clients and the device-side daemon. The ADB server listens on port 5037 by default, which means multiple local clients can talk to the same shared server unless a separate coordination layer is introduced.

The proposed repository adds that coordination layer. It introduces a lock broker that grants one agent an exclusive lease on a specific Android device, queues other agents until the device becomes free, and ensures cleanup after the lease ends, including optional uninstall of apps installed during the session.

## Problem Statement

When multiple Claude agents work on different Android projects on the same machine, they can both attempt to use the same connected phone at nearly the same time. This causes race conditions: one agent may close or uninstall an app that the other agent just installed or launched, and the device becomes an unreliable execution target.

The root issue is not device selection alone. ADB supports targeting a device by serial number, but that only tells ADB which device to address; it does not provide exclusive ownership, blocking, or queueing semantics for a device session.

The missing capability is a shared resource manager for physical devices. Jenkins solves a similar problem in CI with lockable resources, where a build waits until a device-like external resource becomes available.

## Goals

- Provide exclusive access to one Android device per active session.
- Make competing agents wait in a queue instead of interfering with each other.
- Support per-device locking using device serial numbers from `adb devices` output.
- Group multiple ADB commands into one atomic session rather than locking command-by-command.
- Track apps installed during the session and uninstall them on release when configured.
- Recover safely if an agent crashes, times out, or disconnects mid-session.
- Work for local development first, with simple extension to CI and remote workers.
- Be easy to adopt by forcing all ADB usage through one wrapper, CLI, or broker API.

## Non-Goals

- Replace ADB itself.
- Manage iOS devices.
- Run a full cloud device farm in v1, since device farm systems are broader and heavier than the one-phone or few-phone use case.
- Offer deep UI automation features such as gesture recording, screenshot diffing, or test authoring.
- Guarantee cleanup for apps that were already on the phone before the session unless they are explicitly marked as session-owned.

## Users

### Primary Users

- AI coding agents working on Android apps in separate repositories.
- Developers running multiple agent sessions on one machine.
- CI pipelines that share a small pool of physical Android devices.

### Secondary Users

- Teams building internal Android automation tools.
- Device lab maintainers who need a light-weight broker before moving to a full device farm.

## Core Use Cases

### Use Case 1: Two Claude agents, one USB phone

Agent A requests a lease for device `SERIAL_1` and receives the lock. Agent B requests the same device while Agent A is still working, so Agent B waits in a queue until Agent A releases or loses its lease.

### Use Case 2: Install, launch, test, uninstall

An agent acquires the device, installs its debug APK, launches the app, performs validation, and releases the lease. On release, the broker uninstalls the app package that was installed during the session, leaving the phone clean for the next job.

### Use Case 3: Crash recovery

An agent acquires a lease and then crashes. The broker detects lease expiry or missing heartbeat, releases the lock, and executes cleanup steps according to policy.

### Use Case 4: Multiple devices

If three phones are connected, the broker can assign a free device automatically or honor a preferred serial. Requests for busy devices wait; requests for any free compatible device can be scheduled faster.

## Product Scope

The repository should contain:

- A broker service.
- A CLI client.
- A simple SDK or HTTP API for agents.
- A session-aware ADB wrapper.
- Cleanup and uninstall policies.
- Logs and audit trails.
- Sample integrations for Claude Code style agents, shell scripts, and CI pipelines.

## Functional Requirements

### 1. Device Discovery

The system must detect connected Android devices using ADB and maintain a live registry of available serial numbers, connection state, and optional labels such as model or tags.

#### Requirements

- Read connected devices from `adb devices -l`.
- Mark each device as `free`, `leased`, `offline`, `cleanup`, or `error`.
- Refresh state on a configurable interval.
- Expose device state through CLI and API.

### 2. Lock and Lease Management

The system must provide exclusive leases per device and queue conflicting requests.

#### Requirements

- Acquire lock by exact serial.
- Optionally acquire any device matching constraints such as `usb`, `model`, or `tag`.
- Support FIFO queue by default.
- Return a lease ID on successful acquisition.
- Block, poll, or stream wait status to the caller.
- Support timeout for waiting requests.
- Support lease TTL and heartbeat renewal.
- Auto-release expired leases.

### 3. Session Model

The system must treat a device interaction as one session rather than isolated ADB commands.

#### Requirements

- A session starts after lease acquisition.
- All ADB commands in the session must use the leased device serial automatically.
- The system must reject ADB commands for a device if no active lease exists.
- Session metadata must include project name, agent name, repo path, package names, start time, and cleanup policy.

### 4. ADB Command Execution

The system must provide safe command execution through the broker or wrapper.

#### Requirements

- Support `shell`, `install`, `uninstall`, `push`, `pull`, `logcat`, and `am` style commands.
- Prefix every command with the leased device serial automatically.
- Store stdout, stderr, exit code, and timestamps per command.
- Allow command execution in two modes: direct passthrough wrapper or broker-executed command.
- Prevent raw parallel access when users go through the official CLI.

### 5. Install Tracking and Uninstall on Release

The system must support uninstalling session-installed apps when the lease ends.

#### Requirements

- Track package names installed during the lease.
- Support install sources: APK path, split APK set, package name declared by caller, or parsed package metadata when possible.
- Cleanup policy options:
  - `none`
  - `uninstall-installed-apps`
  - `uninstall-explicit-packages`
  - `force-stop-only`
- On release, uninstall tracked packages if configured.
- If uninstall fails, log the failure and mark cleanup state accordingly.
- Support allowlist to avoid uninstalling protected packages such as preinstalled system apps.

### 6. Cleanup Hooks

The system must run cleanup hooks in a predictable order.

#### Release Flow

1. Stop active test or command stream.
2. Optionally capture final logs.
3. Force-stop tracked packages if configured.
4. Uninstall tracked packages if configured.
5. Clear temporary files if they were pushed by the broker.
6. Release the lock.

### 7. Crash Safety

The system must survive agent crashes and machine interruptions.

#### Requirements

- Lease heartbeat from client to broker.
- Lease expiry when heartbeat is missing.
- Idempotent cleanup on repeated release attempts.
- Stale lease recovery after broker restart using persisted state.
- Optional manual override command for admins.

### 8. Audit and Observability

The system must make device ownership easy to inspect.

#### Requirements

- `who-has-device <serial>` command.
- Session history with timestamps.
- Queue visibility per device.
- Cleanup outcome per session.
- Structured logs in JSON.
- Optional OpenTelemetry or simple metrics for queue time and session duration.

### 9. Multi-User and Policy Controls

The system must support basic policy enforcement.

#### Requirements

- User or agent identity attached to each session.
- Max lease duration.
- Max queue wait duration.
- Protected devices reserved by label.
- Admin-only force release.
- Optional package allowlist or denylist.

## API and CLI Requirements

### CLI Commands

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

### API Endpoints

| Method | Endpoint | Purpose |
|---|---|---|
| POST | `/leases` | Request a lease for a device or matching device set |
| GET | `/leases/{id}` | Read lease status |
| POST | `/leases/{id}/heartbeat` | Renew lease |
| POST | `/leases/{id}/commands` | Run an ADB command under the lease |
| POST | `/leases/{id}/installs` | Register installed package or APK |
| POST | `/leases/{id}/release` | Release lease and trigger cleanup |
| GET | `/devices` | List device inventory |
| GET | `/devices/{serial}` | View device state and queue |
| POST | `/devices/{serial}/force-release` | Admin override |

## User Stories

### Epic 1: Safe device ownership

- As an agent, the system should grant a lease before any ADB action so that no other agent can interfere.
- As a waiting agent, the system should queue the request and notify when the phone is free.
- As a developer, the system should show who currently owns the device and for how long.

### Epic 2: Clean device after use

- As a developer, the phone should be returned to a clean state after a session.
- As an agent, any app installed during the session should be uninstalled automatically when cleanup policy requires it.
- As an admin, protected packages should never be removed accidentally.

### Epic 3: Reliability

- As a team, stale locks should disappear when the owning agent dies.
- As a CI engineer, builds should not hang forever on broken leases.
- As an operator, cleanup failures should be visible and retryable.

## Architecture

### Recommended v1 Architecture

A small central broker process should manage leases, queue state, cleanup logic, and audit records. Clients should not call raw `adb` directly for shared devices; they should go through the broker CLI or API so that every action is tied to a lease.

### Components

| Component | Responsibility |
|---|---|
| Broker service | Lease management, queueing, state, cleanup orchestration |
| Device adapter | Talks to `adb`, discovers devices, runs commands. |
| CLI | Developer and agent entry point |
| State store | Persists leases, queue, session metadata |
| Cleanup engine | Tracks installs and runs force-stop and uninstall |
| Audit logger | Structured event logs and metrics |

### State Store Options

| Option | Pros | Cons | Recommendation |
|---|---|---|---|
| In-memory only | Very simple | Loses state on restart | Not recommended |
| SQLite | Easy, local, durable | Single host focus | Best for v1 |
| Redis | Fast, supports distributed locks | Extra infra | Good for v2 |
| Postgres | Strong durability | More setup | Good for v2+ |

### Locking Strategy

The logical lease should be owned by the broker. A host-level file lock can still be used inside the broker process or wrapper as an extra guard on single-machine setups, following the same resource-lock pattern commonly used for shared external resources in CI.

## Repository Proposal

### Repository Name Ideas

- `adb-device-broker`
- `adb-lock-broker`
- `android-device-lease-manager`
- `shared-adb-gateway`

### Suggested Structure

```text
adb-lock-broker/
├── README.md
├── docs/
│   ├── architecture.md
│   ├── api.md
│   ├── cli.md
│   ├── cleanup-policy.md
│   └── agent-integration.md
├── broker/
├── cli/
├── sdk/
├── examples/
│   ├── claude-agent/
│   ├── shell/
│   └── ci/
├── tests/
├── docker/
├── .github/workflows/
└── PRD.md
```

## Recommended Tech Stack

### Preferred Stack

Given the need for easy shell integration, local development, and GitHub-hosted collaboration, a TypeScript or Go implementation is a strong fit.

| Stack | Pros | Cons |
|---|---|---|
| TypeScript + Node.js | Fast to build, easy CLI, JSON-friendly, good HTTP libraries | More runtime overhead |
| Go | Single binary, great concurrency, simple deployment | Slightly slower iteration for some teams |
| Python | Fast prototyping, easy subprocess control | Packaging can get messy across environments |

### Recommendation

TypeScript is best if the main users are developer-tool builders who already use Node in automation. Go is best if the main goal is a single static binary with strong concurrency and simple install. Either works well for v1.

## Detailed Flows

### Lease Acquisition Flow

1. Client requests lease by serial or label.
2. Broker checks device state.
3. If free, broker creates active lease and returns lease ID.
4. If busy, broker adds request to queue.
5. Client waits via blocking call, polling, or event stream.
6. When the device is released, the next queued request becomes active.

### Session Cleanup Flow

1. Client sends release request or lease expires.
2. Broker locks cleanup state for the session.
3. Broker reads tracked installed packages.
4. Broker force-stops packages if configured.
5. Broker uninstalls packages if configured.
6. Broker records cleanup result.
7. Broker releases the device.
8. Broker activates the next waiting request.

### Crash Recovery Flow

1. Heartbeat misses threshold.
2. Broker marks lease stale.
3. Broker starts recovery cleanup.
4. Broker releases the lock.
5. Broker wakes next queued request.

## Acceptance Criteria

### Must Have

- Two agents requesting the same device cannot execute overlapping sessions.
- Second agent waits until the first lease ends.
- A lease can cover install, launch, test, and release as one unit.
- Packages installed during the lease can be uninstalled automatically on release.
- Stale leases are auto-recovered.
- Device state and queue are visible by CLI.
- Cleanup failures are visible in logs and status.

### Nice to Have

- Auto-select least-busy free device.
- Web dashboard.
- Slack or webhook notifications.
- Per-project cleanup presets.
- Video recording and screenshots per session.

## Edge Cases

- Device disconnects during active lease.
- Agent installs multiple packages.
- Install succeeds but package tracking metadata is missing.
- Manual user changes on the phone during the lease.
- A package existed before the session and gets upgraded during the session.
- Cleanup command fails because the device is offline.
- Broker restarts while a cleanup flow is in progress.

## Security Considerations

- Restrict admin operations such as force release.
- Validate all CLI and API inputs before shell execution.
- Avoid arbitrary shell injection by using structured subprocess calls.
- Keep audit logs for who used which phone and what package was installed or removed.
- Optionally support token-based auth if exposed beyond localhost.

## Operational Considerations

### Local-First Mode

In the first version, the broker should run on the same machine where the Android phone is connected. This matches the host-local nature of the ADB server and avoids extra networking complexity in v1.

### Remote Mode

In a later version, a single host can expose the broker over HTTP so multiple remote agents can lease devices through one owner machine. This still preserves the one-broker model around the host-local ADB server.

## Metrics

The product should track:

- Lease success rate.
- Average wait time.
- Average session duration.
- Cleanup success rate.
- Stale lease recovery count.
- Device utilization by serial.
- Number of uninstall failures.

## Rollout Plan

### Phase 1: MVP

- Single host.
- USB devices.
- Serial-based locking.
- CLI acquire and release.
- Session install tracking.
- Auto uninstall on release.
- SQLite persistence.

### Phase 2: Team Use

- HTTP API.
- Heartbeats.
- Queue inspection.
- Admin force release.
- Labels and scheduling.

### Phase 3: Scale

- Remote workers.
- Multi-device pools.
- Dashboard.
- CI templates.
- Redis or Postgres backend.

## Open Questions

- Should the broker execute all ADB commands itself, or should it return a scoped token and rely on a guarded wrapper?
- Should uninstall be the default cleanup behavior or opt-in?
- How should the system identify packages for split APK installs when the package is not declared by the caller?
- Should a lease survive a short broker restart window?
- Should sessions support snapshot or baseline reset for dedicated test phones?

## Recommendation

The best v1 is a standalone GitHub repository with a local broker, CLI, and SQLite-backed queue. It should provide one clear contract: no ADB access without a lease, and every lease ends with deterministic cleanup that can uninstall apps installed during that session. This approach is much lighter than a full device farm, but it directly addresses the same shared-resource problem that lockable CI systems already solve for phones and other external hardware.
