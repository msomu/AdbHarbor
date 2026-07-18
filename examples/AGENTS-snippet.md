# AdbHarbor snippet for agent instructions (AGENTS.md, .codex, etc.)

Paste this into the global instructions of any coding agent that uses adb
on a machine with AdbHarbor installed:

```markdown
## ADB device access (AdbHarbor)

`adb` on this machine is wrapped by AdbHarbor, a lock broker that
serializes device access between concurrent agents. Run adb normally.

- If a device is busy, your adb command WAITS in a queue (stderr shows the
  holder) and then runs. This is normal — do not interrupt it.
- Exit code 75 = gave up waiting for a busy device. It is NOT an app or
  device error: retry later, or run `adbharbor devices` and use a serial
  marked `free` instead.
- Your own consecutive adb commands share one lease and never block each
  other; another agent cannot interleave with your install/launch/test
  sequence.
- Never run `adbharbor release --force` on a device held by someone else
  unless the holder is provably dead; crashed holders are auto-reclaimed
  within ~2 minutes.
- For a long exclusive run: `adbharbor acquire -s SERIAL --ttl 30m`, then
  `adbharbor release -s SERIAL` when done.
```
