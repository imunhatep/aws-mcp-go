package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
)

const testSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"

func forwardedProfile(name, account string, expires time.Time) PassthroughProfile {
	return PassthroughProfile{
		Profile:         name,
		AccountID:       account,
		Region:          "eu-west-1",
		AccessKeyID:     "ASIAEXAMPLE" + name,
		SecretAccessKey: testSecret,
		SessionToken:    "token-" + name,
		Expiration:      expires,
		LoginHint:       "aws sso login --sso-session corp",
	}
}

func encodeDoc(t *testing.T, doc PassthroughCredentials) string {
	t.Helper()

	value, err := EncodePassthroughCredentials(doc)
	require.NoError(t, err)

	return value
}

func TestPassthroughCredentialsRoundTrip(t *testing.T) {
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	doc := PassthroughCredentials{
		Profiles: []PassthroughProfile{forwardedProfile("dev", "111111111111", expires)},
		Errors:   []PassthroughError{{Profile: "prod", AccountID: "222222222222", Error: "sso session expired", LoginHint: "aws sso login --sso-session corp"}},
	}

	decoded, err := DecodePassthroughCredentials(encodeDoc(t, doc))
	require.NoError(t, err)

	assert.Equal(t, PassthroughWireVersion, decoded.Version, "the encoder stamps the version")
	require.Len(t, decoded.Profiles, 1)
	assert.Equal(t, doc.Profiles[0], decoded.Profiles[0])
	assert.Equal(t, doc.Errors, decoded.Errors)
}

func TestDecodePassthroughCredentialsAcceptsStdBase64(t *testing.T) {
	// A hand-written helper piping through coreutils base64 produces the
	// padded standard alphabet; that must work as well as the URL-safe form.
	raw, err := json.Marshal(PassthroughCredentials{
		Version:  PassthroughWireVersion,
		Profiles: []PassthroughProfile{forwardedProfile("dev", "", time.Time{})},
	})
	require.NoError(t, err)

	decoded, err := DecodePassthroughCredentials(base64.StdEncoding.EncodeToString(raw))
	require.NoError(t, err)
	assert.Equal(t, "dev", decoded.Profiles[0].Profile)
}

func TestDecodePassthroughCredentialsRejects(t *testing.T) {
	good := forwardedProfile("dev", "111111111111", time.Time{})

	encodeRaw := func(v any) string {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	noSecret := good
	noSecret.SecretAccessKey = ""

	badAccount := good
	badAccount.AccountID = "1234"

	cases := map[string]struct {
		value string
		want  string
	}{
		"not base64":        {value: "!!!not base64!!!", want: "not valid base64"},
		"not json":          {value: base64.RawURLEncoding.EncodeToString([]byte("secret=" + testSecret)), want: "does not hold a JSON"},
		"future version":    {value: encodeRaw(PassthroughCredentials{Version: 99, Profiles: []PassthroughProfile{good}}), want: "version 99"},
		"empty":             {value: encodeRaw(PassthroughCredentials{Version: 1}), want: "lists no credentials"},
		"missing secret":    {value: encodeRaw(PassthroughCredentials{Version: 1, Profiles: []PassthroughProfile{noSecret}}), want: "missing access_key_id or secret_access_key"},
		"malformed account": {value: encodeRaw(PassthroughCredentials{Version: 1, Profiles: []PassthroughProfile{badAccount}}), want: "malformed account_id"},
		"duplicate profile": {value: encodeRaw(PassthroughCredentials{Version: 1, Profiles: []PassthroughProfile{good, good}}), want: "twice"},
		"oversized":         {value: strings.Repeat("A", maxCredentialsHeaderBytes+1), want: "byte limit"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodePassthroughCredentials(tc.value)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), testSecret, "a decode error must never echo credential material")
		})
	}
}

// The forwarded credentials must be the only ones a passthrough client uses:
// not the server's AWS_ACCESS_KEY_ID, and not its shared config files.
func TestPassthroughConfigIgnoresServerEnvironment(t *testing.T) {
	t.Setenv("AWS_PROFILE", "server-only-profile")
	assert.Equal(t, "server-only-profile", ClearServerProfile())
	assert.Empty(t, os.Getenv("AWS_PROFILE"))

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIASERVERKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "server-secret")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/missing-config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/missing-credentials")

	forwarded := forwardedProfile("dev", "111111111111", time.Time{})

	opts := append(passthroughConfigOptions(&forwarded), awsconfig.WithRegion("us-east-1"))

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	require.NoError(t, err)

	creds, err := cfg.Credentials.Retrieve(context.Background())
	require.NoError(t, err)

	assert.Equal(t, forwarded.AccessKeyID, creds.AccessKeyID)
	assert.Equal(t, forwarded.SessionToken, creds.SessionToken)
}

func requestContext(t *testing.T, value string) context.Context {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if value != "" {
		r.Header.Set(CredentialsHeader, value)
	}

	return withPassthroughRequest(context.Background(), r)
}

