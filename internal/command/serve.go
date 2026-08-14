package command

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/imunhatep/awslib/cache"
	"github.com/imunhatep/awslib/provider"
	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"

	"github.com/imunhatep/aws-mcp-go/internal/config"
	"github.com/imunhatep/aws-mcp-go/internal/mcpserver"
	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// ServeCommand runs the MCP streamable-HTTP server.
type ServeCommand struct{}

func (c ServeCommand) Command() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "run the AWS MCP server (streamable HTTP, served at /mcp)",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "addr",
				Usage:   "listen address for the HTTP server (MCP served at /mcp)",
				Value:   ":3040",
				Sources: cli.EnvVars("MCP_ADDR"),
			},
			&cli.DurationFlag{
				Name:    "cache-ttl",
				Usage:   "TTL for cached AWS resource listings (e.g. 6h, 30m)",
				Value:   mcpserver.DefaultCacheTTL,
				Sources: cli.EnvVars("MCP_CACHE_TTL"),
			},
			&cli.StringFlag{
				Name:    "cache-dir",
				Usage:   "directory for the on-disk resource cache; empty to use in-memory cache only",
				Value:   os.TempDir(),
				Sources: cli.EnvVars("MCP_CACHE_DIR"),
			},
			&cli.BoolFlag{
				Name:  "no-cache",
				Usage: "disable caching entirely",
			},
			&cli.BoolFlag{
				Name:  "assume-role",
				Usage: "enable cross-account mode by auto-discovering assumable roles from the current IAM role's policies",
			},
			&cli.StringFlag{
				Name:    "assume-role-arns",
				Usage:   "comma-separated assumable role ARNs (e.g. arn:aws:iam::111:role/r,arn:aws:iam::222:role/r or accountID=arn); enables cross-account mode and overrides auto-discovery",
				Sources: cli.EnvVars("MCP_ASSUME_ROLE_ARNS"),
			},
		},
		Action: c.run,
	}
}

func (c ServeCommand) run(ctx context.Context, cmd *cli.Command) error {
	cfg := config.FromCommand(cmd)
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	providers, err := v3.DefaultAwsClientProviders()
	if err != nil {
		return errors.WithStack(err)
	}

	builder := v3.NewClientBuilder(ctx, providers...)

	// Validate credentials up front and make the active identity visible, so a
	// misconfigured or missing AWS credential chain fails loudly at startup
	// rather than on the first tool call.
	if err := logCallerIdentity(ctx, builder); err != nil {
		return err
	}

	pool, err := buildClientPool(ctx, builder, cfg)
	if err != nil {
		return err
	}

	dc, err := buildCache(ctx, cfg)
	if err != nil {
		return err
	}

	srv := mcpserver.NewServer(ctx, pool, dc)

	return srv.ServeHTTP(ctx, cfg.Addr)
}

// logCallerIdentity resolves the default (base) credentials' STS caller
// identity and logs the account, ARN and user ID. It returns an error if the
// identity cannot be resolved, so startup aborts with a clear message when the
// AWS credential chain is missing or invalid. In assume-role mode this reports
// the base principal that role assumption chains off of.
func logCallerIdentity(ctx context.Context, builder *v3.ClientBuilder) error {
	client, err := builder.DefaultClient()
	if err != nil {
		return errors.Wrap(err, "resolving AWS credentials failed; check your AWS credential chain (profile / SSO / env / IMDS)")
	}

	id, err := client.GetCallerIdentity(ctx)
	if err != nil {
		return errors.Wrap(err, "STS GetCallerIdentity failed; AWS credentials are missing, expired or invalid")
	}

	log.Info().
		Str("account", aws.ToString(id.Account)).
		Str("arn", aws.ToString(id.Arn)).
		Str("user_id", aws.ToString(id.UserId)).
		Str("region", client.GetRegion().String()).
		Msg("[serve] AWS caller identity resolved")

	return nil
}

// buildClientPool assembles the provider client pool. In local / single-account
// mode it uses the default credential chain; in assume-role mode it either uses
// explicit role ARNs or auto-discovers them from the current IAM role.
func buildClientPool(ctx context.Context, builder *v3.ClientBuilder, cfg *config.Config) (mcpserver.ClientPool, error) {
	// Local / single-account mode: default credentials (AWS SSO, env, IMDS).
	if !cfg.AssumeRole && cfg.AssumeRoleArns == "" {
		log.Info().Msg("[serve] using local credentials (aws profile / sso / env)")
		return provider.NewClientPool(ctx, builder), nil
	}

	// Cross-account assume-role mode.
	var roles map[ptypes.AwsAccountID]ptypes.RoleArn
	var err error

	if cfg.AssumeRoleArns != "" {
		roles, err = parseRoleArns(cfg.AssumeRoleArns)
		if err != nil {
			return nil, err
		}
		log.Info().Int("roles", len(roles)).Msg("[serve] using explicit assumable roles")
	} else {
		defaultClient, err := builder.DefaultClient()
		if err != nil {
			return nil, errors.WithStack(err)
		}

		roles, err = provider.DiscoverAssumableRolesFromCurrentRole(ctx, defaultClient)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		log.Info().Int("roles", len(roles)).Msg("[serve] auto-discovered assumable roles from current IAM role")
	}

	if len(roles) == 0 {
		return nil, errors.New("assume-role mode enabled but no assumable roles were found")
	}

	return v3.NewClientPool(ctx, builder, roles), nil
}

// parseRoleArns parses a comma-separated list of role specs. Each entry is
// either "accountID=roleArn" or a bare "roleArn" (account ID is then derived
// from the ARN).
func parseRoleArns(raw string) (map[ptypes.AwsAccountID]ptypes.RoleArn, error) {
	roles := map[ptypes.AwsAccountID]ptypes.RoleArn{}

	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		var accountID, roleArn string
		if account, arnStr, ok := strings.Cut(entry, "="); ok {
			accountID = strings.TrimSpace(account)
			roleArn = strings.TrimSpace(arnStr)
		} else {
			roleArn = entry
			parsed, err := arn.Parse(roleArn)
			if err != nil {
				return nil, errors.Errorf("invalid role ARN %q: %s", roleArn, err)
			}
			accountID = parsed.AccountID
		}

		if accountID == "" || roleArn == "" {
			return nil, errors.Errorf("invalid role spec %q; expected accountID=roleArn or a role ARN", entry)
		}

		roles[ptypes.AwsAccountID(accountID)] = ptypes.RoleArn(roleArn)
	}

	return roles, nil
}

// buildCache builds the resource cache, or returns nil when caching is disabled.
func buildCache(ctx context.Context, cfg *config.Config) (*cache.DataCache, error) {
	if cfg.NoCache {
		log.Info().Msg("[serve] caching disabled")
		return nil, nil
	}

	return mcpserver.NewCache(ctx, cfg.CacheTTL, cfg.CacheDir)
}
