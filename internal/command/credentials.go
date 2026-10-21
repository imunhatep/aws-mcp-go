package command

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/imunhatep/aws-mcp-go/internal/mcpserver"
	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// Credential sources for credentials-headers.
const (
	// sourceSDK resolves each profile in-process with the AWS SDK. It reads
	// the same ~/.aws/config and the same SSO token cache `aws sso login`
	// writes, refreshes the token the same way, and needs no aws binary.
	sourceSDK = "sdk"
	// sourceAWSCLI shells out to `aws configure export-credentials`, for
	// setups only the AWS CLI v2 itself resolves correctly.
	sourceAWSCLI = "aws-cli"
)

// exportConcurrency bounds parallel profile exports. The AWS CLI is a Python
// process per profile, so 25 profiles run serially would blow through the
// 10-second budget Claude Code gives a headersHelper.
const exportConcurrency = 8

// CredentialsHeadersCommand prints the credential header a passthrough server
// expects, in the JSON shape an MCP headersHelper must write to stdout.
type CredentialsHeadersCommand struct{}

func (c CredentialsHeadersCommand) Command() *cli.Command {
	return &cli.Command{
		Name:  "credentials-headers",
		Usage: "resolve local AWS profiles and print them as the " + mcpserver.CredentialsHeader + " header for a --credentials-passthrough server (use as the MCP headersHelper)",
		Description: "Runs on the MCP client machine. Each profile is resolved from the local ~/.aws/config and SSO cache (log in first with `aws sso login`), " +
			"and the result is written to stdout as {\"" + mcpserver.CredentialsHeader + "\": \"...\"}. A profile that cannot be resolved is forwarded as an error " +
			"entry rather than failing the helper, so the server can name it and the login that fixes it. Secrets are never written to stderr.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "profiles",
				Usage:   "comma-separated AWS shared-config profiles to forward; empty forwards the default credential chain (AWS_PROFILE or default)",
				Sources: cli.EnvVars("MCP_AWS_PROFILES"),
			},
			&cli.StringFlag{
				Name:    "source",
				Usage:   "how credentials are resolved: sdk (in-process, reads the AWS CLI v2 SSO cache) or aws-cli (runs aws configure export-credentials)",
				Value:   sourceSDK,
				Sources: cli.EnvVars("MCP_CREDENTIALS_SOURCE"),
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "overall deadline; Claude Code kills a headersHelper after 10s",
				Value: 8 * time.Second,
			},
		},
		Action: c.run,
	}
}

func (c CredentialsHeadersCommand) run(ctx context.Context, cmd *cli.Command) error {
	var export exportFunc

	switch source := cmd.String("source"); source {
	case sourceSDK:
		export = exportWithSDK
	case sourceAWSCLI:
		export = exportWithAWSCLI(runAWSCLI)
	default:
		return errors.Errorf("unknown --source %q; use %s or %s", source, sourceSDK, sourceAWSCLI)
	}

	profiles := []string{""}

	if raw := cmd.String("profiles"); raw != "" {
		parsed, err := parseProfiles(raw)
		if err != nil {
			return err
		}

		profiles = parsed
	}

	ctx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()

	doc := exportProfiles(ctx, profiles, export)

	return writeHeaders(os.Stdout, doc)
}

// exportFunc resolves one profile ("" for the default chain) to credentials.
type exportFunc func(ctx context.Context, profile string) (mcpserver.PassthroughProfile, error)

// exportProfiles resolves every profile concurrently. A failure becomes an
// error entry, never a helper failure: a helper that exits non-zero leaves the
// client with no server at all, and every working profile with it.
func exportProfiles(ctx context.Context, profiles []string, export exportFunc) mcpserver.PassthroughCredentials {
	type result struct {
		profile mcpserver.PassthroughProfile
		err     error
	}

	results := make([]result, len(profiles))
	sem := make(chan struct{}, exportConcurrency)

	var wg sync.WaitGroup

	for i, name := range profiles {
		wg.Add(1)

		go func() {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			profile, err := export(ctx, name)
			results[i] = result{profile: profile, err: err}
		}()
	}

	wg.Wait()

	doc := mcpserver.PassthroughCredentials{Version: mcpserver.PassthroughWireVersion, Profiles: []mcpserver.PassthroughProfile{}}

	for i, res := range results {
		label := profiles[i]
		if label == "" {
			label = defaultProfileName()
		}

		if res.err != nil {
			// Only the explanation goes to stderr — never a credential.
			log.Warn().Err(res.err).Str("profile", label).Msg("[credentials-headers] profile not exported")

			doc.Errors = append(doc.Errors, mcpserver.PassthroughError{
				Profile:   label,
				AccountID: res.profile.AccountID,
				Error:     res.err.Error(),
				LoginHint: res.profile.LoginHint,
			})

			continue
		}

		res.profile.Profile = label
		doc.Profiles = append(doc.Profiles, res.profile)
	}

	return doc
}

