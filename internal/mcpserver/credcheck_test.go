package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// writeSharedConfig points the SDK's shared-config loader at a temp file
// holding the given INI content.
func writeSharedConfig(t *testing.T, content string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	t.Setenv("AWS_CONFIG_FILE", path)
}

// writeTokenCache creates $HOME/.aws/sso/cache/<sha1(session)>.json with the
// given expiry, mirroring what `aws sso login` leaves behind, and returns the
// cache directory.
func writeTokenCache(t *testing.T, session string, expiresAt time.Time) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".aws", "sso", "cache")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	path, err := CheckSSOToken(ProfileAuth{SSOSession: session})
	require.NoError(t, err)

	body := `{"accessToken":"secret","expiresAt":"` + expiresAt.UTC().Format(time.RFC3339) + `","refreshToken":"r"}`
	require.NoError(t, os.WriteFile(path.Path, []byte(body), 0o600))

	return dir
}

func TestInspectProfile(t *testing.T) {
	writeSharedConfig(t, `
[sso-session evo]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-central-1

[profile modern]
sso_session = evo
sso_account_id = 111111111111
sso_role_name = readonly
region = eu-central-1

[profile legacy]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-central-1
sso_account_id = 222222222222
sso_role_name = readonly

[profile helper]
credential_process = aws-sso process --arn arn:aws:iam::333333333333:role/readonly

[profile plain]
region = eu-west-1
`)

	ctx := context.Background()

	t.Run("modern sso", func(t *testing.T) {
		auth, err := InspectProfile(ctx, "modern")
		require.NoError(t, err)

		assert.True(t, auth.IsSSO())
		assert.False(t, auth.IsLegacySSO())
		assert.Equal(t, "evo", auth.SSOSession)
		assert.Equal(t, "111111111111", auth.SSOAccountID)
		assert.Equal(t, "aws sso login --sso-session evo", auth.LoginHint())
	})

	t.Run("legacy sso", func(t *testing.T) {
		auth, err := InspectProfile(ctx, "legacy")
		require.NoError(t, err)

		assert.True(t, auth.IsSSO())
		assert.True(t, auth.IsLegacySSO(), "no sso_session means the SDK cannot refresh")
		assert.Equal(t, "aws sso login --profile legacy", auth.LoginHint())
	})

	t.Run("credential process", func(t *testing.T) {
		auth, err := InspectProfile(ctx, "helper")
		require.NoError(t, err)

		assert.False(t, auth.IsSSO())
		assert.Contains(t, auth.CredentialProcess, "aws-sso process")
		assert.Empty(t, auth.LoginHint())
	})

	t.Run("plain", func(t *testing.T) {
		auth, err := InspectProfile(ctx, "plain")
		require.NoError(t, err)

		assert.False(t, auth.IsSSO())
		assert.Equal(t, "eu-west-1", auth.Region)
	})

	t.Run("undefined", func(t *testing.T) {
		_, err := InspectProfile(ctx, "nope")
		require.Error(t, err)
	})
}

func TestCheckSSOToken(t *testing.T) {
	t.Run("non-sso profile", func(t *testing.T) {
		status, err := CheckSSOToken(ProfileAuth{Name: "plain"})
		require.NoError(t, err)

		assert.False(t, status.Found)
		assert.Empty(t, status.Path)
	})

	t.Run("valid token", func(t *testing.T) {
		expiry := time.Now().Add(4 * time.Hour)
		writeTokenCache(t, "evo", expiry)

		status, err := CheckSSOToken(ProfileAuth{SSOSession: "evo"})
		require.NoError(t, err)

		assert.True(t, status.Found)
		assert.False(t, status.Expired())
		assert.True(t, status.CacheWritable)
		assert.WithinDuration(t, expiry, status.ExpiresAt, time.Second)
	})

	t.Run("expired token", func(t *testing.T) {
		writeTokenCache(t, "evo", time.Now().Add(-time.Hour))

		status, err := CheckSSOToken(ProfileAuth{SSOSession: "evo"})
		require.NoError(t, err)

		assert.True(t, status.Found)
		assert.True(t, status.Expired())
	})

	t.Run("read-only cache dir", func(t *testing.T) {
		dir := writeTokenCache(t, "evo", time.Now().Add(4*time.Hour))
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		status, err := CheckSSOToken(ProfileAuth{SSOSession: "evo"})
		require.NoError(t, err)

		// The SDK writes a temp file into this directory and renames it, so a
		// read-only mount breaks refresh even though the token reads fine.
		assert.True(t, status.Found)
		assert.False(t, status.CacheWritable)
	})

	t.Run("no login yet", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())

		status, err := CheckSSOToken(ProfileAuth{SSOSession: "evo"})
		require.NoError(t, err)

		assert.False(t, status.Found)
		assert.NotEmpty(t, status.Path)
	})
}

