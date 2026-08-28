package command

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"

	"github.com/imunhatep/aws-mcp-go/internal/config"
	"github.com/imunhatep/aws-mcp-go/internal/mcpserver"
)

func TestParseProfiles(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr string
	}{
		{name: "single", raw: "dev", want: []string{"dev"}},
		{name: "multiple with spaces", raw: "dev, prod ,sandbox", want: []string{"dev", "prod", "sandbox"}},
		{name: "empty entries dropped", raw: "dev,,prod,", want: []string{"dev", "prod"}},
		{name: "duplicate rejected", raw: "dev,prod,dev", wantErr: "duplicate aws profile"},
		{name: "no names", raw: " , ", wantErr: "no profile names"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProfiles(tc.raw)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// writeSharedConfig points the SDK's shared-config loader at a temp file holding
// the given INI content.
func writeSharedConfig(t *testing.T, content string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	t.Setenv("AWS_CONFIG_FILE", path)
	t.Setenv("HOME", t.TempDir())
}

// TestBuildClientPoolSurvivesBrokenProfile is the regression this whole change
// exists for: a profile with no usable SSO login used to abort `serve`, which
// exits non-zero, which leaves the calling agent with a refused connection
// instead of an explanation. Startup must succeed regardless.
func TestBuildClientPoolSurvivesBrokenProfile(t *testing.T) {
	writeSharedConfig(t, `
[sso-session evo]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-central-1

[profile dev]
sso_session = evo
sso_account_id = 111111111111
sso_role_name = readonly
`)

	cfg := &config.Config{Addr: ":3040", Profiles: "dev"}

	pool, err := buildClientPool(context.Background(), cfg, nil)
	require.NoError(t, err, "an unusable profile must not keep the server from starting")
	require.NotNil(t, pool)

	// ...and the failure is reported to whoever asks, rather than looking like an
	// empty account. No AWS call is involved: there is no cached token to use.
	_, err = pool.GetClients(ptypes.DefaultAwsRegion)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev")
	assert.Contains(t, err.Error(), "aws sso login --sso-session evo")
}

// TestBuildClientPoolDefersLocalMode: local and assume-role modes are wrapped in
// a lazy pool, so no credential resolution happens during startup either.
func TestBuildClientPoolDefersLocalMode(t *testing.T) {
	writeSharedConfig(t, "[profile dev]\nregion = eu-central-1\n")

	pool, err := buildClientPool(context.Background(), &config.Config{Addr: ":3040"}, nil)
	require.NoError(t, err)

	assert.IsType(t, &mcpserver.LazyPool{}, pool)

	ready, lastErr := pool.(*mcpserver.LazyPool).Ready()
	assert.False(t, ready, "credentials must not be resolved before the first tool call")
	assert.NoError(t, lastErr)
}

// TestBuildClientPoolStillRejectsBadFlags: operator mistakes cannot fix
// themselves, so they stay fatal.
func TestBuildClientPoolStillRejectsBadFlags(t *testing.T) {
	writeSharedConfig(t, "[profile dev]\nregion = eu-central-1\n")

	_, err := buildClientPool(context.Background(), &config.Config{Addr: ":3040", AssumeRoleArns: "not-an-arn"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid role ARN")

	_, err = buildClientPool(context.Background(), &config.Config{Addr: ":3040", Profiles: "dev,dev"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate aws profile")
}
