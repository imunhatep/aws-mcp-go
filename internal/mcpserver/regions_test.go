package mcpserver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

func enabledSet(regions ...ptypes.AwsRegion) map[ptypes.AwsRegion]struct{} {
	set := make(map[ptypes.AwsRegion]struct{}, len(regions))
	for _, region := range regions {
		set[region] = struct{}{}
	}

	return set
}

// TestNarrowToEnabled covers the rule that decides which regions a sweep pays
// for. Getting this wrong in the narrowing direction produces a confident empty
// answer, so each case states what it protects.
func TestNarrowToEnabled(t *testing.T) {
	requested := []ptypes.AwsRegion{"eu-central-1", "me-south-1", "eu-west-1", "ap-south-2"}

	t.Run("skips regions no account enabled", func(t *testing.T) {
		kept, skipped := narrowToEnabled(requested, enabledSet("eu-central-1", "eu-west-1"))

		assert.Equal(t, []ptypes.AwsRegion{"eu-central-1", "eu-west-1"}, kept)
		assert.Equal(t, []string{"me-south-1", "ap-south-2"}, skipped)
	})

	t.Run("keeps the default region unconditionally", func(t *testing.T) {
		// us-east-1 needs no opt-in and serves the global endpoints, so it must
		// survive even a lookup that failed to mention it.
		kept, skipped := narrowToEnabled(
			[]ptypes.AwsRegion{ptypes.DefaultAwsRegion, "me-south-1"},
			enabledSet(),
		)

		assert.Equal(t, []ptypes.AwsRegion{ptypes.DefaultAwsRegion}, kept)
		assert.Equal(t, []string{"me-south-1"}, skipped)
	})

	t.Run("keeps everything when every region is enabled", func(t *testing.T) {
		kept, skipped := narrowToEnabled(requested, enabledSet(requested...))

		assert.Equal(t, requested, kept)
		assert.Empty(t, skipped, "nothing was skipped, so nothing should be reported")
	})
}

// TestNarrowRegionsHonoursExplicitRegion: an explicit region is an instruction.
// Narrowing it would answer a different question than the one asked, and the
// nil pool here would panic if the lookup were attempted at all.
func TestNarrowRegionsHonoursExplicitRegion(t *testing.T) {
	srv := &Server{regions: newRegionCache(time.Hour)}

	regions, warnings := srv.narrowRegions([]ptypes.AwsRegion{"eu-central-1"}, "eu-central-1", "")

	assert.Equal(t, []ptypes.AwsRegion{"eu-central-1"}, regions)
	assert.Empty(t, warnings)
}

// TestNarrowRegionsFailsOpen is the load-bearing case: when the server cannot
// find out what an account enabled, it must query everything and say so. The
// alternative — narrowing on a partial answer — turns an unknown into a
// confident "no resources", which is the failure this whole feature exists to
// remove.
func TestNarrowRegionsFailsOpen(t *testing.T) {
	spy := &scopeSpyPool{err: errors.New("sso session expired")}
	srv := &Server{pool: spy, regions: newRegionCache(time.Hour)}

	requested := ptypes.GetAwsRegionList()
	regions, warnings := srv.narrowRegions(requested, "", "")

	assert.Len(t, regions, len(requested), "every region must still be queried")
	require.Len(t, warnings, 1, "the caller must be told the estate was not narrowed")
	assert.Contains(t, warnings[0], "could not determine which regions")
}

// TestNarrowRegionsWithoutPool guards the same fail-open path for a server built
// without a pool, which is how the AWS-independent tools are tested.
func TestNarrowRegionsWithoutPool(t *testing.T) {
	srv := &Server{regions: newRegionCache(time.Hour)}

	requested := ptypes.GetAwsRegionList()
	regions, warnings := srv.narrowRegions(requested, "", "")

	assert.Len(t, regions, len(requested))
	assert.Len(t, warnings, 1)
}

func TestRegionCacheExpires(t *testing.T) {
	cache := newRegionCache(time.Hour)
	account := ptypes.AwsAccountID("111111111111")

	_, ok := cache.get(account)
	assert.False(t, ok, "an unknown account is a miss")

	cache.put(account, enabledSet("eu-central-1"), time.Now())
	got, ok := cache.get(account)
	require.True(t, ok)
	assert.Contains(t, got, ptypes.AwsRegion("eu-central-1"))

	// An entry older than the TTL is a miss: a region enabled after the last
	// lookup has to become visible without a restart.
	cache.put(account, enabledSet("eu-central-1"), time.Now().Add(-2*time.Hour))
	_, ok = cache.get(account)
	assert.False(t, ok, "an entry past its TTL must not be served")
}

func TestNamedRegionsTruncates(t *testing.T) {
	many := make([]string, 0, maxNamedRegions+3)
	for range maxNamedRegions + 3 {
		many = append(many, "region")
	}

	assert.Contains(t, namedRegions(many), "and 3 more")
	assert.NotContains(t, namedRegions([]string{"eu-west-1"}), "more")
}

// TestRegionCacheNilIsSafe: a Server assembled directly in a test has no region
// cache, and a nil cache must degrade to "always look it up", never panic.
func TestRegionCacheNilIsSafe(t *testing.T) {
	var cache *regionCache

	_, ok := cache.get("111111111111")
	assert.False(t, ok)

	assert.NotPanics(t, func() {
		cache.put("111111111111", enabledSet("eu-central-1"), time.Now())
	})
}
