// Package config holds server configuration and its validation.
package config

import (
	"time"

	"github.com/urfave/cli/v3"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// Config is the fully-resolved MCP server configuration.
type Config struct {
	// Addr is the listen address for the HTTP server (e.g. ":3040"); the MCP
	// endpoint is served at the "/mcp" path.
	Addr string
	// CacheTTL is the time-to-live for cached AWS resource listings.
	CacheTTL time.Duration
	// CacheDir is the on-disk cache directory; empty means in-memory only.
	CacheDir string
	// NoCache disables caching entirely.
	NoCache bool
	// AssumeRole enables cross-account mode by auto-discovering assumable roles.
	AssumeRole bool
	// AssumeRoleArns is a comma-separated list of assumable role ARNs; when set
	// it enables cross-account mode and overrides auto-discovery.
	AssumeRoleArns string
	// Profiles is a comma-separated list of AWS shared-config profile names;
	// when set, each profile is served as its own account.
	Profiles string
}

// FromCommand builds a Config from the serve command's flags.
func FromCommand(cmd *cli.Command) *Config {
	return &Config{
		Addr:           cmd.String("addr"),
		CacheTTL:       cmd.Duration("cache-ttl"),
		CacheDir:       cmd.String("cache-dir"),
		NoCache:        cmd.Bool("no-cache"),
		AssumeRole:     cmd.Bool("assume-role"),
		AssumeRoleArns: cmd.String("assume-role-arns"),
		Profiles:       cmd.String("profiles"),
	}
}

// Validate fails fast on missing or malformed required settings.
func (c *Config) Validate() error {
	if c.Addr == "" {
		return errors.New("--addr (MCP_ADDR) is required")
	}

	// The three authentication modes are exclusive: local (no flags),
	// multi-profile (--profiles) and cross-account (--assume-role*).
	if c.Profiles != "" && (c.AssumeRole || c.AssumeRoleArns != "") {
		return errors.New("--profiles cannot be combined with --assume-role or --assume-role-arns; pick one authentication mode")
	}

	return nil
}
