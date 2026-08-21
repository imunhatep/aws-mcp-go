package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// scopeSpyPool records which pool method a query reached for, which is the whole
// question: an account-scoped query must resolve its clients through
// GetAccountClients, because GetClients has already contacted every account by
// the time it returns.
type scopeSpyPool struct {
	allCalls     int
	accountCalls int
	lastAccount  ptypes.AwsAccountID
	err          error
}

func (p *scopeSpyPool) GetClients(_ ...ptypes.AwsRegion) ([]*v3.Client, error) {
	p.allCalls++
	return nil, p.err
}

func (p *scopeSpyPool) GetAccountClients(accountID ptypes.AwsAccountID, _ ...ptypes.AwsRegion) ([]*v3.Client, error) {
	p.accountCalls++
	p.lastAccount = accountID
	return nil, p.err
}

func (p *scopeSpyPool) ListAccountIDs() ([]ptypes.AwsAccountID, error) {
	return []ptypes.AwsAccountID{"111111111111"}, nil
}

func TestPoolClientsScopesWhenAccountGiven(t *testing.T) {
	spy := &scopeSpyPool{}
	srv := &Server{pool: spy}

	_, err := srv.poolClients("111111111111", []ptypes.AwsRegion{ptypes.DefaultAwsRegion})
	require.NoError(t, err)

	assert.Equal(t, 1, spy.accountCalls, "an account_id must go through the scoped path")
	assert.Zero(t, spy.allCalls, "the unscoped path must not be reached")
	assert.Equal(t, ptypes.AwsAccountID("111111111111"), spy.lastAccount)
}

func TestPoolClientsFansOutWhenNoAccountGiven(t *testing.T) {
	spy := &scopeSpyPool{}
	srv := &Server{pool: spy}

	_, err := srv.poolClients("", []ptypes.AwsRegion{ptypes.DefaultAwsRegion})
	require.NoError(t, err)

	assert.Equal(t, 1, spy.allCalls)
	assert.Zero(t, spy.accountCalls)
}

// TestPoolClientsPropagatesUnreachableAccount: the pool's "not reachable" error
// must surface to the caller rather than degrade into an empty result set.
func TestPoolClientsPropagatesUnreachableAccount(t *testing.T) {
	spy := &scopeSpyPool{err: errors.New("account 999999999999 is not reachable by any configured profile")}
	srv := &Server{pool: spy}

	_, err := srv.poolClients("999999999999", []ptypes.AwsRegion{ptypes.DefaultAwsRegion})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not reachable")
}

// TestFetchPathsScopeByAccount covers both resource paths at once: each must
// resolve clients through the scoped method when given an account_id. They share
// poolClients, so this pins that neither ever calls the pool directly again.
func TestFetchPathsScopeByAccount(t *testing.T) {
	regions := []ptypes.AwsRegion{ptypes.DefaultAwsRegion}

	t.Run("typed path", func(t *testing.T) {
		spy := &scopeSpyPool{}
		srv := &Server{pool: spy}

		_, err := srv.fetchResources("AWS::EC2::Instance", regions, "111111111111")
		require.NoError(t, err)

		assert.Equal(t, 1, spy.accountCalls)
		assert.Zero(t, spy.allCalls)
	})

	t.Run("fallback path", func(t *testing.T) {
		spy := &scopeSpyPool{}
		srv := &Server{pool: spy}

		fetched, err := srv.fetchFallbackResources("AWS::Kinesis::Stream", regions, false, "111111111111")
		require.NoError(t, err)

		assert.Equal(t, 1, spy.accountCalls)
		assert.Zero(t, spy.allCalls)
		assert.Equal(t, "cloudcontrol", fetched.scope.Source)
	})
}
