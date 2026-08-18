package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// ssoTokenExpiryWarning is how close to expiry a cached SSO token has to be
// before startup warns about it. Below this the server is likely to outlive the
// token, and a refresh needs the SSO session to still be valid.
const ssoTokenExpiryWarning = 30 * time.Minute

// ProfileAuth describes how a shared-config profile obtains credentials. It is
// resolved by parsing ~/.aws/config only — no AWS call is made.
type ProfileAuth struct {
	// Name is the profile name; empty means the default credential chain.
	Name string
	// Region is the profile's configured region, if any.
	Region string
	// SSOSession is the sso_session name for a modern SSO profile.
	SSOSession string
	// SSOAccountID / SSORoleName are set for any SSO profile.
	SSOAccountID string
	SSORoleName  string
	// SSOStartURL is set for a legacy SSO profile (sso_start_url on the profile
	// itself, no sso_session), which the SDK cannot refresh.
	SSOStartURL string
	// CredentialProcess is the configured credential_process command, if any.
	CredentialProcess string
}

// IsSSO reports whether the profile resolves credentials through AWS SSO.
func (a ProfileAuth) IsSSO() bool {
	return a.SSOSession != "" || a.SSOStartURL != ""
}

// IsLegacySSO reports whether the profile uses the pre-sso_session format,
// which has no refresh token and expires for good when the token does.
func (a ProfileAuth) IsLegacySSO() bool {
	return a.SSOSession == "" && a.SSOStartURL != ""
}

// LoginHint is the command that re-authenticates this profile, or "" when the
// profile is not SSO-backed.
func (a ProfileAuth) LoginHint() string {
	switch {
	case a.SSOSession != "":
		return "aws sso login --sso-session " + a.SSOSession
	case a.SSOStartURL != "":
		return "aws sso login --profile " + a.Name
	default:
		return ""
	}
}

// SSOTokenStatus is the state of the cached SSO token backing a profile.
type SSOTokenStatus struct {
	// Path is where the SDK looks the token up (sha1 of the session name).
	Path string
	// Found is false when no login has happened yet for this session.
	Found bool
	// ExpiresAt is the cached token's expiry; zero when unknown.
	ExpiresAt time.Time
	// CacheWritable reports whether the SDK can replace the token file on
	// refresh — it writes a temp file into the cache directory and renames it,
	// so the *directory* must be writable, not just the file.
	CacheWritable bool
}

// Expired reports whether the cached token is past its expiry.
func (s SSOTokenStatus) Expired() bool {
	return !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt)
}

// InspectProfile parses the shared config for a profile without contacting AWS.
// An empty name inspects the default profile.
func InspectProfile(ctx context.Context, name string) (ProfileAuth, error) {
	lookup := name
	if lookup == "" {
		lookup = "default"
	}

	// LoadSharedConfigProfile defaults to ~/.aws/{config,credentials} and, unlike
	// LoadDefaultConfig, does not consult AWS_CONFIG_FILE /
	// AWS_SHARED_CREDENTIALS_FILE itself — so the overrides are applied here,
	// or the preflight would inspect different files than the credential chain.
	env, err := awsconfig.NewEnvConfig()
	if err != nil {
		return ProfileAuth{Name: name}, errors.WithStack(err)
	}

	cfg, err := awsconfig.LoadSharedConfigProfile(ctx, lookup, func(o *awsconfig.LoadSharedConfigOptions) {
		if env.SharedConfigFile != "" {
			o.ConfigFiles = []string{env.SharedConfigFile}
		}
		if env.SharedCredentialsFile != "" {
			o.CredentialsFiles = []string{env.SharedCredentialsFile}
		}
	})
	if err != nil {
		return ProfileAuth{Name: name}, errors.WithStack(err)
	}

	auth := ProfileAuth{
		Name:              name,
		Region:            cfg.Region,
		SSOSession:        cfg.SSOSessionName,
		SSOAccountID:      cfg.SSOAccountID,
		SSORoleName:       cfg.SSORoleName,
		CredentialProcess: cfg.CredentialProcess,
	}

	// A legacy SSO profile carries the start URL on the profile itself; the
	// modern form carries it on the referenced [sso-session] block.
	if cfg.SSOSessionName == "" {
		auth.SSOStartURL = cfg.SSOStartURL
	}

	return auth, nil
}

// CheckSSOToken locates and reads the cached SSO token for the profile. It
// returns a zero status for non-SSO profiles.
func CheckSSOToken(auth ProfileAuth) (SSOTokenStatus, error) {
	if !auth.IsSSO() {
		return SSOTokenStatus{}, nil
	}

	// The SDK keys the cache by sso_session name, falling back to the start URL
	// for legacy profiles.
	key := auth.SSOSession
	if key == "" {
		key = auth.SSOStartURL
	}

	path, err := ssocreds.StandardCachedTokenFilepath(key)
	if err != nil {
		return SSOTokenStatus{}, errors.WithStack(err)
	}

	status := SSOTokenStatus{Path: path}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return status, errors.WithStack(err)
	}

	status.Found = true
	status.ExpiresAt = parseTokenExpiry(raw)
	status.CacheWritable = dirWritable(filepath.Dir(path))

	return status, nil
}

