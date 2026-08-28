package mcpserver

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/awslib/provider"
	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// profileRetryInterval throttles re-resolving a profile that failed to
// authenticate. It is short on purpose: the fix for the common failure is a
// human approving a browser prompt, which takes seconds, and the next tool call
// must pick the profile up without a restart.
const profileRetryInterval = 5 * time.Second

// regionClientPool is the per-profile pool contract ProfilePool builds on.
// provider.ClientPool satisfies it; the indirection keeps ProfilePool unit
// testable without AWS credentials.
type regionClientPool interface {
	GetClients(regions ...ptypes.AwsRegion) ([]*v3.Client, error)
}

// profileEntry is one AWS shared-config profile and the client pool serving it.
//
// A profile is resolved lazily — see ProfilePool.resolve — so an entry may hold
// no pool yet, or the error from its last failed attempt.
type profileEntry struct {
	name string

	mu        sync.Mutex
	auth      ProfileAuth
	accountID ptypes.AwsAccountID
	pool      regionClientPool
	err       error
	nextRetry time.Time
}

// knownAccountID returns the account this profile serves when that is already
// known — from a resolved identity, or from sso_account_id in the shared config.
// An empty result means "resolve it to find out".
func (e *profileEntry) knownAccountID() ptypes.AwsAccountID {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.accountID
}

// ProfilePool is a ClientPool that fans out across several AWS shared-config
// profiles, one account per profile. Each profile gets its own credential chain
// and its own provider.ClientPool, which caches clients per region.
type ProfilePool struct {
	ctx        context.Context
	entries    []*profileEntry
	login      *SSOLoginManager
	failureTTL time.Duration
}

// NewProfilePool prepares one entry per named profile. It contacts AWS for
// nothing: each profile's configuration is inspected locally (which catches an
// undefined profile, a missing or expired SSO login and a read-only token cache
// with an actionable message) and its account ID is taken from sso_account_id
// when the profile declares one.
//
// Credentials are exercised on first use instead, per profile. That is the whole
// point: a server that cannot authenticate must still start and still answer, so
// the agent calling it gets "this profile needs a login, here is how" rather
// than a dead endpoint. See docs/authentication.md.
func NewProfilePool(ctx context.Context, profiles []string, login *SSOLoginManager) (*ProfilePool, error) {
	if len(profiles) == 0 {
		return nil, errors.New("no aws profiles were given")
	}

	entries := make([]*profileEntry, 0, len(profiles))

	for _, name := range profiles {
		entry := &profileEntry{name: name}

		auth, err := PreflightProfile(ctx, name)
		entry.auth = auth
		entry.accountID = ptypes.AwsAccountID(auth.SSOAccountID)

		if err != nil {
			// Not fatal: record it, warn, and let the first use retry. The
			// human fix (an SSO approval) usually lands before the first tool
			// call does.
			entry.err = login.Explain(auth, err)
			entry.nextRetry = time.Now().Add(profileRetryInterval)

			log.Warn().Err(entry.err).
				Str("profile", name).
				Msg("[ProfilePool] profile is not usable yet; the server starts anyway and retries on use")
		}

		entries = append(entries, entry)
	}

	return &ProfilePool{ctx: ctx, entries: entries, login: login}, nil
}

// WithFailureTTL sets the client-failure cache TTL applied to every profile's
// pool, so the whole ProfilePool ages failed (account, region) pairs on the same
// clock as the resource cache. Profiles resolved later inherit it too.
func (p *ProfilePool) WithFailureTTL(ttl time.Duration) *ProfilePool {
	p.failureTTL = ttl

	for _, entry := range p.entries {
		entry.mu.Lock()
		// Only the real pool caches failures; the test double does not.
		if pool, ok := entry.pool.(*provider.ClientPool); ok {
			pool.WithFailureTTL(ttl)
		}
		entry.mu.Unlock()
	}

	return p
}

// resolve builds the profile's client pool if it does not have one yet, and
// records its account identity.
//
// A failure is remembered only for profileRetryInterval: credentials are exactly
// the kind of state that changes underneath a running server, so a profile that
// failed a minute ago is worth trying again rather than being written off for the
// process lifetime.
func (p *ProfilePool) resolve(entry *profileEntry) error {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.pool != nil {
		return nil
	}

	if entry.err != nil && time.Now().Before(entry.nextRetry) {
		return entry.err
	}

	pool, auth, accountID, err := p.build(entry.name)
	if err != nil {
		entry.err = err
		entry.nextRetry = time.Now().Add(profileRetryInterval)

		// Keep whatever the config told us about the account, so account-scoped
		// queries can still skip this profile without exercising it.
		if entry.accountID == "" {
			entry.accountID = ptypes.AwsAccountID(auth.SSOAccountID)
		}

		return entry.err
	}

	entry.auth = auth
	entry.accountID = accountID
	entry.pool = pool
	entry.err = nil

	return nil
}

