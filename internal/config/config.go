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
	// SSOAutoLogin lets the server start an SSO device-authorization flow itself
	// when a profile's session has expired beyond refresh, and report the
	// verification URL and user code in the failing tool's result.
	SSOAutoLogin bool
	// SSOOpenBrowser additionally opens that URL locally. Best-effort and
	// skipped in a container, where there is no browser to open.
	SSOOpenBrowser bool
	// CredentialsPassthrough makes the server hold no AWS credentials of its
	// own: every request must forward them in the X-Aws-Credentials header,
	// and one without them is refused.
	CredentialsPassthrough bool
	// ReadWrite registers the write tools (create/update/delete resources, tag
	// and untag, cancel requests). Off by default: without it the server
	// exposes only tools that cannot change AWS.
	ReadWrite bool
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
		SSOAutoLogin:   cmd.Bool("sso-auto-login"),
		SSOOpenBrowser: cmd.Bool("sso-open-browser"),

		CredentialsPassthrough: cmd.Bool("credentials-passthrough"),
		ReadWrite:              cmd.Bool("read-write"),
	}
}

// Validate fails fast on missing or malformed required settings.
func (c *Config) Validate() error {
	if c.Addr == "" {
		return errors.New("--addr (MCP_ADDR) is required")
	}

	// The four authentication modes are exclusive: local (no flags),
	// multi-profile (--profiles), cross-account (--assume-role*) and
	// passthrough (--credentials-passthrough).
	if c.Profiles != "" && (c.AssumeRole || c.AssumeRoleArns != "") {
		return errors.New("--profiles cannot be combined with --assume-role or --assume-role-arns; pick one authentication mode")
	}

	// Passthrough uses the client's credentials and nothing else, so a server
	// profile or role alongside it could only ever be silently ignored.
	if c.CredentialsPassthrough && (c.Profiles != "" || c.AssumeRole || c.AssumeRoleArns != "") {
		return errors.New("--credentials-passthrough cannot be combined with --profiles, --assume-role or --assume-role-arns: in passthrough mode the server uses only the credentials each request forwards")
	}

	return nil
}
