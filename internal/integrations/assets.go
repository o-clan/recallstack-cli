package integrations

import _ "embed"

// Skill teaches every agent the commands. One text for all of them.
//
//go:embed assets/skill.md
var Skill []byte
