package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
	ssooidctypes "github.com/aws/aws-sdk-go-v2/service/ssooidc/types"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// The OIDC device-authorization flow this file implements is the same one the
// AWS CLI runs for `aws sso login`, done in-process so no CLI, no shell and no
// browser is needed on the machine running the server: the verification URL and
// user code are handed back to the calling agent, which shows them to the human.
//
// Once approved, the resulting token is written to the SDK's standard cache path
// (~/.aws/sso/cache/<sha1(session)>.json) in the same JSON shape the CLI writes,
// so the credential chain — and the CLI itself — picks it up with no restart.
// See docs/authentication.md.
const (
	// ssoClientName is what shows up in the IAM Identity Center device list.
	ssoClientName = "aws-mcp"
	// ssoClientType must be "public" for the device flow (no client secret the
	// user could keep).
	ssoClientType = "public"
	// ssoDefaultScope is what the AWS CLI registers with when the sso-session
	// declares no sso_registration_scopes; it is what GetRoleCredentials needs.
	ssoDefaultScope = "sso:account:access"
	// ssoDeviceGrantType is the RFC 8628 grant type.
	ssoDeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// ssoPollInterval is used when the service does not supply one.
	ssoPollInterval = 5 * time.Second
	// ssoSlowDownIncrement is the RFC 8628-recommended backoff step when the
	// service answers SlowDown.
	ssoSlowDownIncrement = 5 * time.Second
	// ssoCompletedGrace is how long a completed login stays visible, so a tool
	// call that raced the approval reports "authenticated, retry" rather than
	// starting a second flow.
	ssoCompletedGrace = 2 * time.Minute
	// ssoTokenFileMode matches the SDK's own token cache permissions.
	ssoTokenFileMode = 0o600
)

// LoginState is the lifecycle of one device-authorization flow.
type LoginState string

const (
	// LoginPending means the user has not approved the request yet.
	LoginPending LoginState = "pending"
	// LoginCompleted means a token was obtained and cached.
	LoginCompleted LoginState = "completed"
	// LoginExpired means the user code expired unapproved.
	LoginExpired LoginState = "expired"
	// LoginFailed means the flow errored out.
	LoginFailed LoginState = "failed"
)

// ssoOIDCAPI is the subset of the ssooidc client the device flow uses. It is an
// interface so the flow is testable without contacting AWS.
type ssoOIDCAPI interface {
	RegisterClient(context.Context, *ssooidc.RegisterClientInput, ...func(*ssooidc.Options)) (*ssooidc.RegisterClientOutput, error)
	StartDeviceAuthorization(context.Context, *ssooidc.StartDeviceAuthorizationInput, ...func(*ssooidc.Options)) (*ssooidc.StartDeviceAuthorizationOutput, error)
	CreateToken(context.Context, *ssooidc.CreateTokenInput, ...func(*ssooidc.Options)) (*ssooidc.CreateTokenOutput, error)
}

// SSOLogin is one in-flight (or recently finished) device authorization.
//
// The device code is deliberately unexported: it is the bearer of the pending
// authorization and must not reach a tool result.
type SSOLogin struct {
	// Session is the sso_session name, or the start URL for a legacy profile.
	Session string
	// StartURL / Region identify the IAM Identity Center instance.
	StartURL string
	Region   string
	// VerificationURI is the plain URL the user opens; the Complete form has the
	// user code pre-filled, which is the one worth showing first.
	VerificationURI         string
	VerificationURIComplete string
	// UserCode is what the user confirms in the browser.
	UserCode string
	// ExpiresAt is when the user code stops being accepted.
	ExpiresAt time.Time
	// TokenPath is where the resulting token is cached.
	TokenPath string

	deviceCode            string
	clientID              string
	clientSecret          string
	interval              time.Duration
	registrationExpiresAt time.Time

	mu             sync.Mutex
	state          LoginState
	err            error
	tokenExpiresAt time.Time
	finishedAt     time.Time
}

// State reports the current lifecycle state.
func (l *SSOLogin) State() LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.state
}

// Err returns why the flow failed, if it did.
func (l *SSOLogin) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.err
}

// TokenExpiresAt is the expiry of the token a completed flow cached.
func (l *SSOLogin) TokenExpiresAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.tokenExpiresAt
}

