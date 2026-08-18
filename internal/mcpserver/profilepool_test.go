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