// writeHeaders writes the headersHelper output: a flat JSON object of headers.
func writeHeaders(w io.Writer, doc mcpserver.PassthroughCredentials) error {
	value, err := mcpserver.EncodePassthroughCredentials(doc)
	if err != nil {
		return err
	}

	return json.NewEncoder(w).Encode(map[string]string{mcpserver.CredentialsHeader: value})
}

// defaultProfileName is what the default chain resolves to, for labels.
func defaultProfileName() string {
	if profile := os.Getenv("AWS_PROFILE"); profile != "" {
		return profile
	}

	return "default"
}

// profileMetadata reads what the shared config says about a profile — account,
// region and the login command — with no AWS call. A failed preflight is
// returned with whatever metadata it did find, so even an error entry carries
// the account it was meant to serve.
func profileMetadata(ctx context.Context, profile string) (mcpserver.PassthroughProfile, mcpserver.ProfileAuth, error) {
	auth, err := mcpserver.PreflightProfile(ctx, profile)

	meta := mcpserver.PassthroughProfile{
		AccountID: auth.SSOAccountID,
		Region:    auth.Region,
		LoginHint: auth.LoginHint(),
	}

	return meta, auth, err
}

// exportWithSDK resolves a profile in-process. It reads the same SSO token cache
// the AWS CLI v2 writes, so `aws sso login` is all the user ever runs.
func exportWithSDK(ctx context.Context, profile string) (mcpserver.PassthroughProfile, error) {
	meta, auth, err := profileMetadata(ctx, profile)
	if err != nil {
		return meta, err
	}

	opts := []func(*awsconfig.LoadOptions) error{}
	if profile != "" {
		// An explicit profile outranks AWS_ACCESS_KEY_ID in the environment, so
		// every profile resolves to its own identity.
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return meta, mcpserver.ExplainCredentialError(auth, err)
	}

	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return meta, mcpserver.ExplainCredentialError(auth, err)
	}

	meta.AccessKeyID = creds.AccessKeyID
	meta.SecretAccessKey = creds.SecretAccessKey
	meta.SessionToken = creds.SessionToken

	if creds.AccountID != "" {
		meta.AccountID = creds.AccountID
	}

	if cfg.Region != "" {
		meta.Region = cfg.Region
	}

	if creds.CanExpire {
		meta.Expiration = creds.Expires.UTC()
	}

	return meta, nil
}

// cliCredentials is the `--format process` output of export-credentials.
type cliCredentials struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
	AccountID       string `json:"AccountId"`
}

// cliRunner runs the aws binary and returns its stdout.
type cliRunner func(ctx context.Context, args ...string) ([]byte, error)

// runAWSCLI runs `aws` from PATH. stderr is captured for the error message: it
// is where the CLI explains an expired SSO session, and it holds no secrets.
func runAWSCLI(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "aws", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}

		return nil, errors.Errorf("aws configure export-credentials failed: %s", msg)
	}

	return stdout.Bytes(), nil
}

// exportWithAWSCLI resolves a profile with `aws configure export-credentials`.
func exportWithAWSCLI(run cliRunner) exportFunc {
	return func(ctx context.Context, profile string) (mcpserver.PassthroughProfile, error) {
		// Preflight failures are not fatal here: the CLI resolves some setups
		// the shared-config parser does not, so it gets the final word.
		meta, _, _ := profileMetadata(ctx, profile)

		args := []string{"configure", "export-credentials", "--format", "process"}
		if profile != "" {
			args = append(args, "--profile", profile)
		}

		out, err := run(ctx, args...)
		if err != nil {
			return meta, err
		}

		var creds cliCredentials
		if err := json.Unmarshal(out, &creds); err != nil {
			return meta, errors.New("aws configure export-credentials printed something other than process-format JSON")
		}

		if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
			return meta, errors.New("aws configure export-credentials returned no access key")
		}

		meta.AccessKeyID = creds.AccessKeyID
		meta.SecretAccessKey = creds.SecretAccessKey
		meta.SessionToken = creds.SessionToken

		if creds.AccountID != "" {
			meta.AccountID = creds.AccountID
		}

		if creds.Expiration != "" {
			expires, err := time.Parse(time.RFC3339, creds.Expiration)
			if err != nil {
				return meta, errors.Errorf("aws configure export-credentials returned an unparseable Expiration %q", creds.Expiration)
			}

			meta.Expiration = expires.UTC()
		}

		return meta, nil
	}
}
