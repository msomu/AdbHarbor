// Package skills embeds the agent skills shipped with adbharbor so the
// installer can place them without needing the source tree on disk.
package skills

import _ "embed"

// ClaudeSkill is the Claude Code skill teaching agents the lease model:
// why an adb command waits, what exit 75 means, and how to tell their own
// lease from another agent's.
//
//go:embed claude/adbharbor/SKILL.md
var ClaudeSkill []byte