// build resolves one profile's credentials and returns its pool and identity.
func (p *ProfilePool) build(name string) (regionClientPool, ProfileAuth, ptypes.AwsAccountID, error) {
	// Parse the profile's config first: this catches an undefined profile, a
	// missing or unrefreshable SSO login and a read-only token cache with an
	// actionable message, instead of an opaque SDK error on the STS call below.
	auth, err := PreflightProfile(p.ctx, name)
	if err != nil {
		return nil, auth, "", p.login.Explain(auth, err)
	}

	// Build the credential chain from the named profile only. Note this
	// deliberately does not go through v3.DefaultAwsClientProviders: that
	// helper folds in ambient AWS_ACCESS_KEY_ID / AWS_PROFILE env credentials,
	// which would override the profile and collapse every profile onto the same
	// identity.
	providers := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRetryMaxAttempts(v3.AwsRetryAttempts),
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.AddWithMaxBackoffDelay(retry.NewStandard(), v3.AwsRetryMaxBackoffDelay)
		}),
		awsconfig.WithSharedConfigProfile(name),
	}

	builder := v3.NewClientBuilder(p.ctx, providers...)

	client, err := builder.DefaultClient()
	if err != nil {
		return nil, auth, "", p.login.Explain(auth, err)
	}

	// One STS call per profile, on first use: it confirms the credentials really
	// work and pins the account ID for account-scoped queries.
	id, err := client.GetCallerIdentity(p.ctx)
	if err != nil {
		return nil, auth, "", p.login.Explain(auth, err)
	}

	log.Info().
		Str("profile", name).
		Str("account", aws.ToString(id.Account)).
		Str("arn", aws.ToString(id.Arn)).
		Str("region", client.GetRegion().String()).
		Msg("[ProfilePool] profile resolved")

	pool := provider.NewClientPool(p.ctx, builder)
	if p.failureTTL > 0 {
		pool.WithFailureTTL(p.failureTTL)
	}

	return pool, auth, client.GetAccountID(), nil
}

