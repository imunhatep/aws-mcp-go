package mcpserver

import (
	"context"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/awslib/provider"
	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// regionClientPool is the per-profile pool contract ProfilePool builds on.
// provider.ClientPool satisfies it; the indirection keeps ProfilePool unit
// testable without AWS credentials.
type regionClientPool interface {
	GetClients(regions ...ptypes.AwsRegion) ([]*v3.Client, error)
}

// profileEntry is one AWS shared-config profile and the client pool serving it.
type profileEntry struct {
	name      string
	accountID ptypes.AwsAccountID
	pool      regionClientPool
}

// ProfilePool is a ClientPool that fans out across several AWS shared-config
// profiles, one account per profile. Each profile gets its own credential chain
// and its own provider.ClientPool, which caches clients per region.
type ProfilePool struct {
	entries []profileEntry
}

// NewProfilePool resolves each named profile's identity up front — a bad or
// expired profile fails here rather than on the first tool call — and wraps it
// in a per-profile client pool. Region clients themselves are still created
// lazily on first use.
func NewProfilePool(ctx context.Context, profiles []string) (*ProfilePool, error) {
	entries := make([]profileEntry, 0, len(profiles))

	for _, name := range profiles {
		// Build the credential chain from the named profile only. Note this
		// deliberately does not go through v3.DefaultAwsClientProviders: that
		// helper folds in ambient AWS_ACCESS_KEY_ID / AWS_PROFILE env
		// credentials, which would override the profile and collapse every
		// profile onto the same identity.
		providers := []func(*awsconfig.LoadOptions) error{
			awsconfig.WithRetryMaxAttempts(v3.AwsRetryAttempts),
			awsconfig.WithRetryer(func() aws.Retryer {
				return retry.AddWithMaxBackoffDelay(retry.NewStandard(), v3.AwsRetryMaxBackoffDelay)
			}),
			awsconfig.WithSharedConfigProfile(name),
		}

		builder := v3.NewClientBuilder(ctx, providers...)

		client, err := builder.DefaultClient()
		if err != nil {
			return nil, errors.Wrapf(err, "aws profile %q: resolving credentials failed; check the profile in ~/.aws/config (sso login expired?)", name)
		}

		id, err := client.GetCallerIdentity(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "aws profile %q: STS GetCallerIdentity failed; credentials are missing, expired or invalid", name)
		}

		log.Info().
			Str("profile", name).
			Str("account", aws.ToString(id.Account)).
			Str("arn", aws.ToString(id.Arn)).
			Str("region", client.GetRegion().String()).
			Msg("[ProfilePool] profile resolved")

		entries = append(entries, profileEntry{
			name:      name,
			accountID: client.GetAccountID(),
			pool:      provider.NewClientPool(ctx, builder),
		})
	}

	return &ProfilePool{entries: entries}, nil
}

// GetClients returns one client per (account, region) across all profiles.
//
// Duplicates are dropped: two profiles may point at the same account, and the
// resource query path fans out over every client without deduplicating, so a
// repeated (account, region) pair would duplicate rows and inflate counts.
// A profile that fails for a given region is logged and skipped, matching the
// behaviour of the awslib pools — one region disabled in one account must not
// fail the whole query.
func (p *ProfilePool) GetClients(regions ...ptypes.AwsRegion) ([]*v3.Client, error) {
	clients := make([]*v3.Client, 0, len(p.entries)*len(regions))
	seen := map[string]bool{}

	for _, entry := range p.entries {
		found, err := entry.pool.GetClients(regions...)
		if err != nil {
			log.Warn().Err(err).
				Str("profile", entry.name).
				Msg("[ProfilePool.GetClients] failed to init aws clients for profile. Skipping..")

			continue
		}

		for _, client := range found {
			key := client.GetAccountID().String() + "/" + client.GetRegion().String()
			if seen[key] {
				log.Debug().
					Str("profile", entry.name).
					Str("key", key).
					Msg("[ProfilePool.GetClients] duplicate account/region, skipping")

				continue
			}

			seen[key] = true
			clients = append(clients, client)
		}
	}

	return clients, nil
}

// ListAccountIDs returns the accounts reached by the configured profiles,
// deduplicated and sorted. The IDs are resolved at construction, so this is
// answerable without having created any region client yet.
func (p *ProfilePool) ListAccountIDs() ([]ptypes.AwsAccountID, error) {
	seen := map[ptypes.AwsAccountID]bool{}
	ids := make([]ptypes.AwsAccountID, 0, len(p.entries))

	for _, entry := range p.entries {
		if entry.accountID == "" || seen[entry.accountID] {
			continue
		}

		seen[entry.accountID] = true
		ids = append(ids, entry.accountID)
	}

	slices.Sort(ids)

	return ids, nil
}
