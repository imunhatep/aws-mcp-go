package mcpserver

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/rs/zerolog/log"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service/ec2"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// maxNamedRegions bounds how many skipped regions the narrowing note lists, for
// the same reason describeFailures truncates: the count is always exact, the
// enumeration is a courtesy.
const maxNamedRegions = 8

// regionCache remembers which regions each account has enabled.
//
// An account answers for every region in awslib's static list, but only for the
// ones it has actually enabled: a region the account never opted into rejects
// every call, and the client for it costs a full STS round trip to discover
// that. Asking each account once — DescribeRegions is a single call that returns
// the whole answer — replaces up to one dead client build per region per
// account, which is what made an all-regions sweep across many accounts too slow
// to finish.
//
// This is deliberately not the same thing as v3.FailureCache. That cache makes a
// repeat of a known-bad pair cheap; this avoids asking in the first place, which
// is what the first sweep after a restart needs.
type regionCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[ptypes.AwsAccountID]regionCacheEntry
}

type regionCacheEntry struct {
	regions   map[ptypes.AwsRegion]struct{}
	fetchedAt time.Time
}

func newRegionCache(ttl time.Duration) *regionCache {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}

	return &regionCache{ttl: ttl, entries: map[ptypes.AwsAccountID]regionCacheEntry{}}
}

// get and put tolerate a nil cache — a Server assembled directly (as the tests
// do) then simply looks the regions up every time rather than panicking.
func (c *regionCache) get(accountID ptypes.AwsAccountID) (map[ptypes.AwsRegion]struct{}, bool) {
	if c == nil {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[accountID]
	if !ok || time.Since(entry.fetchedAt) > c.ttl {
		return nil, false
	}

	return entry.regions, true
}

func (c *regionCache) put(accountID ptypes.AwsAccountID, regions map[ptypes.AwsRegion]struct{}, now time.Time) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[accountID] = regionCacheEntry{regions: regions, fetchedAt: now}
}

// narrowRegions drops the regions no reachable account has enabled, so a sweep
// pays for the estate that exists rather than for all 30 regions AWS publishes.
//
// It narrows only when the caller did not name a region: an explicit region is
// an instruction, and answering a different question than the one asked — even a
// cheaper one — is not this function's call to make.
//
// It fails open in every uncertain case. If the lookup fails for even one
// account, the full region list is used: the enabled sets are unioned across
// accounts, so one missing set could silently drop a region another account does
// hold resources in. A wrong narrowing produces an empty answer that looks
// authoritative, which is the failure mode this whole change exists to remove —
// so uncertainty always costs time rather than correctness.
//
// Whatever it does is stated in the returned notes. Narrowing that the caller
// cannot see is indistinguishable from an estate that is smaller than it is.
func (s *Server) narrowRegions(regions []ptypes.AwsRegion, regionArg, accountID string) ([]ptypes.AwsRegion, []string) {
	if regionArg != "" || len(regions) <= 1 {
		return regions, nil
	}

	enabled, err := s.enabledRegions(accountID)
	if err != nil {
		log.Warn().Err(err).Msg("[mcpserver.narrowRegions] falling back to every known region")

		return regions, []string{fmt.Sprintf(
			"could not determine which regions these accounts have enabled (%v); every known region was queried, which is slower but never narrower than the truth",
			err,
		)}
	}

	kept, skipped := narrowToEnabled(regions, enabled)
	if len(skipped) == 0 {
		return regions, nil
	}

	return kept, []string{fmt.Sprintf(
		"queried %d of %d regions: %d are not enabled in any reachable account and were skipped (%s)",
		len(kept), len(regions), len(skipped), namedRegions(skipped),
	)}
}

// narrowToEnabled splits the requested regions into the ones some account has
// enabled and the ones no account has.
func narrowToEnabled(regions []ptypes.AwsRegion, enabled map[ptypes.AwsRegion]struct{}) (kept []ptypes.AwsRegion, skipped []string) {
	kept = make([]ptypes.AwsRegion, 0, len(regions))

	for _, region := range regions {
		// The default region needs no opt-in and is where the global services
		// are reached, so it stays in regardless of what the lookup reported.
		if _, ok := enabled[region]; ok || region == ptypes.DefaultAwsRegion {
			kept = append(kept, region)
			continue
		}

		skipped = append(skipped, region.String())
	}

	return kept, skipped
}

// namedRegions renders a truncated region list for the narrowing note.
func namedRegions(regions []string) string {
	if len(regions) <= maxNamedRegions {
		return strings.Join(regions, ", ")
	}

	return strings.Join(regions[:maxNamedRegions], ", ") +
		fmt.Sprintf(" and %d more", len(regions)-maxNamedRegions)
}

// enabledRegions returns the union of the regions the pool's accounts have
// enabled, hitting AWS at most once per account per TTL.
//
// The union, not the intersection: a region enabled in one account of many still
// has to be swept, or resources living there go unreported. Trimming per account
// would be tighter, but the pool's fan-out takes one region list for every
// account, and a client that fails for an account that lacks the region is
// exactly what v3.FailureCache already makes cheap.
func (s *Server) enabledRegions(accountID string) (map[ptypes.AwsRegion]struct{}, error) {
	if s.pool == nil {
		return nil, errors.Errorf("no client pool configured")
	}

	// One client per account, in the region every account has: enough to ask
	// each account what it has enabled, without building the matrix this call
	// exists to shrink.
	clients, err := s.poolClients(accountID, []ptypes.AwsRegion{ptypes.DefaultAwsRegion})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if len(clients) == 0 {
		return nil, errors.Errorf("no reachable accounts")
	}

	known := map[ptypes.AwsRegion]struct{}{}
	for _, region := range ptypes.GetAwsRegionList() {
		known[region] = struct{}{}
	}

	union := map[ptypes.AwsRegion]struct{}{}
	for _, client := range clients {
		account := client.GetAccountID()

		regions, cached := s.regions.get(account)
		if !cached {
			// DescribeRegions without AllRegions returns the regions enabled for
			// this account — the opt-in ones it accepted plus the ones that need
			// no opt-in. ListRegionsOptIn is the wrong call here despite the
			// name: it filters to opt-in-status=opted-in, which excludes every
			// standard region and would narrow a sweep to almost nothing.
			found, err := ec2.NewEc2Repository(s.ctx, client).ListRegionsAll()
			if err != nil {
				return nil, errors.WithStack(err)
			}

			regions = map[ptypes.AwsRegion]struct{}{}
			for _, region := range found {
				name := ptypes.AwsRegion(aws.ToString(region.RegionName))
				// A region AWS has launched but awslib does not know cannot be
				// queried anyway — the proxies are built from its static list.
				if _, ok := known[name]; ok {
					regions[name] = struct{}{}
				}
			}

			log.Debug().
				Stringer("accountID", account).
				Int("regions", len(regions)).
				Msg("[mcpserver.enabledRegions] account region opt-in status resolved")

			s.regions.put(account, regions, time.Now())
		}

		for region := range regions {
			union[region] = struct{}{}
		}
	}

	return union, nil
}