// parseTokenExpiry pulls expiresAt out of a cached token file. The token itself
// is deliberately not decoded — only the expiry is needed, and the file holds
// live credentials.
func parseTokenExpiry(raw []byte) time.Time {
	var doc struct {
		ExpiresAt string `json:"expiresAt"`
	}

	if err := json.Unmarshal(raw, &doc); err != nil || doc.ExpiresAt == "" {
		return time.Time{}
	}

	expiresAt, err := time.Parse(time.RFC3339, doc.ExpiresAt)
	if err != nil {
		return time.Time{}
	}

	return expiresAt
}

// dirWritable reports whether a file can be created in dir, which is what the
// SDK does when it caches a refreshed token (temp file + rename).
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".aws-mcp-writecheck-*")
	if err != nil {
		return false
	}

	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)

	return true
}

// PreflightProfile inspects a profile's configuration and logs what it finds:
// which credential mechanism it uses, how long its SSO token is still valid,
// and any condition that will break the server later (unrefreshable token,
// read-only token cache, a credential_process that cannot run here). It returns
// an error only for conditions that are already fatal, so a caller can abort
// before making AWS calls.
func PreflightProfile(ctx context.Context, name string) (ProfileAuth, error) {
	label := name
	if label == "" {
		label = "(default chain)"
	}

	auth, err := InspectProfile(ctx, name)
	if err != nil {
		// A missing profile is only fatal when one was named explicitly; the
		// default chain legitimately works with no config file at all.
		if name == "" {
			log.Debug().Err(err).Msg("[mcpserver.PreflightProfile] no default profile in shared config, relying on the credential chain")
			return auth, nil
		}

		return auth, errors.Wrapf(err, "aws profile %q is not defined in ~/.aws/config", name)
	}

	switch {
	case auth.CredentialProcess != "":
		// credential_process spawns a helper binary. The distroless runtime
		// image has neither a shell nor the helper, so this is the single most
		// common container failure.
		log.Warn().
			Str("profile", label).
			Str("credential_process", auth.CredentialProcess).
			Msg("[mcpserver.PreflightProfile] profile uses credential_process; it needs that binary on PATH and fails in the distroless image (no shell). Prefer a native sso_session profile")

	case auth.IsSSO():
		if auth.IsLegacySSO() {
			log.Warn().
				Str("profile", label).
				Msg("[mcpserver.PreflightProfile] legacy SSO profile (sso_start_url without sso_session): the SDK cannot refresh this token, it will expire mid-run")
		}

		status, err := CheckSSOToken(auth)
		if err != nil {
			log.Warn().Err(err).Str("profile", label).Msg("[mcpserver.PreflightProfile] could not read the cached SSO token")
			break
		}

		if !status.Found {
			return auth, errors.Errorf("aws profile %q has no cached SSO token (%s); run: %s", label, status.Path, auth.LoginHint())
		}

		if status.Expired() {
			return auth, errors.Errorf("aws profile %q has an expired SSO token (expired %s); run: %s",
				label, status.ExpiresAt.Format(time.RFC3339), auth.LoginHint())
		}

		if status.ExpiresAt.IsZero() {
			// A cached token the SDK cannot read an expiry out of is one it will
			// reject on first use, with a parse error nowhere near the cause.
			return auth, errors.Errorf("aws profile %q has a malformed SSO token cache with no readable expiresAt (%s); run: %s",
				label, status.Path, auth.LoginHint())
		}

		event := log.Info()
		if remaining := time.Until(status.ExpiresAt); remaining < ssoTokenExpiryWarning {
			event = log.Warn()
		}

		event.
			Str("profile", label).
			Str("sso_session", auth.SSOSession).
			Time("token_expires_at", status.ExpiresAt).
			Str("token_valid_for", time.Until(status.ExpiresAt).Truncate(time.Second).String()).
			Msg("[mcpserver.PreflightProfile] sso token cached")

		if !status.CacheWritable {
			// The SDK writes a temp file next to the token and renames it, so a
			// read-only mount fails at the first refresh — hours in, long after
			// startup looked healthy.
			log.Warn().
				Str("profile", label).
				Str("cache_dir", filepath.Dir(status.Path)).
				Msg("[mcpserver.PreflightProfile] sso token cache is not writable; refresh will fail when the token expires. Mount it read-write")
		}
	}

	return auth, nil
}

// ExplainCredentialError turns an opaque AWS SDK credential failure into a
// message that names the profile and the command that fixes it.
func ExplainCredentialError(auth ProfileAuth, err error) error {
	if err == nil {
		return nil
	}

	label := auth.Name
	if label == "" {
		label = "(default chain)"
	}

	msg := err.Error()

	switch {
	case containsAny(msg, "cached SSO token is expired", "refresh cached SSO token failed", "unable to refresh SSO token", "InvalidGrantException"):
		hint := auth.LoginHint()
		if hint == "" {
			hint = "aws sso login"
		}
		return errors.Wrapf(err, "aws profile %q: the SSO session has expired and could not be refreshed; run: %s", label, hint)

	case containsAny(msg, "failed to get shared config profile", "SharedConfigProfileNotExist"):
		return errors.Wrapf(err, "aws profile %q is not defined in ~/.aws/config", label)

	case containsAny(msg, `exec: "sh"`, "executable file not found", "error in credential_process"):
		return errors.Wrapf(err, "aws profile %q: credential_process could not be executed; the runtime image has no shell, so use a native sso_session profile or pass credentials as environment variables", label)

	case containsAny(msg, "ExpiredToken", "ExpiredTokenException", "token has expired"):
		return errors.Wrapf(err, "aws profile %q: the credentials have expired; refresh them and restart", label)

	default:
		return errors.WithStack(err)
	}
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}

	return false
}
