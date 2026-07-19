# Video demo run-book

A ~2.5 minute screen-recording script showcasing AdbHarbor: two "agents"
compete for one phone, the harbor queues them, and session cleanup wipes
the app off the device on camera.

## Screen layout

- **Left terminal pane** — "Agent Claude"
- **Right terminal pane** — "Agent Codex"
- **Bottom terminal pane** — live harbor status
- **scrcpy window** (right side of screen) — the phone's actual display

## Pre-flight (off camera, ~2 min)

```bash
# clean slate
pkill -f ".adbharbor/bin/adb " 2>/dev/null; adbharbor stop; rm -f ~/.adbharbor/state.json
adbharbor cleanup on          # for the finale
adbharbor devices             # daemon restarts; both devices free

# every pane: fill in YOUR values (no angle brackets — they break zsh)
S=YOUR_PIXEL_SERIAL
APK=/path/to/any/app-debug.apk
PKG=the.apps.package.name
# left pane:   export ADB_HARBOR_SESSION=claude ADB_HARBOR_IDLE=5
# right pane:  export ADB_HARBOR_SESSION=codex  ADB_HARBOR_IDLE=5

scrcpy -s $S --window-title "Pixel" --max-size 800 &   # phone on screen
```

Bottom pane, leave running for the whole video:

```bash
while true; do clear; adbharbor devices; sleep 1; done
```

## Beats

### 1. Two phones, zero config (0:00–0:15)

Nothing to narrate but the status pane: both devices `free`. Say: every
adb client on this machine — any path, Maestro, CI — is brokered because
the harbor owns ADB server port 5037.

### 2. Agent Claude takes the device (0:15–0:45)

Left pane:

```bash
adb -s $S install -r $APK
adb -s $S shell monkey -p $PKG 1     # app opens on the scrcpy window
```

Status pane flips to `claude (…)`. The phone shows the app launching.

### 3. Agent Codex queues instead of fighting (0:45–1:15)

Right pane, while Claude's lease is live:

```bash
adb -s $S shell "sleep 1; echo codex got the device"
```

On camera: `adbharbor: device … is busy (held by claude …); waiting in
queue (position 1)` — then ~5s after Claude's last command it runs by
itself. Say: before AdbHarbor, this command would have force-stopped
Claude's app mid-test.

### 4. Even bypassing the shim doesn't bypass the broker (1:15–1:40)

Left pane — hold the device again (`adb -s $S shell sleep 10`).
Right pane — call the SDK binary directly, no shim:

```bash
~/Library/Android/sdk/platform-tools/adb -s $S shell "echo raw adb, still brokered"
```

It waits silently, then runs. Say: absolute paths, Maestro, Android
Studio — same port, same queue. Read-only commands stay instant:

```bash
~/Library/Android/sdk/platform-tools/adb -s $S shell getprop ro.product.model  # instant, even while held
```

### 5. Session cleanup finale (1:40–2:20)

Point at the phone: the app Claude installed is still there. Wait out
Claude's 5s idle (watch status flip to `session cleanup`, then `free`) —
the app icon vanishes from the phone on camera. Show the receipt:

```bash
tail -2 ~/.adbharbor/history.jsonl   # "cleanup … removed <pkg>"
```

Say: opt-in — snapshot diff at lease start/end, so pre-existing apps are
never touched.

### 6. Close (2:20–2:30)

```bash
brew install msomu/tap/adbharbor
```

github.com/msomu/AdbHarbor — no ADB access without a lease, every lease
ends clean.

## After recording

```bash
adbharbor cleanup off   # restore your default
```

## Gotchas

- `ADB_HARBOR_IDLE=5` in the agent panes is what keeps the video fast;
  the real default linger is 5 minutes.
- Don't use `echo`/`getprop` as the *contended* command — read-only
  commands are lease-exempt and won't queue (that's beat 4's punchline,
  not beat 3's).
- Stray waiters from aborted takes: rerun the pre-flight reset between
  takes.
