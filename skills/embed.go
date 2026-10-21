// Package skills embeds the agent-facing usage guides shipped in this directory,
// so the server binary serves the guide that matches its own tools.
//
// The markdown files are the source of truth: edit them, not a copy. They also
// work unmodified as Claude Code skills (skills/<name>/SKILL.md).
package skills

import (
	_ "embed"
)

// UsingAWSMCP is skills/using-aws-mcp/SKILL.md, frontmatter included.
//
//go:embed using-aws-mcp/SKILL.md
var UsingAWSMCP string
