// Package mcpserver wires the awslib client pool and resource cache into an MCP
// server that exposes tools for listing AWS resources.
package mcpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/awslib/cache"
	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"

	"github.com/imunhatep/aws-mcp-go/internal/version"
	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

const serverName = "aws-mcp"

// ClientPool is the subset of the provider client-pool API the MCP server
// depends on. Both provider.ClientPool (local / default credentials) and
// v3.ClientPool (cross-account assume-role) satisfy it, so the server works
// identically in either authentication mode.
type ClientPool interface {
	// GetClients returns one client per requested region (per account, in
	// assume-role mode). Clients are cached inside the pool across calls.
	GetClients(regions ...ptypes.AwsRegion) ([]*v3.Client, error)
	// GetAccountClients returns clients for a single account.
	//
	// This exists because filtering afterwards is too late: creating a client
	// assumes that account's role (or uses that profile's credentials), so a
	// query scoped to one account must be scoped here, before any other
	// account has been touched. An account the pool cannot reach must be an
	// error, not an empty slice — otherwise a caller asking about the wrong
	// account is told it holds nothing.
	GetAccountClients(accountID ptypes.AwsAccountID, regions ...ptypes.AwsRegion) ([]*v3.Client, error)
	// ListAccountIDs returns the account IDs the pool can reach.
	ListAccountIDs() ([]ptypes.AwsAccountID, error)
}

// poolClients resolves the clients a query should run against, scoping to one
// account when the caller asked for one.
//
// The scoping has to happen here rather than on the resulting rows. Creating a
// client assumes the target account's role or exercises that profile's
// credentials, and every client is then fanned out over by the provider — so an
// account_id applied after the fetch would still have issued API calls against
// every other account in the pool, and merely hidden their rows. For a server
// whose pool spans both a development and a production account, that difference
// is the whole point of the argument.
func (s *Server) poolClients(accountID string, regions []ptypes.AwsRegion) ([]*v3.Client, error) {
	if accountID == "" {
		return s.pool.GetClients(regions...)
	}

	return s.pool.GetAccountClients(ptypes.AwsAccountID(accountID), regions...)
}

// Server wires the AWS client pool and resource cache into an MCP server that
// exposes tools for listing AWS resources.
type Server struct {
	ctx     context.Context
	pool    ClientPool
	cache   *cache.DataCache
	regions *regionCache
	mcp     *server.MCPServer
}

// NewServer builds an MCP server around the given client pool. dc may be nil to
// disable caching.
func NewServer(ctx context.Context, pool ClientPool, dc *cache.DataCache) *Server {
	s := &Server{
		ctx:     ctx,
		pool:    pool,
		cache:   dc,
		regions: newRegionCache(DefaultCacheTTL),
	}

	s.mcp = server.NewMCPServer(
		serverName,
		version.Version,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithLogging(),
	)

	s.registerTools()

	return s
}

// WithRegionTTL sets how long an account's enabled-region list is trusted. It
// tracks the resource cache TTL for the same reason the client-failure cache
// does: both answer "has anything changed since we last looked", and enabling a
// region is a deliberate, rare act. A non-positive value keeps the default.
func (s *Server) WithRegionTTL(ttl time.Duration) *Server {
	s.regions = newRegionCache(ttl)

	return s
}

// MCPServer exposes the underlying mcp-go server, useful for embedding the
// server in a custom transport or test harness.
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcp
}

// The path the MCP server is served on. Clients connect to
// http://<addr>/mcp using the streamable-HTTP transport.
const mcpEndpointPath = "/mcp"

// ServeHTTP starts the MCP server over the streamable-HTTP transport, blocking
// until ctx is cancelled or the server fails. addr is the listen address
// (e.g. ":3040"); the MCP endpoint is exposed at the "/mcp" path. When ctx is
// cancelled the server is shut down gracefully.
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	httpSrv := server.NewStreamableHTTPServer(
		s.mcp,
		server.WithEndpointPath(mcpEndpointPath),
		server.WithHeartbeatInterval(30*time.Second),
	)

	log.Info().
		Str("addr", addr).
		Str("endpoint", mcpEndpointPath).
		Msg("[mcpserver.ServeHTTP] starting MCP streamable-HTTP server")

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Start(addr) }()

	select {
	case <-ctx.Done():
		log.Info().Msg("[mcpserver.ServeHTTP] shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return errors.WithStack(err)
		}
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.WithStack(err)
		}
		return nil
	}
}
