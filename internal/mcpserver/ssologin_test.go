package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
	ssooidctypes "github.com/aws/aws-sdk-go-v2/service/ssooidc/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// fakeOIDC stands in for the ssooidc client. pendingCalls is how many
// CreateToken calls answer AuthorizationPending before the "user" approves.
type fakeOIDC struct {
	mu sync.Mutex

	registerCalls int
	deviceCalls   int
	tokenCalls    int
	pendingCalls  int
	createErr     error
}

func (f *fakeOIDC) RegisterClient(context.Context, *ssooidc.RegisterClientInput, ...func(*ssooidc.Options)) (*ssooidc.RegisterClientOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.registerCalls++

	return &ssooidc.RegisterClientOutput{
		ClientId:              aws.String("client-id"),
		ClientSecret:          aws.String("client-secret"),
		ClientSecretExpiresAt: time.Now().Add(90 * 24 * time.Hour).Unix(),
	}, nil
}

func (f *fakeOIDC) StartDeviceAuthorization(context.Context, *ssooidc.StartDeviceAuthorizationInput, ...func(*ssooidc.Options)) (*ssooidc.StartDeviceAuthorizationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.deviceCalls++

	return &ssooidc.StartDeviceAuthorizationOutput{
		DeviceCode:              aws.String("device-code"),
		UserCode:                aws.String("ABCD-EFGH"),
		VerificationUri:         aws.String("https://device.sso.eu-central-1.amazonaws.com/"),
		VerificationUriComplete: aws.String("https://device.sso.eu-central-1.amazonaws.com/?user_code=ABCD-EFGH"),
		ExpiresIn:               600,
		Interval:                1,
	}, nil
}

func (f *fakeOIDC) CreateToken(context.Context, *ssooidc.CreateTokenInput, ...func(*ssooidc.Options)) (*ssooidc.CreateTokenOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tokenCalls++

	if f.createErr != nil {
		return nil, f.createErr
	}

	if f.tokenCalls <= f.pendingCalls {
		return nil, &ssooidctypes.AuthorizationPendingException{Message: aws.String("pending")}
	}

	return &ssooidc.CreateTokenOutput{
		AccessToken:  aws.String("access-token"),
		RefreshToken: aws.String("refresh-token"),
		ExpiresIn:    28800,
		TokenType:    aws.String("Bearer"),
	}, nil
}

func (f *fakeOIDC) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.registerCalls, f.deviceCalls, f.tokenCalls
}

// newTestLoginManager wires a manager onto a fake OIDC service and an isolated
// HOME, so the token cache it writes is a temp directory rather than the
// developer's real ~/.aws/sso/cache.
func newTestLoginManager(t *testing.T, oidc *fakeOIDC) *SSOLoginManager {
	t.Helper()

	t.Setenv("HOME", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	m := NewSSOLoginManager(ctx, true, false)
	m.newClient = func(string) (ssoOIDCAPI, error) { return oidc, nil }

	return m
}

func testSSOAuth() ProfileAuth {
	return ProfileAuth{
		Name:        "dev",
		SSOSession:  "evo",
		SSOStartURL: "https://example.awsapps.com/start",
		SSORegion:   "eu-central-1",
	}
}

// TestSSOLoginDeviceFlowCachesToken is the whole feature end to end: the flow
// starts, reports a URL and code an agent can relay, and once the "user"
// approves, the token lands where the SDK and the AWS CLI both look for it — which
// is what lets a running server recover without a restart.
func TestSSOLoginDeviceFlowCachesToken(t *testing.T) {
	oidc := &fakeOIDC{pendingCalls: 1}
	m := newTestLoginManager(t, oidc)

	login, err := m.Begin(testSSOAuth())
	require.NoError(t, err)

	assert.Equal(t, LoginPending, login.State())
	assert.Equal(t, "ABCD-EFGH", login.UserCode)
	assert.Contains(t, login.VerificationURIComplete, "user_code=ABCD-EFGH")
	assert.Contains(t, login.Instructions(), "ABCD-EFGH")

	require.Eventually(t, func() bool { return login.State() == LoginCompleted }, 15*time.Second, 100*time.Millisecond)

	raw, err := os.ReadFile(login.TokenPath)
	require.NoError(t, err)

	doc := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &doc))

	assert.Equal(t, "access-token", doc["accessToken"])
	assert.Equal(t, "refresh-token", doc["refreshToken"])
	assert.Equal(t, "client-id", doc["clientId"])
	assert.Equal(t, "https://example.awsapps.com/start", doc["startUrl"])
	assert.Equal(t, "eu-central-1", doc["region"])

	// The real proof: the preflight — the same code the credential chain's health
	// is judged by — now sees a usable, refreshable token.
	status, err := CheckSSOToken(testSSOAuth())
	require.NoError(t, err)

	assert.True(t, status.Found)
	assert.False(t, status.Expired())
	assert.True(t, status.Refreshable)

	info, err := os.Stat(login.TokenPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the token file holds live credentials")
}