func (l *SSOLogin) setState(state LoginState, err error, tokenExpiresAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.state = state
	l.err = err
	l.tokenExpiresAt = tokenExpiresAt
	l.finishedAt = time.Now()
}

// reusable reports whether this login can be handed to another caller instead of
// starting a second flow. A pending code that is still accepted obviously can;
// so can a just-completed one, so a caller that raced the approval is told to
// retry rather than sent to the browser again.
func (l *SSOLogin) reusable() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case LoginPending:
		return time.Now().Before(l.ExpiresAt.Add(-l.interval))
	case LoginCompleted:
		return time.Since(l.finishedAt) < ssoCompletedGrace
	default:
		return false
	}
}

// Instructions is the agent-facing next step for this login. It is written to be
// relayed verbatim to a human: the URL and the code are the whole payload.
func (l *SSOLogin) Instructions() string {
	switch l.State() {
	case LoginCompleted:
		return fmt.Sprintf("aws sso login completed for session %q (token valid until %s); retry this call",
			l.Session, l.TokenExpiresAt().Format(time.RFC3339))

	case LoginPending:
		url := l.VerificationURIComplete
		if url == "" {
			url = l.VerificationURI
		}

		return fmt.Sprintf("aws sso login started for session %q: ask the user to open %s and confirm code %s (valid for %s). "+
			"The server is polling and caches the token itself — retry this call once the user has approved; no restart is needed",
			l.Session, url, l.UserCode, time.Until(l.ExpiresAt).Truncate(time.Second))

	case LoginExpired:
		return fmt.Sprintf("the aws sso login request for session %q expired unapproved; call aws_sso_login to start a new one", l.Session)

	default:
		return fmt.Sprintf("the aws sso login attempt for session %q failed: %v", l.Session, l.Err())
	}
}

// SSOLoginManager starts and tracks device-authorization flows, at most one per
// sso-session at a time.
//
// It is safe to use a nil *SSOLoginManager: every method degrades to "no login
// was started", which is what a server with auto-login switched off wants.
type SSOLoginManager struct {
	ctx         context.Context
	enabled     bool
	openBrowser bool
	newClient   func(region string) (ssoOIDCAPI, error)

	mu      sync.Mutex
	pending map[string]*SSOLogin
}

// NewSSOLoginManager builds a login manager bound to the server's lifetime. ctx
// must outlive individual tool calls: approval takes as long as the human takes,
// so polling cannot hang off a request context.
func NewSSOLoginManager(ctx context.Context, enabled, openBrowser bool) *SSOLoginManager {
	return &SSOLoginManager{
		ctx:         ctx,
		enabled:     enabled,
		openBrowser: openBrowser,
		newClient:   defaultSSOOIDCClient,
		pending:     map[string]*SSOLogin{},
	}
}

func defaultSSOOIDCClient(region string) (ssoOIDCAPI, error) {
	// The OIDC endpoint is unauthenticated (device flow), so this deliberately
	// does not load any profile or credentials — only the region matters.
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}),
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return ssooidc.NewFromConfig(cfg), nil
}

// Enabled reports whether auto-login is switched on.
func (m *SSOLoginManager) Enabled() bool {
	return m != nil && m.enabled
}

// Logins returns the tracked flows, for the auth-status tool.
func (m *SSOLoginManager) Logins() []*SSOLogin {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	logins := make([]*SSOLogin, 0, len(m.pending))
	for _, login := range m.pending {
		logins = append(logins, login)
	}

	return logins
}

// Begin starts a device-authorization flow for the profile's sso-session, or
// returns the one already running for it.
//
// This is the explicit path (the aws_sso_login tool) and works regardless of
// --sso-auto-login: that flag governs whether a *failing* tool call starts a
// login by itself, not whether the operator may ask for one.
func (m *SSOLoginManager) Begin(auth ProfileAuth) (*SSOLogin, error) {
	if m == nil {
		return nil, errors.New("sso login is not available on this server")
	}

	if !auth.IsSSO() {
		return nil, errors.Errorf("aws profile %q does not use AWS SSO, so there is no login to start", auth.Label())
	}

	if auth.SSOStartURL == "" || auth.SSORegion == "" {
		return nil, errors.Errorf("aws profile %q has an incomplete SSO configuration (start url %q, region %q); fix ~/.aws/config",
			auth.Label(), auth.SSOStartURL, auth.SSORegion)
	}

	key := auth.SSOCacheKey()

	m.mu.Lock()
	defer m.mu.Unlock()

	if login, ok := m.pending[key]; ok && login.reusable() {
		return login, nil
	}

	login, err := m.start(auth)
	if err != nil {
		return nil, err
	}

	m.pending[key] = login

	go m.poll(login)

	log.Info().
		Str("sso_session", login.Session).
		Str("verification_uri", login.VerificationURIComplete).
		Str("user_code", login.UserCode).
		Time("expires_at", login.ExpiresAt).
		Msg("[SSOLoginManager] aws sso device authorization started; open the url and confirm the code")

	m.maybeOpenBrowser(login)

	return login, nil
}

