package command

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imunhatep/aws-mcp-go/internal/mcpserver"
	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// isolateAWSConfig points the shared config at an empty temp dir, so profile
// metadata lookups never read the developer's real ~/.aws.
func isolateAWSConfig(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", dir+"/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", dir+"/credentials")
	t.Setenv("AWS_PROFILE", "")
}

func TestExportProfilesForwardsFailuresAsErrors(t *testing.T) {
	isolateAWSConfig(t)

	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	export := func(_ context.Context, profile string) (mcpserver.PassthroughProfile, error) {
		if profile == "prod" {
			return mcpserver.PassthroughProfile{AccountID: "222222222222", LoginHint: "aws sso login --sso-session corp"}, errors.New("sso session expired")
		}

		return mcpserver.PassthroughProfile{AccessKeyID: "ASIADEV", SecretAccessKey: "secret", Expiration: expires}, nil
	}

	doc := exportProfiles(context.Background(), []string{"dev", "prod"}, export)

	require.Len(t, doc.Profiles, 1)
	assert.Equal(t, "dev", doc.Profiles[0].Profile, "the label is the profile name")
	assert.Equal(t, expires, doc.Profiles[0].Expiration)

	require.Len(t, doc.Errors, 1)
	assert.Equal(t, mcpserver.PassthroughError{
		Profile:   "prod",
		AccountID: "222222222222",
		Error:     "sso session expired",
		LoginHint: "aws sso login --sso-session corp",
	}, doc.Errors[0])
}

func TestWriteHeadersIsHeadersHelperOutput(t *testing.T) {
	doc := mcpserver.PassthroughCredentials{
		Profiles: []mcpserver.PassthroughProfile{{Profile: "dev", AccessKeyID: "ASIADEV", SecretAccessKey: "secret"}},
	}

	var out bytes.Buffer
	require.NoError(t, writeHeaders(&out, doc))

	// A headersHelper must print a flat JSON object of string headers.
	headers := map[string]string{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &headers))
	require.Len(t, headers, 1)

	decoded, err := mcpserver.DecodePassthroughCredentials(headers[mcpserver.CredentialsHeader])
	require.NoError(t, err)
	assert.Equal(t, "ASIADEV", decoded.Profiles[0].AccessKeyID)
}

func TestExportWithAWSCLI(t *testing.T) {
	isolateAWSConfig(t)

	var gotArgs []string

	run := func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"Version":1,"AccessKeyId":"ASIACLI","SecretAccessKey":"secret","SessionToken":"tok","Expiration":"2030-01-01T00:00:00+00:00","AccountId":"111111111111"}`), nil
	}

	profile, err := exportWithAWSCLI(run)(context.Background(), "dev")
	require.NoError(t, err)

	assert.Equal(t, "configure export-credentials --format process --profile dev", strings.Join(gotArgs, " "))
	assert.Equal(t, "ASIACLI", profile.AccessKeyID)
	assert.Equal(t, "tok", profile.SessionToken)
	assert.Equal(t, "111111111111", profile.AccountID)
	assert.Equal(t, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), profile.Expiration)
}

func TestExportWithAWSCLIFailures(t *testing.T) {
	isolateAWSConfig(t)

	cases := map[string]struct {
		out  string
		err  error
		want string
	}{
		"cli error":      {err: errors.New("aws configure export-credentials failed: Error loading SSO Token"), want: "Error loading SSO Token"},
		"not json":       {out: "export AWS_ACCESS_KEY_ID=x", want: "process-format JSON"},
		"no key":         {out: `{"Version":1}`, want: "no access key"},
		"bad expiration": {out: `{"Version":1,"AccessKeyId":"A","SecretAccessKey":"S","Expiration":"soon"}`, want: "unparseable Expiration"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run := func(context.Context, ...string) ([]byte, error) { return []byte(tc.out), tc.err }

			_, err := exportWithAWSCLI(run)(context.Background(), "dev")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestExportWithSDKReportsUndefinedProfile(t *testing.T) {
	isolateAWSConfig(t)

	_, err := exportWithSDK(context.Background(), "does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}
