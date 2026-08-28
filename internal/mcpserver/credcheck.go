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
	// SSOStartURL and SSORegion identify the IAM Identity Center instance,
	// whether they come from the [sso-session] block or (legacy form) from the
	// profile itself. Both are what the device-authorization flow logs in
	// against, so they are always populated for an SSO profile.
	SSOStartURL string
	SSORegion   string
	// SSORegistrationScopes is the sso_registration_scopes of the sso-session,
	// if it declares any; the OIDC client is registered with these.
	SSORegistrationScopes []string
	// CredentialProcess is the configured credential_process command, if any.
	CredentialProcess string
}

// Label is the profile name for messages, naming the default chain explicitly
// rather than showing an empty string.
func (a ProfileAuth) Label() string {
	if a.Name == "" {
		return "(default chain)"
	}

	return a.Name
}

// SSOCacheKey is what the SDK hashes to find the cached token: the sso_session
// name, or the start URL for a legacy profile.
func (a ProfileAuth) SSOCacheKey() string {
	if a.SSOSession != "" {
		return a.SSOSession
	}

	return a.SSOStartURL
}

// SSOSessionKeyName is a display name for the SSO session behind this profile.
func (a ProfileAuth) SSOSessionKeyName() string {
	if a.SSOSession != "" {
		return a.SSOSession
	}

	return a.SSOStartURL
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
	// Refreshable reports whether the cache holds a refresh token and its client
	// registration. An expired access token with those present is not a problem:
	// the SDK trades the refresh token for a new one on next use, with no human
	// involved. Without them, expiry means a fresh browser approval.
	Refreshable bool
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

	configFile := env.SharedConfigFile
	if configFile == "" && len(awsconfig.DefaultSharedConfigFiles) > 0 {
		configFile = awsconfig.DefaultSharedConfigFiles[0]
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

	// A legacy SSO profile carries the start URL and region on the profile
	// itself; the modern form carries them on the referenced [sso-session]
	// block, which is also where the registration scopes live. The SDK parses
	// that block but exposes no scopes, so they are read from the file directly.
	switch {
	case cfg.SSOSession != nil:
		auth.SSOStartURL = cfg.SSOSession.SSOStartURL
		auth.SSORegion = cfg.SSOSession.SSORegion
		auth.SSORegistrationScopes = readSSORegistrationScopes(configFile, cfg.SSOSessionName)

	default:
		auth.SSOStartURL = cfg.SSOStartURL
		auth.SSORegion = cfg.SSORegion
	}

	return auth, nil
}

// ssoTokenPath is where the SDK caches this profile's SSO token: a sha1 of the
// sso_session name, or of the start URL for a legacy profile. Both the preflight
// and the device-authorization flow resolve it through here, so a login always
// writes the file the credential chain then reads.
func ssoTokenPath(auth ProfileAuth) (string, error) {
	path, err := ssocreds.StandardCachedTokenFilepath(auth.SSOCacheKey())
	if err != nil {
		return "", errors.WithStack(err)
	}

	return path, nil
}

// CheckSSOToken locates and reads the cached SSO token for the profile. It
// returns a zero status for non-SSO profiles.
func CheckSSOToken(auth ProfileAuth) (SSOTokenStatus, error) {
	if !auth.IsSSO() {
		return SSOTokenStatus{}, nil
	}

	path, err := ssoTokenPath(auth)
	if err != nil {
		return SSOTokenStatus{}, err
	}

	status := SSOTokenStatus{Path: path}

	// Writability is reported even when no token exists yet: that is exactly the
	// case where a login is about to create the file, and a read-only mount is
	// the reason it will fail.
	status.CacheWritable = dirWritable(filepath.Dir(path))

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return status, errors.WithStack(err)
	}

	status.Found = true
	status.ExpiresAt, status.Refreshable = parseTokenExpiry(raw)

	return status, nil
}

// parseTokenExpiry pulls the expiry out of a cached token file, and whether the
// cache carries everything the SDK needs to refresh itself. The secrets are
// deliberately not returned — only their presence is interesting, and the file
// holds live credentials.
func parseTokenExpiry(raw []byte) (time.Time, bool) {
	var doc struct {
		ExpiresAt    string `json:"expiresAt"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}

	if err := json.Unmarshal(raw, &doc); err != nil {
		return time.Time{}, false
	}

	refreshable := doc.RefreshToken != "" && doc.ClientID != "" && doc.ClientSecret != ""

	if doc.ExpiresAt == "" {
		return time.Time{}, refreshable
	}

	expiresAt, err := time.Parse(time.RFC3339, doc.ExpiresAt)
	if err != nil {
		return time.Time{}, refreshable
	}

	return expiresAt, refreshable
}

// dirWritable reports whether a file can be created in dir, which is what the
// SDK does when it caches a refreshed token (temp file + rename).
//
// A missing directory is judged by its nearest existing ancestor: before the
// first login ~/.aws/sso/cache does not exist, and whether it can be created is
// the question that matters — reporting "not writable" for it would point at the
// wrong problem.
func dirWritable(dir string) bool {
	probe := dir

	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}

		parent := filepath.Dir(probe)
		if parent == probe {
			return false
		}

		probe = parent
	}

	f, err := os.CreateTemp(probe, ".aws-mcp-writecheck-*")
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
			// An expired access token is only a problem when the cache cannot
			// renew itself. With a refresh token and client registration present
			// the SDK trades them for a new access token on first use, so
			// aborting here would demand a browser approval nobody needs.
			if !status.Refreshable {
				return auth, errors.Errorf("aws profile %q has an expired SSO token that cannot be refreshed (expired %s); run: %s",
					label, status.ExpiresAt.Format(time.RFC3339), auth.LoginHint())
			}

			log.Warn().
				Str("profile", label).
				Time("token_expired_at", status.ExpiresAt).
				Msg("[mcpserver.PreflightProfile] sso access token expired; the sdk will refresh it from the cached refresh token on first use")

			break
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

// credentialFailureMarkers are the substrings that identify a failure a login
// would fix. They cover both the SDK's wording and this package's own preflight
// messages.
var credentialFailureMarkers = []string{
	"cached SSO token is expired",
	"refresh cached SSO token failed",
	"unable to refresh SSO token",
	"InvalidGrantException",
	"no cached SSO token",
	"expired SSO token",
	"malformed SSO token cache",
	"ExpiredToken",
	"token has expired",
	"failed to refresh cached credentials",
	"failed to retrieve credentials",
	"UnrecognizedClientException",
	"InvalidClientTokenId",
}

// IsCredentialFailure reports whether an error is the kind a re-login fixes.
//
// It gates starting a device-authorization flow: an AWS call can fail for plenty
// of reasons that have nothing to do with credentials — a region the account has
// not enabled, a missing IAM grant, a throttle — and sending the user to a
// browser for those would be noise at best.
func IsCredentialFailure(err error) bool {
	if err == nil {
		return false
	}

	return containsAny(err.Error(), credentialFailureMarkers...)
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