// start performs the two synchronous legs of the flow: client registration and
// the device-authorization request.
func (m *SSOLoginManager) start(auth ProfileAuth) (*SSOLogin, error) {
	client, err := m.newClient(auth.SSORegion)
	if err != nil {
		return nil, errors.Wrap(err, "building the sso oidc client failed")
	}

	tokenPath, err := ssoTokenPath(auth)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	clientID, clientSecret, registrationExpiresAt, err := m.registration(ctx, client, auth, tokenPath)
	if err != nil {
		return nil, err
	}

	device, err := client.StartDeviceAuthorization(ctx, &ssooidc.StartDeviceAuthorizationInput{
		ClientId:     aws.String(clientID),
		ClientSecret: aws.String(clientSecret),
		StartUrl:     aws.String(auth.SSOStartURL),
	})
	if err != nil {
		return nil, errors.Wrap(err, "starting the sso device authorization failed")
	}

	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = ssoPollInterval
	}

	expiresIn := time.Duration(device.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 10 * time.Minute
	}

	return &SSOLogin{
		Session:                 auth.SSOSessionKeyName(),
		StartURL:                auth.SSOStartURL,
		Region:                  auth.SSORegion,
		VerificationURI:         aws.ToString(device.VerificationUri),
		VerificationURIComplete: aws.ToString(device.VerificationUriComplete),
		UserCode:                aws.ToString(device.UserCode),
		ExpiresAt:               time.Now().Add(expiresIn),
		TokenPath:               tokenPath,
		deviceCode:              aws.ToString(device.DeviceCode),
		clientID:                clientID,
		clientSecret:            clientSecret,
		interval:                interval,
		state:                   LoginPending,
		// Carried so a completed flow can write a CLI-compatible cache file.
		registrationExpiresAt: registrationExpiresAt,
	}, nil
}

// registration reuses the clientId/clientSecret already in the token cache when
// that registration is still valid, and registers a new client otherwise.
//
// Reuse matters: a fresh registration invalidates the refresh token stored
// alongside the old one, so re-registering on every login would turn every
// expiry into a full re-approval.
func (m *SSOLoginManager) registration(ctx context.Context, client ssoOIDCAPI, auth ProfileAuth, tokenPath string) (string, string, time.Time, error) {
	if cached, err := readSSOTokenCache(tokenPath); err == nil {
		expiresAt := parseCacheTime(cached["registrationExpiresAt"])
		id, _ := cached["clientId"].(string)
		secret, _ := cached["clientSecret"].(string)

		if id != "" && secret != "" && !expiresAt.IsZero() && time.Now().Before(expiresAt) {
			log.Debug().Str("sso_session", auth.SSOSessionKeyName()).Msg("[SSOLoginManager] reusing the cached oidc client registration")
			return id, secret, expiresAt, nil
		}
	}

	out, err := client.RegisterClient(ctx, &ssooidc.RegisterClientInput{
		ClientName: aws.String(ssoClientName),
		ClientType: aws.String(ssoClientType),
		Scopes:     ssoRegistrationScopes(auth),
	})
	if err != nil {
		return "", "", time.Time{}, errors.Wrap(err, "registering an sso oidc client failed")
	}

	return aws.ToString(out.ClientId), aws.ToString(out.ClientSecret), time.Unix(out.ClientSecretExpiresAt, 0), nil
}