func TestClientPoolPassthroughRefusesRequestsWithoutCredentials(t *testing.T) {
	srv := &Server{passthrough: NewPassthroughPools(context.Background())}

	_, err := srv.clientPool(requestContext(t, ""))
	require.ErrorIs(t, err, errNoPassthroughCredentials)

	// No context value at all (a non-HTTP caller) is refused the same way.
	_, err = srv.clientPool(context.Background())
	require.ErrorIs(t, err, errNoPassthroughCredentials)

	_, err = srv.clientPool(requestContext(t, "garbage!!"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid base64")
}

func TestClientPoolPassthroughReusesPoolPerCredentialSet(t *testing.T) {
	pools := NewPassthroughPools(context.Background())
	srv := &Server{passthrough: pools}

	expires := time.Now().Add(time.Hour)
	first := encodeDoc(t, PassthroughCredentials{Profiles: []PassthroughProfile{forwardedProfile("dev", "111111111111", expires)}})
	second := encodeDoc(t, PassthroughCredentials{Profiles: []PassthroughProfile{forwardedProfile("prod", "222222222222", expires)}})

	a, err := srv.clientPool(requestContext(t, first))
	require.NoError(t, err)
	b, err := srv.clientPool(requestContext(t, first))
	require.NoError(t, err)
	c, err := srv.clientPool(requestContext(t, second))
	require.NoError(t, err)

	assert.Same(t, a, b, "the same credentials must reuse the clients built for them")
	assert.NotSame(t, a, c, "different credentials must never share a pool")
	assert.Equal(t, 2, pools.len())

	ids, err := a.ListAccountIDs()
	require.NoError(t, err)
	assert.Equal(t, []ptypes.AwsAccountID{"111111111111"}, ids, "a declared account is answered without AWS")
}

func TestPassthroughPoolsEvictIdleAndExpired(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	pools := NewPassthroughPools(context.Background())
	pools.now = func() time.Time { return now }

	shortLived := &PassthroughCredentials{Version: 1, Profiles: []PassthroughProfile{forwardedProfile("dev", "", now.Add(10*time.Minute))}}
	longLived := &PassthroughCredentials{Version: 1, Profiles: []PassthroughProfile{forwardedProfile("ci", "", time.Time{})}}

	pools.Pool("short", shortLived)
	pools.Pool("long", longLived)
	require.Equal(t, 2, pools.len())

	// Past the short set's expiry but within the idle window: only it goes.
	now = now.Add(15 * time.Minute)
	pools.Pool("long", longLived)
	assert.Equal(t, 1, pools.len())

	// Idle beyond passthroughIdleTTL: long-lived keys are evicted too.
	now = now.Add(passthroughIdleTTL + time.Minute)
	pools.Pool("other", shortLived)
	assert.Equal(t, 1, pools.len(), "only the set just requested remains")
}

func TestPassthroughProfilePoolFailuresAreReportedNotEmpty(t *testing.T) {
	expired := forwardedProfile("dev", "111111111111", time.Now().Add(-time.Minute))

	pool := newPassthroughProfilePool(context.Background(), &PassthroughCredentials{
		Version:  1,
		Profiles: []PassthroughProfile{expired},
		Errors:   []PassthroughError{{Profile: "prod", AccountID: "222222222222", Error: "sso session expired", LoginHint: "aws sso login --sso-session corp"}},
	})

	// Neither entry can authenticate, and neither may reach AWS to find out:
	// the expired one is refused on its timestamp, the failed one is final.
	_, err := pool.GetClients(ptypes.DefaultAwsRegion)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no aws profile could be authenticated")
	assert.Contains(t, err.Error(), "expired at")
	assert.Contains(t, err.Error(), "aws sso login --sso-session corp")

	_, err = pool.GetAccountClients("222222222222", ptypes.DefaultAwsRegion)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account 222222222222 could not be reached")
	assert.Contains(t, err.Error(), "could not export credentials")

	// Both accounts are declared, so an unrelated account is rejected without
	// exercising either set.
	_, err = pool.GetAccountClients("333333333333", ptypes.DefaultAwsRegion)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not reachable by any configured profile")

	states := pool.Profiles()
	require.Len(t, states, 2)

	for _, state := range states {
		assert.True(t, state.Passthrough)
		assert.False(t, state.Authenticated)
		assert.Error(t, state.Err)
		assert.Equal(t, "aws sso login --sso-session corp", state.LoginHint)
	}
}

func TestRefreshExpiredCredentialsAsksForRefreshOnToolCalls(t *testing.T) {
	expired := encodeDoc(t, PassthroughCredentials{Profiles: []PassthroughProfile{forwardedProfile("dev", "", time.Now().Add(30*time.Second))}})
	fresh := encodeDoc(t, PassthroughCredentials{Profiles: []PassthroughProfile{forwardedProfile("dev", "", time.Now().Add(time.Hour))}})
	onlyErrors := encodeDoc(t, PassthroughCredentials{Errors: []PassthroughError{{Profile: "dev", Error: "sso session expired"}}})

	toolCall := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_accounts"}}`
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

	cases := map[string]struct {
		header   string
		body     string
		wantCode int
	}{
		"expired tool call is refreshed":       {header: expired, body: toolCall, wantCode: http.StatusUnauthorized},
		"fresh tool call passes":               {header: fresh, body: toolCall, wantCode: http.StatusOK},
		"handshake is never refused":           {header: expired, body: initialize, wantCode: http.StatusOK},
		"missing header reaches the tool":      {header: "", body: toolCall, wantCode: http.StatusOK},
		"unexportable profiles reach the tool": {header: onlyErrors, body: toolCall, wantCode: http.StatusOK},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var seenBody string

			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				seenBody = string(raw)
				w.WriteHeader(http.StatusOK)
			})

			r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(tc.body))
			if tc.header != "" {
				r.Header.Set(CredentialsHeader, tc.header)
			}

			w := httptest.NewRecorder()
			refreshExpiredCredentials(next).ServeHTTP(w, r)

			assert.Equal(t, tc.wantCode, w.Code)

			if tc.wantCode == http.StatusOK {
				assert.Equal(t, tc.body, seenBody, "peeking at the method must not consume the body")
			} else {
				assert.Contains(t, w.Body.String(), "dev")
				assert.NotContains(t, w.Body.String(), testSecret)
			}
		})
	}
}

