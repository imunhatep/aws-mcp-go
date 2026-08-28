package mcpserver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// stubPool is a minimal ClientPool for the lazy-pool tests.
type stubPool struct {
	accounts []ptypes.AwsAccountID
}

func (s *stubPool) GetClients(...ptypes.AwsRegion) ([]*v3.Client, error) {
	return []*v3.Client{{}}, nil
}

func (s *stubPool) GetAccountClients(ptypes.AwsAccountID, ...ptypes.AwsRegion) ([]*v3.Client, error) {
	return []*v3.Client{{}}, nil
}

func (s *stubPool) ListAccountIDs() ([]ptypes.AwsAccountID, error) {
	return s.accounts, nil
}

// TestLazyPoolDefersConstruction: nothing may touch AWS until a tool call does.
// This is what keeps an expired SSO session from taking the process down at
// startup.
func TestLazyPoolDefersConstruction(t *testing.T) {
	builds := 0

	pool := NewLazyPool(context.Background(), func(context.Context) (ClientPool, error) {
		builds++

		return &stubPool{accounts: []ptypes.AwsAccountID{"111111111111"}}, nil
	})

	assert.Zero(t, builds, "constructing the pool must not resolve credentials")

	ready, err := pool.Ready()
	assert.False(t, ready)
	assert.NoError(t, err)

	ids, err := pool.ListAccountIDs()
	require.NoError(t, err)
	assert.Equal(t, []ptypes.AwsAccountID{"111111111111"}, ids)
	assert.Equal(t, 1, builds)

	// Built once, reused after that.
	_, err = pool.GetClients(ptypes.DefaultAwsRegion)
	require.NoError(t, err)
	assert.Equal(t, 1, builds)

	ready, err = pool.Ready()
	assert.True(t, ready)
	assert.NoError(t, err)
}

// TestLazyPoolRetriesAfterFailure: a credential failure must be reported to the
// caller and then retried, so an `aws sso login` in another terminal takes effect
// on the next question the agent asks — no restart.
func TestLazyPoolRetriesAfterFailure(t *testing.T) {
	attempts := 0
	loggedIn := false

	pool := &LazyPool{
		ctx: context.Background(),
		// No throttle in the test; the production value is lazyRetryInterval.
		retryInterval: 0,
		build: func(context.Context) (ClientPool, error) {
			attempts++

			if !loggedIn {
				return nil, errors.New("cached SSO token is expired")
			}

			return &stubPool{accounts: []ptypes.AwsAccountID{"111111111111"}}, nil
		},
	}

	_, err := pool.GetClients(ptypes.DefaultAwsRegion)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")

	ready, lastErr := pool.Ready()
	assert.False(t, ready)
	require.Error(t, lastErr)

	// The user logs in; the very next call succeeds.
	loggedIn = true

	clients, err := pool.GetClients(ptypes.DefaultAwsRegion)
	require.NoError(t, err)
	assert.Len(t, clients, 1)
	assert.Equal(t, 2, attempts)
}

// TestLazyPoolThrottlesRetries: rebuilding costs STS calls (and role discovery in
// assume-role mode), so a burst of tool calls must not turn into a burst of them.
func TestLazyPoolThrottlesRetries(t *testing.T) {
	attempts := 0

	pool := NewLazyPool(context.Background(), func(context.Context) (ClientPool, error) {
		attempts++

		return nil, errors.New("cached SSO token is expired")
	})

	for range 5 {
		_, err := pool.ListAccountIDs()
		require.Error(t, err)
	}

	assert.Equal(t, 1, attempts)
}