// poll waits for the user to approve the request, then caches the token.
func (m *SSOLoginManager) poll(login *SSOLogin) {
	interval := login.interval
	deadline := time.NewTimer(time.Until(login.ExpiresAt))
	defer deadline.Stop()

	for {
		wait := time.NewTimer(interval)

		select {
		case <-m.ctx.Done():
			wait.Stop()
			login.setState(LoginFailed, errors.New("server is shutting down"), time.Time{})

			return

		case <-deadline.C:
			wait.Stop()
			login.setState(LoginExpired, nil, time.Time{})
			log.Warn().Str("sso_session", login.Session).Msg("[SSOLoginManager] aws sso device authorization expired unapproved")

			return

		case <-wait.C:
		}

		out, err := m.createToken(login)

		switch {
		case err == nil:
			expiresAt, werr := m.storeToken(login, out)
			if werr != nil {
				login.setState(LoginFailed, werr, time.Time{})
				log.Error().Err(werr).Str("sso_session", login.Session).Msg("[SSOLoginManager] caching the sso token failed")

				return
			}

			login.setState(LoginCompleted, nil, expiresAt)
			log.Info().
				Str("sso_session", login.Session).
				Str("token_path", login.TokenPath).
				Time("token_expires_at", expiresAt).
				Msg("[SSOLoginManager] aws sso login completed, token cached")

			return

		case isAuthorizationPending(err):
			continue

		case isSlowDown(err):
			interval += ssoSlowDownIncrement
			log.Debug().Str("sso_session", login.Session).Dur("interval", interval).Msg("[SSOLoginManager] sso oidc asked to slow down")

		case isExpiredDeviceCode(err):
			login.setState(LoginExpired, nil, time.Time{})
			log.Warn().Str("sso_session", login.Session).Msg("[SSOLoginManager] aws sso device code expired")

			return

		default:
			login.setState(LoginFailed, errors.WithStack(err), time.Time{})
			log.Error().Err(err).Str("sso_session", login.Session).Msg("[SSOLoginManager] aws sso login failed")

			return
		}
	}
}

func (m *SSOLoginManager) createToken(login *SSOLogin) (*ssooidc.CreateTokenOutput, error) {
	client, err := m.newClient(login.Region)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	return client.CreateToken(ctx, &ssooidc.CreateTokenInput{
		ClientId:     aws.String(login.clientID),
		ClientSecret: aws.String(login.clientSecret),
		GrantType:    aws.String(ssoDeviceGrantType),
		DeviceCode:   aws.String(login.deviceCode),
	})
}

// storeToken writes the SSO token where the SDK and the AWS CLI both look for
// it, preserving the fields the CLI adds so the two stay interchangeable.
func (m *SSOLoginManager) storeToken(login *SSOLogin, out *ssooidc.CreateTokenOutput) (time.Time, error) {
	expiresIn := time.Duration(out.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 8 * time.Hour
	}

	expiresAt := time.Now().Add(expiresIn)

	// Start from whatever is already cached so unknown fields the CLI wrote
	// survive; the SDK preserves them the same way.
	doc, err := readSSOTokenCache(login.TokenPath)
	if err != nil {
		doc = map[string]any{}
	}

	doc["startUrl"] = login.StartURL
	doc["region"] = login.Region
	doc["clientId"] = login.clientID
	doc["clientSecret"] = login.clientSecret
	doc["accessToken"] = aws.ToString(out.AccessToken)
	doc["expiresAt"] = expiresAt.UTC().Format(time.RFC3339)

	if refresh := aws.ToString(out.RefreshToken); refresh != "" {
		doc["refreshToken"] = refresh
	}

	if !login.registrationExpiresAt.IsZero() {
		doc["registrationExpiresAt"] = login.registrationExpiresAt.UTC().Format(time.RFC3339)
	}

	if err := writeSSOTokenCache(login.TokenPath, doc); err != nil {
		return time.Time{}, err
	}

	return expiresAt, nil
}

// Explain turns a credential failure into a tool-facing message and, when
// auto-login is on and the profile is SSO-backed, kicks off (or picks up) a
// device authorization so the message can carry the URL and code.
//
// The failure itself is always reported: an agent must never be left thinking
// the account is empty when it is only unauthenticated.
func (m *SSOLoginManager) Explain(auth ProfileAuth, err error) error {
	if err == nil {
		return nil
	}

	explained := ExplainCredentialError(auth, err)

	// Only a credential failure justifies sending someone to a browser: a region
	// an account has not enabled, a missing IAM grant or a throttle all arrive
	// here too, and a login would fix none of them.
	if !m.Enabled() || !auth.IsSSO() || !IsCredentialFailure(err) {
		return explained
	}

	login, beginErr := m.Begin(auth)
	if beginErr != nil {
		log.Warn().Err(beginErr).Str("profile", auth.Label()).Msg("[SSOLoginManager.Explain] could not start an sso login")

		return explained
	}

	return errors.Errorf("%w | %s", explained, login.Instructions())
}

