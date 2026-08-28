package mcpserver

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// The tools in this file exist because "AWS is not authenticated right now" is a
// legitimate answer to a resource question, and an agent that gets it needs
// somewhere to go. aws_auth_status says what state every profile is in;
// aws_sso_login starts the browser approval and hands back the URL and code.
// Neither touches the resource pipeline, so both answer while AWS is unusable.

// registerAuthTools declares the credential inspection and login tools.
func (s *Server) registerAuthTools() {
	s.mcp.AddTool(
		mcp.NewTool(
			"aws_auth_status",
			mcp.WithDescription("Report the AWS credential state of this server: every configured profile, how it authenticates, which account it serves, whether it is currently usable, its SSO token expiry, and any login in progress. Call this when another tool reports an authentication failure — it says whether a login is needed and which command or URL fixes it, without touching AWS."),
			mcp.WithString("profile",
				mcp.Description("Optional profile name to report on; omit for every configured profile."),
			),
		),
		s.handleAuthStatus,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"aws_sso_login",
			mcp.WithDescription("Start (or pick up) an AWS SSO login for a profile and return the verification URL and user code the user must confirm in a browser. The server polls for approval and caches the token itself, so once the user confirms, retry the original tool — no restart is needed. Relay the URL and code to the user verbatim; approval requires a human."),
			mcp.WithString("profile",
				mcp.Description("Optional profile name to log in; omit to log in every configured SSO profile that needs it."),
			),
		),
		s.handleSSOLogin,
	)
}

// authStatusResult is the aws_auth_status response.
type authStatusResult struct {
	Mode      string             `json:"mode"`
	AutoLogin bool               `json:"auto_login"`
	Profiles  []profileStatusDTO `json:"profiles"`
	Logins    []loginDTO         `json:"logins,omitempty"`
	Note      string             `json:"note,omitempty"`
}

// profileStatusDTO is one profile's credential state.
type profileStatusDTO struct {
	Profile       string    `json:"profile"`
	Mechanism     string    `json:"mechanism"`
	Region        string    `json:"region,omitempty"`
	AccountID     string    `json:"account_id,omitempty"`
	SSOSession    string    `json:"sso_session,omitempty"`
	Authenticated bool      `json:"authenticated"`
	Token         *tokenDTO `json:"sso_token,omitempty"`
	LoginHint     string    `json:"login_hint,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// tokenDTO is the cached SSO token's state, with no secret in it.
type tokenDTO struct {
	Found         bool   `json:"found"`
	Path          string `json:"path,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	ValidFor      string `json:"valid_for,omitempty"`
	Expired       bool   `json:"expired"`
	Refreshable   bool   `json:"refreshable"`
	CacheWritable bool   `json:"cache_writable"`
}

