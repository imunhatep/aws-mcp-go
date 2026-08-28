package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callAuthTool drives one of the auth tools' handlers directly and decodes the
// JSON body it returns.
func callAuthTool(t *testing.T, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) map[string]any {
	t.Helper()

	req := mcp.CallToolRequest{}
	req.Params.Arguments = args

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected tool error: %v", res.Content)
	require.Len(t, res.Content, 1)

	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)

	doc := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &doc))

	return doc
}

// TestAuthStatusReportsExpiredProfile: the tool has to answer while AWS is
// unusable — that is its entire reason to exist — so it reads only local files.
func TestAuthStatusReportsExpiredProfile(t *testing.T) {
	writeSharedConfig(t, `
[sso-session evo]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-central-1

[profile dev]
sso_session = evo
sso_account_id = 111111111111
sso_role_name = readonly
region = eu-central-1
`)
	writeTokenCache(t, "evo", time.Now().Add(-time.Hour))

	srv := NewServer(context.Background(), nil, nil).WithAuth(nil, []string{"dev"})

	doc := callAuthTool(t, srv.handleAuthStatus, nil)

	profiles, ok := doc["profiles"].([]any)
	require.True(t, ok)
	require.Len(t, profiles, 1)

	profile := profiles[0].(map[string]any)
	assert.Equal(t, "dev", profile["profile"])
	assert.Equal(t, "sso", profile["mechanism"])
	assert.Equal(t, "111111111111", profile["account_id"])
	assert.Equal(t, "evo", profile["sso_session"])
	assert.Equal(t, "aws sso login --sso-session evo", profile["login_hint"])

	token := profile["sso_token"].(map[string]any)
	assert.Equal(t, true, token["found"])
	assert.Equal(t, true, token["expired"])
}

// TestAuthStatusRejectsUnknownProfile: answering about a profile the server does
// not serve would be worse than an error — it would read as "that account is
// fine".
func TestAuthStatusRejectsUnknownProfile(t *testing.T) {
	writeSharedConfig(t, "[profile dev]\nregion = eu-central-1\n")

	srv := NewServer(context.Background(), nil, nil).WithAuth(nil, []string{"dev"})

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"profile": "prod"}

	res, err := srv.handleAuthStatus(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

// TestSSOLoginToolReturnsVerificationURL: the tool's output is what the agent
// relays to the human, so the URL and the code must be in it.
func TestSSOLoginToolReturnsVerificationURL(t *testing.T) {
	writeSharedConfig(t, `
[sso-session evo]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-central-1

[profile dev]
sso_session = evo
sso_account_id = 111111111111
sso_role_name = readonly
`)

	oidc := &fakeOIDC{pendingCalls: 1000}
	login := newTestLoginManager(t, oidc)

	srv := NewServer(context.Background(), nil, nil).WithAuth(login, []string{"dev"})

	doc := callAuthTool(t, srv.handleSSOLogin, nil)

	logins, ok := doc["logins"].([]any)
	require.True(t, ok)
	require.Len(t, logins, 1)

	entry := logins[0].(map[string]any)
	assert.Equal(t, "evo", entry["sso_session"])
	assert.Equal(t, "pending", entry["state"])
	assert.Equal(t, "ABCD-EFGH", entry["user_code"])
	assert.Contains(t, entry["verification_uri_complete"], "user_code=ABCD-EFGH")
	assert.Contains(t, entry["instructions"], "retry this call")
}

// TestSSOLoginToolRejectsNonSSOProfile: there is nothing to log into for static
// keys, and saying so beats a device code that would never work.
func TestSSOLoginToolRejectsNonSSOProfile(t *testing.T) {
	writeSharedConfig(t, "[profile dev]\nregion = eu-central-1\n")

	oidc := &fakeOIDC{}
	login := newTestLoginManager(t, oidc)

	srv := NewServer(context.Background(), nil, nil).WithAuth(login, []string{"dev"})

	req := mcp.CallToolRequest{}

	res, err := srv.handleSSOLogin(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, res.IsError)

	register, device, _ := oidc.counts()
	assert.Zero(t, register)
	assert.Zero(t, device)
}
