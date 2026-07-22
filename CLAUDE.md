# AdbHarbor — working notes for agents

## Orchestration

Use dynamic workflows (the `Workflow` tool) whenever the task plausibly
benefits: multi-file investigations, wide audits, anything that fans out
across independent tracks, and any review or verification pass worth doing
adversarially. Prefer authoring a workflow over doing the same work serially
in the main loop.

## Verifying changes

The broker's behaviour is testable without a device: `internal/harbor/broker_test.go`
drives the real acquire/queue/expire path in-process at controlled instants
(`sweep(now)`), so handoff rules are asserted rather than argued. Add to it
rather than reaching for a phone.

Some tests spawn the test binary as a child to get a real pid to inspect
(`startHelper`). That is deliberate: macOS redacts the environment of
SIP-protected platform binaries, so process inspection has to be exercised
against a binary we built.

## Shell gotcha on this machine

`cat` is aliased to `bat`, which is not installed — a heredoc written with
`cat >> file` fails silently and appends nothing. Use the Write/Edit tools,
or `python3` for appends.
