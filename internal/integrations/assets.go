package integrations

import _ "embed"

//go:embed assets/codex-skill.md
var codexSkill []byte

//go:embed assets/claude-skill.md
var claudeSkill []byte

//go:embed assets/cursor-rule.mdc
var cursorRule []byte

//go:embed assets/copilot-instructions.md
var copilotInstructions []byte

//go:embed assets/copilot-plugin-skill.md
var copilotPluginSkill []byte

//go:embed assets/antigravity-skill.md
var antigravitySkill []byte