// loginDTO is one device-authorization flow.
type loginDTO struct {
	SSOSession              string `json:"sso_session"`
	State                   string `json:"state"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
	ExpiresAt               string `json:"expires_at,omitempty"`
	TokenExpiresAt          string `json:"token_expires_at,omitempty"`
	Instructions            string `json:"instructions"`
	Error                   string `json:"error,omitempty"`
}

func (s *Server) handleAuthStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	wanted := strings.TrimSpace(req.GetString("profile", ""))

	result := authStatusResult{
		Mode:      "default-chain",
		AutoLogin: s.login.Enabled(),
		Profiles:  []profileStatusDTO{},
	}

	for _, state := range s.profileStates() {
		if wanted != "" && state.Auth.Name != wanted {
			continue
		}

		result.Profiles = append(result.Profiles, profileStatus(state))
	}

	if _, ok := s.pool.(*ProfilePool); ok {
		result.Mode = "profiles"
	}

	if wanted != "" && len(result.Profiles) == 0 {
		return mcp.NewToolResultErrorf("profile %q is not served by this server; call aws_auth_status with no argument for the configured profiles", wanted), nil
	}

	for _, login := range s.login.Logins() {
		result.Logins = append(result.Logins, loginStatus(login))
	}

	if !result.AutoLogin {
		result.Note = "automatic SSO login is disabled (--sso-auto-login=false); aws_sso_login still starts one on request"
	}

	return jsonResult(result)
}

func (s *Server) handleSSOLogin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	wanted := strings.TrimSpace(req.GetString("profile", ""))

	states := s.profileStates()
	logins := []loginDTO{}
	skipped := []string{}
	found := false

	for _, state := range states {
		if wanted != "" && state.Auth.Name != wanted {
			continue
		}

		found = true

		if !state.Auth.IsSSO() {
			skipped = append(skipped, state.Auth.Label()+" (not an SSO profile: "+authMechanism(state.Auth)+")")

			continue
		}

		login, err := s.login.Begin(state.Auth)
		if err != nil {
			skipped = append(skipped, state.Auth.Label()+" ("+err.Error()+")")

			continue
		}

		logins = append(logins, loginStatus(login))
	}

	if wanted != "" && !found {
		return mcp.NewToolResultErrorf("profile %q is not served by this server; call aws_auth_status for the configured profiles", wanted), nil
	}

	if len(logins) == 0 {
		return mcp.NewToolResultErrorf("no SSO login could be started: %s", strings.Join(skipped, "; ")), nil
	}

	return jsonResult(struct {
		Logins  []loginDTO `json:"logins"`
		Skipped []string   `json:"skipped,omitempty"`
		Note    string     `json:"note"`
	}{
		Logins:  logins,
		Skipped: skipped,
		Note:    "relay the verification URL and user code to the user; the server polls for approval and caches the token, so retry the original call afterwards",
	})
}

// profileStates returns the authentication state of every profile this server
// serves. In profile mode the pool knows it; otherwise there is one implicit
// profile — whatever the default credential chain lands on.
func (s *Server) profileStates() []ProfileState {
	if pool, ok := s.pool.(*ProfilePool); ok {
		return pool.Profiles()
	}

	name := os.Getenv("AWS_PROFILE")
	if len(s.profiles) > 0 && s.profiles[0] != "" {
		name = s.profiles[0]
	}

	auth, err := InspectProfile(s.ctx, name)
	auth.Name = name

	state := ProfileState{Auth: auth, Err: err}

	// The lazy pool knows whether the chain has actually resolved; anything else
	// (a test double, a pre-built pool) is reported as usable.
	if lazy, ok := s.pool.(*LazyPool); ok {
		ready, lazyErr := lazy.Ready()
		state.Authenticated = ready

		if lazyErr != nil {
			state.Err = lazyErr
		}
	} else if s.pool != nil {
		state.Authenticated = true
	}

	return []ProfileState{state}
}

// profileStatus renders one profile's state, reading the cached SSO token to
// report expiry. Only local files are touched.
func profileStatus(state ProfileState) profileStatusDTO {
	dto := profileStatusDTO{
		Profile:       state.Auth.Label(),
		Mechanism:     authMechanism(state.Auth),
		Region:        state.Auth.Region,
		AccountID:     state.AccountID,
		SSOSession:    state.Auth.SSOSession,
		Authenticated: state.Authenticated,
		LoginHint:     state.Auth.LoginHint(),
	}

	if state.Err != nil {
		dto.Error = state.Err.Error()
	}

	if dto.AccountID == "" && state.Auth.SSOAccountID != "" {
		dto.AccountID = state.Auth.SSOAccountID
	}

	if !state.Auth.IsSSO() {
		return dto
	}

	status, err := CheckSSOToken(state.Auth)
	if err != nil {
		if dto.Error == "" {
			dto.Error = err.Error()
		}

		return dto
	}

	token := &tokenDTO{
		Found:         status.Found,
		Path:          status.Path,
		Expired:       status.Expired(),
		Refreshable:   status.Refreshable,
		CacheWritable: status.CacheWritable,
	}

	if !status.ExpiresAt.IsZero() {
		token.ExpiresAt = status.ExpiresAt.Format(time.RFC3339)

		if remaining := time.Until(status.ExpiresAt); remaining > 0 {
			token.ValidFor = remaining.Truncate(time.Second).String()
		}
	}

	dto.Token = token

	return dto
}

// authMechanism names how a profile obtains credentials.
func authMechanism(auth ProfileAuth) string {
	switch {
	case auth.IsLegacySSO():
		return "sso-legacy"
	case auth.IsSSO():
		return "sso"
	case auth.CredentialProcess != "":
		return "credential_process"
	default:
		return "static-or-chain"
	}
}

func loginStatus(login *SSOLogin) loginDTO {
	dto := loginDTO{
		SSOSession:              login.Session,
		State:                   string(login.State()),
		VerificationURI:         login.VerificationURI,
		VerificationURIComplete: login.VerificationURIComplete,
		UserCode:                login.UserCode,
		Instructions:            login.Instructions(),
	}

	if !login.ExpiresAt.IsZero() {
		dto.ExpiresAt = login.ExpiresAt.Format(time.RFC3339)
	}

	if expires := login.TokenExpiresAt(); !expires.IsZero() {
		dto.TokenExpiresAt = expires.Format(time.RFC3339)
	}

	if err := login.Err(); err != nil {
		dto.Error = err.Error()
	}

	return dto
}