// GetClients returns one client per (account, region) across all profiles.
//
// Duplicates are dropped: two profiles may point at the same account, and the
// resource query path fans out over every client without deduplicating, so a
// repeated (account, region) pair would duplicate rows and inflate counts.
// A profile that fails for a given region is logged and skipped, matching the
// behaviour of the awslib pools — one region disabled in one account must not
// fail the whole query. But if *no* profile could be authenticated at all, that
// is an error rather than an empty list: "no resources" and "nobody is logged
// in" must never look the same to the caller.
func (p *ProfilePool) GetClients(regions ...ptypes.AwsRegion) ([]*v3.Client, error) {
	clients := make([]*v3.Client, 0, len(p.entries)*len(regions))
	seen := map[string]bool{}
	failures := []string{}
	authenticated := 0

	for _, entry := range p.entries {
		if err := p.resolve(entry); err != nil {
			log.Warn().Err(err).
				Str("profile", entry.name).
				Msg("[ProfilePool.GetClients] profile could not be authenticated. Skipping..")

			failures = append(failures, entry.name+": "+err.Error())

			continue
		}

		authenticated++

		found, err := entry.pool.GetClients(regions...)
		if err != nil {
			log.Warn().Err(err).
				Str("profile", entry.name).
				Msg("[ProfilePool.GetClients] failed to init aws clients for profile. Skipping..")

			// Credentials that expire while the server runs surface here rather
			// than from resolve, so this failure gets the same treatment: an
			// explanation, and a login if one can be started.
			failures = append(failures, entry.name+": "+p.explain(entry, err).Error())

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

	if authenticated == 0 && len(failures) > 0 {
		return nil, errors.Errorf("no aws profile could be authenticated: %s", strings.Join(failures, " | "))
	}

	// An empty client set with failures behind it is not an empty account — it is
	// an unanswered question. Returning it as success is how a listing ends up
	// reporting "no resources" for an account nobody could reach.
	if len(clients) == 0 && len(failures) > 0 {
		return nil, errors.Errorf("no aws client could be created: %s", strings.Join(failures, " | "))
	}

	return clients, nil
}

// explain wraps a mid-run AWS failure with the profile's credential context and,
// when the session has expired beyond refresh, a login to point the user at.
func (p *ProfilePool) explain(entry *profileEntry, err error) error {
	entry.mu.Lock()
	auth := entry.auth
	entry.mu.Unlock()

	return p.login.Explain(auth, err)
}

// GetAccountClients returns one client per region for a single account.
//
// Profiles known to serve another account are skipped without any AWS call at
// all — the account comes from sso_account_id, or from an identity resolved
// earlier — so the scoping is free and, more importantly, the other accounts'
// credentials are never exercised. A profile whose account is not yet known has
// to be resolved to find out, which costs one STS call for that profile.
//
// An account no profile can serve is an error rather than an empty slice, and an
// account whose profile exists but cannot authenticate reports that instead.
func (p *ProfilePool) GetAccountClients(accountID ptypes.AwsAccountID, regions ...ptypes.AwsRegion) ([]*v3.Client, error) {
	clients := make([]*v3.Client, 0, len(regions))
	seen := map[string]bool{}
	authFailures := []string{}
	matched := false

	for _, entry := range p.entries {
		if known := entry.knownAccountID(); known != "" && known != accountID {
			continue
		}

		if err := p.resolve(entry); err != nil {
			// Only report it if this profile might have served the account: a
			// profile whose account is already known to match is a real failure,
			// an unknown one is a maybe. Both are worth surfacing, since the
			// alternative is answering "no resources" for an account nobody
			// could reach.
			authFailures = append(authFailures, entry.name+": "+err.Error())

			log.Warn().Err(err).
				Str("profile", entry.name).
				Msg("[ProfilePool.GetAccountClients] profile could not be authenticated. Skipping..")

			continue
		}

		if entry.knownAccountID() != accountID {
			continue
		}

		matched = true

		found, err := entry.pool.GetClients(regions...)
		if err != nil {
			log.Warn().Err(err).
				Str("profile", entry.name).
				Msg("[ProfilePool.GetAccountClients] failed to init aws clients for profile. Skipping..")

			authFailures = append(authFailures, entry.name+": "+p.explain(entry, err).Error())

			continue
		}

		for _, client := range found {
			key := client.GetAccountID().String() + "/" + client.GetRegion().String()
			if seen[key] {
				continue
			}

			seen[key] = true
			clients = append(clients, client)
		}
	}

	if !matched {
		if len(authFailures) > 0 {
			return nil, errors.Errorf("account %s could not be reached: %s", accountID, strings.Join(authFailures, " | "))
		}

		return nil, errors.Errorf("account %s is not reachable by any configured profile", accountID)
	}

	// Same rule as GetClients: an empty client set with a failure behind it is an
	// unanswered question, not an empty account.
	if len(clients) == 0 && len(authFailures) > 0 {
		return nil, errors.Errorf("account %s could not be reached: %s", accountID, strings.Join(authFailures, " | "))
	}

	return clients, nil
}

// ListAccountIDs returns the accounts reached by the configured profiles,
// deduplicated and sorted. Accounts declared in the shared config
// (sso_account_id) are answered without any AWS call; profiles that declare none
// are resolved to find out.
func (p *ProfilePool) ListAccountIDs() ([]ptypes.AwsAccountID, error) {
	seen := map[ptypes.AwsAccountID]bool{}
	ids := make([]ptypes.AwsAccountID, 0, len(p.entries))
	authFailures := []string{}

	for _, entry := range p.entries {
		id := entry.knownAccountID()
		if id == "" {
			if err := p.resolve(entry); err != nil {
				authFailures = append(authFailures, entry.name+": "+err.Error())

				continue
			}

			id = entry.knownAccountID()
		}

		if id == "" || seen[id] {
			continue
		}

		seen[id] = true
		ids = append(ids, id)
	}

	if len(ids) == 0 && len(authFailures) > 0 {
		return nil, errors.Errorf("no aws profile could be authenticated: %s", strings.Join(authFailures, " | "))
	}

	slices.Sort(ids)

	return ids, nil
}

// Profiles returns the configured profiles and their current authentication
// state, for the aws_auth_status tool.
func (p *ProfilePool) Profiles() []ProfileState {
	states := make([]ProfileState, 0, len(p.entries))

	for _, entry := range p.entries {
		entry.mu.Lock()
		states = append(states, ProfileState{
			Auth:          entry.auth,
			AccountID:     entry.accountID.String(),
			Authenticated: entry.pool != nil,
			Err:           entry.err,
		})
		entry.mu.Unlock()
	}

	return states
}

// ProfileState is a snapshot of one profile's authentication state.
type ProfileState struct {
	Auth          ProfileAuth
	AccountID     string
	Authenticated bool
	Err           error
}