// startPassthroughServer runs the real passthrough handler chain and connects
// an MCP client that sends the given credentials header (none when empty).
func startPassthroughServer(t *testing.T, header string) (*client.Client, context.Context) {
	t.Helper()

	srv := NewServer(context.Background(), nil, nil).WithPassthrough(NewPassthroughPools(context.Background()))

	handler, _ := srv.httpHandler()
	mux := http.NewServeMux()
	mux.Handle(mcpEndpointPath, handler)

	testServer := httptest.NewServer(mux)
	t.Cleanup(testServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	opts := []transport.StreamableHTTPCOption{}
	if header != "" {
		opts = append(opts, transport.WithHTTPHeaders(map[string]string{CredentialsHeader: header}))
	}

	c, err := client.NewStreamableHttpClient(testServer.URL+mcpEndpointPath, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	require.NoError(t, c.Start(ctx))

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "1.0.0"}
	_, err = c.Initialize(ctx, initReq)
	require.NoError(t, err)

	return c, ctx
}

func callTool(t *testing.T, c *client.Client, ctx context.Context, name string) (*mcp.CallToolResult, string) {
	t.Helper()

	req := mcp.CallToolRequest{}
	req.Params.Name = name

	res, err := c.CallTool(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)

	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)

	return res, text.Text
}

func TestPassthroughServerWithoutCredentialsExplainsSetup(t *testing.T) {
	c, ctx := startPassthroughServer(t, "")

	res, text := callTool(t, c, ctx, "list_accounts")
	assert.True(t, res.IsError)
	assert.Contains(t, text, "carries no AWS credentials")
	assert.Contains(t, text, "credentials-headers")
}

func TestPassthroughServerReportsForwardedState(t *testing.T) {
	header := encodeDoc(t, PassthroughCredentials{
		Profiles: []PassthroughProfile{forwardedProfile("dev", "111111111111", time.Now().Add(time.Hour))},
		Errors:   []PassthroughError{{Profile: "prod", AccountID: "222222222222", Error: "sso session expired", LoginHint: "aws sso login --sso-session corp"}},
	})

	c, ctx := startPassthroughServer(t, header)

	res, text := callTool(t, c, ctx, "aws_auth_status")
	require.False(t, res.IsError, text)
	assert.NotContains(t, text, testSecret, "auth status must never echo a secret")

	var status authStatusResult
	require.NoError(t, json.Unmarshal([]byte(text), &status))

	assert.Equal(t, "passthrough", status.Mode)
	assert.False(t, status.AutoLogin)
	require.Len(t, status.Profiles, 2)

	byName := map[string]profileStatusDTO{}
	for _, p := range status.Profiles {
		byName[p.Profile] = p
	}

	assert.Equal(t, "passthrough", byName["dev"].Mechanism)
	assert.Equal(t, "111111111111", byName["dev"].AccountID)
	assert.NotEmpty(t, byName["dev"].CredentialsExpireAt)
	assert.NotEmpty(t, byName["dev"].CredentialsValidFor)
	assert.Contains(t, byName["prod"].Error, "could not export credentials")

	res, text = callTool(t, c, ctx, "aws_sso_login")
	assert.True(t, res.IsError)
	assert.Contains(t, text, "credential-passthrough mode")
	assert.Contains(t, text, "aws sso login --sso-session corp")
}