// TestSSOLoginReusesPendingFlow: several profiles usually share one sso-session,
// and a fan-out across them must not send the user a different code per profile.
func TestSSOLoginReusesPendingFlow(t *testing.T) {
	oidc := &fakeOIDC{pendingCalls: 1000}
	m := newTestLoginManager(t, oidc)

	first, err := m.Begin(testSSOAuth())
	require.NoError(t, err)

	second, err := m.Begin(ProfileAuth{
		Name:        "prod",
		SSOSession:  "evo",
		SSOStartURL: "https://example.awsapps.com/start",
		SSORegion:   "eu-central-1",
	})
	require.NoError(t, err)

	assert.Same(t, first, second)

	register, device, _ := oidc.counts()
	assert.Equal(t, 1, register)
	assert.Equal(t, 1, device)
}

// TestSSOLoginReusesCachedRegistration: re-registering the OIDC client would
// invalidate the refresh token cached next to it, turning every expiry into a
// full re-approval.
func TestSSOLoginReusesCachedRegistration(t *testing.T) {
	oidc := &fakeOIDC{pendingCalls: 1000}
	m := newTestLoginManager(t, oidc)

	auth := testSSOAuth()

	path, err := ssoTokenPath(auth)
	require.NoError(t, err)

	require.NoError(t, writeSSOTokenCache(path, map[string]any{
		"clientId":              "cached-id",
		"clientSecret":          "cached-secret",
		"registrationExpiresAt": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}))

	_, err = m.Begin(auth)
	require.NoError(t, err)

	register, device, _ := oidc.counts()
	assert.Zero(t, register, "a valid cached registration must be reused")
	assert.Equal(t, 1, device)
}

// TestExplainOnlyLoginsForCredentialFailures: a region an account has not
// enabled, or a missing IAM grant, is not something a browser login fixes.
// Starting one anyway would send the user chasing an unrelated URL.
func TestExplainOnlyLoginsForCredentialFailures(t *testing.T) {
	oidc := &fakeOIDC{pendingCalls: 1000}
	m := newTestLoginManager(t, oidc)

	unrelated := m.Explain(testSSOAuth(), errors.New("AccessDenied: not authorized to perform ec2:DescribeInstances"))
	require.Error(t, unrelated)
	assert.NotContains(t, unrelated.Error(), "ABCD-EFGH")

	register, device, _ := oidc.counts()
	assert.Zero(t, register)
	assert.Zero(t, device)

	expired := m.Explain(testSSOAuth(), errors.New("get identity: get credentials: failed to refresh cached credentials, cached SSO token is expired"))
	require.Error(t, expired)

	// The failure is still reported — it just now carries the way out of it.
	assert.Contains(t, expired.Error(), "expired")
	assert.Contains(t, expired.Error(), "ABCD-EFGH")
	assert.Contains(t, expired.Error(), "aws sso login --sso-session evo")
}

// TestExplainWithAutoLoginDisabled: the flag governs automatic starts only, and
// the error must still say what to run by hand.
func TestExplainWithAutoLoginDisabled(t *testing.T) {
	oidc := &fakeOIDC{pendingCalls: 1000}
	m := newTestLoginManager(t, oidc)
	m.enabled = false

	err := m.Explain(testSSOAuth(), errors.New("cached SSO token is expired"))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "aws sso login --sso-session evo")
	assert.NotContains(t, err.Error(), "ABCD-EFGH")

	register, _, _ := oidc.counts()
	assert.Zero(t, register)

	// An explicit request still works — that is the aws_sso_login tool's path.
	login, beginErr := m.Begin(testSSOAuth())
	require.NoError(t, beginErr)
	assert.Equal(t, LoginPending, login.State())
}

// TestNilLoginManagerIsUsable: a server built without a login manager (as the
// tests and any embedder do) must still explain credential errors.
func TestNilLoginManagerIsUsable(t *testing.T) {
	var m *SSOLoginManager

	assert.False(t, m.Enabled())
	assert.Nil(t, m.Logins())

	err := m.Explain(testSSOAuth(), errors.New("cached SSO token is expired"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aws sso login --sso-session evo")

	_, beginErr := m.Begin(testSSOAuth())
	assert.Error(t, beginErr)
}
