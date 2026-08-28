package mcpserver

import (
	"context"
	"sync"
	"time"

	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"
)

// lazyRetryInterval throttles rebuilding a pool whose construction failed.
// Construction can cost several AWS calls (STS, and role discovery in
// assume-role mode), so it is not repeated per tool call, but it is short enough
// that a fresh login takes effect on the next question the agent asks.
const lazyRetryInterval = 5 * time.Second

// LazyPool defers building the real client pool until the first tool call, and
// rebuilds it after a failure.
//
// This is what keeps a credential problem from being fatal. Building a pool
// touches AWS — STS GetCallerIdentity, and role discovery in assume-role mode —
// so doing it during startup meant an expired SSO session took the whole process
// down (the error reached main, which exits non-zero) and the MCP client saw a
// dead endpoint instead of an explanation. Deferred, the server always starts,
// every tool call gets a real answer or a real error, and a re-login is picked up
// without a restart.
type LazyPool struct {
	ctx           context.Context
	build         func(context.Context) (ClientPool, error)
	retryInterval time.Duration

	mu        sync.Mutex
	pool      ClientPool
	err       error
	nextRetry time.Time
}

// NewLazyPool wraps a pool constructor. build is called on first use, and again
// after lazyRetryInterval whenever it failed.
func NewLazyPool(ctx context.Context, build func(context.Context) (ClientPool, error)) *LazyPool {
	return &LazyPool{ctx: ctx, build: build, retryInterval: lazyRetryInterval}
}

// get returns the built pool, building it if needed.
//
// The lock is held across construction on purpose: concurrent tool calls arriving
// while credentials are being resolved should wait for the one attempt, not each
// start their own STS storm.
func (p *LazyPool) get() (ClientPool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.pool != nil {
		return p.pool, nil
	}

	if p.err != nil && time.Now().Before(p.nextRetry) {
		return nil, p.err
	}

	pool, err := p.build(p.ctx)
	if err != nil {
		p.err = err
		p.nextRetry = time.Now().Add(p.retryInterval)

		return nil, p.err
	}

	p.pool = pool
	p.err = nil

	return p.pool, nil
}

// Warm attempts construction ahead of the first tool call, so the log shows the
// active identity — or the login instructions — at startup rather than minutes
// later. The error is returned for logging; it is never fatal.
func (p *LazyPool) Warm() error {
	_, err := p.get()

	return err
}

// Ready reports whether the pool has been built, and the last failure if not.
func (p *LazyPool) Ready() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.pool != nil, p.err
}

// GetClients implements ClientPool.
func (p *LazyPool) GetClients(regions ...ptypes.AwsRegion) ([]*v3.Client, error) {
	pool, err := p.get()
	if err != nil {
		return nil, err
	}

	return pool.GetClients(regions...)
}

// GetAccountClients implements ClientPool.
func (p *LazyPool) GetAccountClients(accountID ptypes.AwsAccountID, regions ...ptypes.AwsRegion) ([]*v3.Client, error) {
	pool, err := p.get()
	if err != nil {
		return nil, err
	}

	return pool.GetAccountClients(accountID, regions...)
}

// ListAccountIDs implements ClientPool.
func (p *LazyPool) ListAccountIDs() ([]ptypes.AwsAccountID, error) {
	pool, err := p.get()
	if err != nil {
		return nil, err
	}

	return pool.ListAccountIDs()
}
