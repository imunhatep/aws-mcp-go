package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// fakeRegionPool stands in for a per-profile provider.ClientPool. Note the
// clients it hands back cannot carry synthetic account IDs or regions — those
// fields are unexported in awslib and only set by a real STS call — so the
// tests here cover entry bookkeeping and failure tolerance, and exercise the
// dedupe path with identity-less clients.
type fakeRegionPool struct {
	clients []*v3.Client
	err     error
	calls   int
}

func (f *fakeRegionPool) GetClients(_ ...ptypes.AwsRegion) ([]*v3.Client, error) {
	f.calls++
	return f.clients, f.err
}

func TestProfilePoolListAccountIDsDedupesAndSorts(t *testing.T) {
	pool := &ProfilePool{entries: []profileEntry{
		{name: "prod", accountID: "222222222222"},
		{name: "dev", accountID: "111111111111"},
		// A second profile onto the same account, e.g. a read-only variant.
		{name: "dev-ro", accountID: "111111111111"},
		{name: "broken", accountID: ""},
	}}

	ids, err := pool.ListAccountIDs()
	require.NoError(t, err)

	assert.Equal(t, []ptypes.AwsAccountID{"111111111111", "222222222222"}, ids)
}

func TestProfilePoolGetClientsSkipsFailingProfile(t *testing.T) {
	ok := &fakeRegionPool{clients: []*v3.Client{}}
	broken := &fakeRegionPool{err: errors.New("sso session expired")}

	pool := &ProfilePool{entries: []profileEntry{
		{name: "broken", accountID: "111111111111", pool: broken},
		{name: "dev", accountID: "222222222222", pool: ok},
	}}

	clients, err := pool.GetClients(ptypes.DefaultAwsRegion)

	// One bad profile must not fail the whole query — the healthy profile is
	// still consulted.
	require.NoError(t, err)
	assert.Empty(t, clients)
	assert.Equal(t, 1, ok.calls)
}

// TestProfilePoolGetAccountClientsOnlyTouchesThatAccount is the point of the
// whole account-scoping change: a query scoped to one account must not exercise
// any other account's credentials. Filtering rows afterwards would leave the
// other pool's call count at 1 — for a server configured with both a
// development and a production profile, that call is the thing being prevented.
func TestProfilePoolGetAccountClientsOnlyTouchesThatAccount(t *testing.T) {
	dev := &fakeRegionPool{clients: []*v3.Client{{}}}
	prod := &fakeRegionPool{clients: []*v3.Client{{}}}

	pool := &ProfilePool{entries: []profileEntry{
		{name: "mydev", accountID: "111111111111", pool: dev},
		{name: "myprod", accountID: "222222222222", pool: prod},
	}}

	clients, err := pool.GetAccountClients("111111111111", ptypes.DefaultAwsRegion)
	require.NoError(t, err)

	assert.Len(t, clients, 1)
	assert.Equal(t, 1, dev.calls, "the requested account's profile must be used")
	assert.Zero(t, prod.calls, "no other account may be touched")
}

// TestProfilePoolGetAccountClientsRejectsUnreachableAccount: an account no
// profile points at is an error, not an empty list that reads as "nothing there".
func TestProfilePoolGetAccountClientsRejectsUnreachableAccount(t *testing.T) {
	dev := &fakeRegionPool{clients: []*v3.Client{{}}}

	pool := &ProfilePool{entries: []profileEntry{
		{name: "mydev", accountID: "111111111111", pool: dev},
	}}

	clients, err := pool.GetAccountClients("999999999999", ptypes.DefaultAwsRegion)

	require.Error(t, err)
	assert.Empty(t, clients)
	assert.Contains(t, err.Error(), "999999999999")
	assert.Zero(t, dev.calls, "an unreachable account must be rejected before any AWS work")
}

// TestProfilePoolGetAccountClientsSpansProfilesForOneAccount: several profiles
// can point at the same account (a read-only and an operator variant), and all
// of them are consulted, with the (account, region) dedupe still applied.
func TestProfilePoolGetAccountClientsSpansProfilesForOneAccount(t *testing.T) {
	shared := &v3.Client{}
	first := &fakeRegionPool{clients: []*v3.Client{shared}}
	second := &fakeRegionPool{clients: []*v3.Client{{}}}
	other := &fakeRegionPool{clients: []*v3.Client{{}}}

	pool := &ProfilePool{entries: []profileEntry{
		{name: "dev", accountID: "111111111111", pool: first},
		{name: "dev-ro", accountID: "111111111111", pool: second},
		{name: "prod", accountID: "222222222222", pool: other},
	}}

	clients, err := pool.GetAccountClients("111111111111", ptypes.DefaultAwsRegion)
	require.NoError(t, err)

	require.Len(t, clients, 1, "identity-less duplicates collapse to one client")
	assert.Same(t, shared, clients[0])
	assert.Equal(t, 1, first.calls)
	assert.Equal(t, 1, second.calls)
	assert.Zero(t, other.calls)
}

func TestProfilePoolGetClientsDedupesAccountRegion(t *testing.T) {
	// Two profiles pointing at the same account return the same (account,
	// region) pair; only one client must survive, or list_resources would
	// return every row twice.
	shared := &v3.Client{}

	pool := &ProfilePool{entries: []profileEntry{
		{name: "dev", accountID: "111111111111", pool: &fakeRegionPool{clients: []*v3.Client{shared}}},
		{name: "dev-ro", accountID: "111111111111", pool: &fakeRegionPool{clients: []*v3.Client{{}}}},
	}}

	clients, err := pool.GetClients(ptypes.DefaultAwsRegion)
	require.NoError(t, err)

	require.Len(t, clients, 1)
	assert.Same(t, shared, clients[0])
}