func TestPreflightProfileFailsOnExpiredSSO(t *testing.T) {
	writeSharedConfig(t, `
[sso-session evo]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-central-1

[profile dev]
sso_session = evo
sso_account_id = 111111111111
sso_role_name = readonly
`)
	writeTokenCache(t, "evo", time.Now().Add(-time.Hour))

	_, err := PreflightProfile(context.Background(), "dev")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired SSO token")
	assert.Contains(t, err.Error(), "aws sso login --sso-session evo")
}

func TestPreflightProfileUndefinedProfile(t *testing.T) {
	writeSharedConfig(t, "[profile other]\nregion = eu-central-1\n")

	_, err := PreflightProfile(context.Background(), "dev")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `aws profile "dev" is not defined`)
}

func TestExplainCredentialError(t *testing.T) {
	sso := ProfileAuth{Name: "dev", SSOSession: "evo"}

	tests := []struct {
		name string
		auth ProfileAuth
		err  error
		want []string
	}{
		{
			name: "expired sso token",
			auth: sso,
			err:  errors.New("refresh cached SSO token failed, cached SSO token is expired, or not present, and cannot be refreshed"),
			want: []string{`"dev"`, "aws sso login --sso-session evo"},
		},
		{
			name: "missing profile",
			auth: ProfileAuth{Name: "ghost"},
			err:  errors.New("failed to get shared config profile, ghost"),
			want: []string{`"ghost"`, "not defined in ~/.aws/config"},
		},
		{
			name: "credential process without a shell",
			auth: ProfileAuth{Name: "helper", CredentialProcess: "aws-sso process"},
			err:  errors.New(`error in credential_process: exec: "sh": executable file not found in $PATH`),
			want: []string{`"helper"`, "no shell"},
		},
		{
			name: "expired static credentials",
			auth: ProfileAuth{Name: "envcreds"},
			err:  errors.New("operation error STS: GetCallerIdentity, api error ExpiredToken: token expired"),
			want: []string{`"envcreds"`, "expired"},
		},
		{
			name: "unrecognised error passes through",
			auth: sso,
			err:  errors.New("dial tcp: lookup sts.amazonaws.com: no such host"),
			want: []string{"no such host"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExplainCredentialError(tc.auth, tc.err)

			require.Error(t, got)
			for _, want := range tc.want {
				assert.Contains(t, got.Error(), want)
			}
			assert.ErrorIs(t, got, tc.err, "the original error must stay unwrappable")
		})
	}

	t.Run("nil stays nil", func(t *testing.T) {
		assert.NoError(t, ExplainCredentialError(sso, nil))
	})
}

func TestPreflightProfileRejectsMalformedTokenCache(t *testing.T) {
	writeSharedConfig(t, `
[sso-session evo]
sso_start_url = https://example.awsapps.com/start
sso_region = eu-central-1

[profile dev]
sso_session = evo
sso_account_id = 111111111111
sso_role_name = readonly
`)

	// A token file the SDK cannot read an expiry out of: without this check the
	// server starts and fails later with an opaque RFC3339 parse error.
	dir := writeTokenCache(t, "evo", time.Now().Add(time.Hour))
	status, err := CheckSSOToken(ProfileAuth{SSOSession: "evo"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(status.Path, []byte(`{"accessToken":"x"}`), 0o600))
	require.DirExists(t, dir)

	_, err = PreflightProfile(context.Background(), "dev")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed SSO token cache")
	assert.Contains(t, err.Error(), "aws sso login --sso-session evo")
}