// maybeOpenBrowser opens the verification URL when the server is running on a
// desktop. It is best-effort by design: in a container there is no browser (and
// usually no user at the machine at all), which is exactly why the URL is also
// returned to the calling agent.
func (m *SSOLoginManager) maybeOpenBrowser(login *SSOLogin) {
	if !m.openBrowser {
		return
	}

	url := login.VerificationURIComplete
	if url == "" {
		url = login.VerificationURI
	}

	if url == "" {
		return
	}

	if inContainer() {
		log.Debug().Msg("[SSOLoginManager] running in a container, not opening a browser")
		return
	}

	var opener string

	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		opener = "xdg-open"
	default:
		return
	}

	path, err := exec.LookPath(opener)
	if err != nil {
		log.Debug().Str("opener", opener).Msg("[SSOLoginManager] no browser opener on PATH")
		return
	}

	if err := exec.Command(path, url).Start(); err != nil {
		log.Debug().Err(err).Msg("[SSOLoginManager] opening a browser failed")
	}
}

// inContainer reports whether this process looks containerised, using the
// markers podman and docker leave behind.
func inContainer() bool {
	if os.Getenv("container") != "" {
		return true
	}

	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}

	return false
}

// ssoRegistrationScopes returns the scopes to register with: whatever the
// sso-session declares, else the CLI's default.
func ssoRegistrationScopes(auth ProfileAuth) []string {
	if len(auth.SSORegistrationScopes) > 0 {
		return auth.SSORegistrationScopes
	}

	return []string{ssoDefaultScope}
}

func readSSOTokenCache(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errors.WithStack(err)
	}

	return doc, nil
}

// writeTokenCache writes the token atomically: a temp file in the same directory
// followed by a rename, which is what the SDK does — and why the *directory*,
// not just the file, has to be writable.
func writeSSOTokenCache(path string, doc map[string]any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.WithStack(err)
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return errors.WithStack(err)
	}

	tmp, err := os.CreateTemp(dir, ".aws-mcp-token-*")
	if err != nil {
		return errors.Wrapf(err, "the sso token cache directory %s is not writable", dir)
	}

	name := tmp.Name()

	if err := tmp.Chmod(ssoTokenFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)

		return errors.WithStack(err)
	}

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)

		return errors.WithStack(err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)

		return errors.WithStack(err)
	}

	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)

		return errors.WithStack(err)
	}

	return nil
}

func parseCacheTime(value any) time.Time {
	text, ok := value.(string)
	if !ok || text == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}

	return parsed
}

func isAuthorizationPending(err error) bool {
	var target *ssooidctypes.AuthorizationPendingException

	return errors.As(err, &target)
}

func isSlowDown(err error) bool {
	var target *ssooidctypes.SlowDownException

	return errors.As(err, &target)
}

func isExpiredDeviceCode(err error) bool {
	var expired *ssooidctypes.ExpiredTokenException
	if errors.As(err, &expired) {
		return true
	}

	var invalid *ssooidctypes.InvalidGrantException

	return errors.As(err, &invalid)
}

// readSSORegistrationScopes pulls sso_registration_scopes out of the shared
// config, which the SDK parses for its own use but does not expose. Registering
// with narrower scopes than the session declares yields a token the session's
// other consumers cannot use.
func readSSORegistrationScopes(configFile, session string) []string {
	file, err := os.Open(configFile)
	if err != nil {
		return nil
	}
	defer file.Close()

	want := "sso-session " + session
	scanner := bufio.NewScanner(file)
	inSection := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = strings.TrimSpace(strings.Trim(line, "[]")) == want
			continue
		}

		if !inSection {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "sso_registration_scopes" {
			continue
		}

		scopes := []string{}
		for scope := range strings.SplitSeq(value, ",") {
			if scope = strings.TrimSpace(scope); scope != "" {
				scopes = append(scopes, scope)
			}
		}

		return scopes
	}

	return nil
}
