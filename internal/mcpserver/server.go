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
	// ListAccountIDs returns the account IDs the pool can reach.
	ListAccountIDs() ([]ptypes.AwsAccountID, error)
}

// Server wires the AWS client pool and resource cache into an MCP server that
// exposes tools for listing AWS resources.
type Server struct {
	ctx   context.Context
	pool  ClientPool
	cache *cache.DataCache
	mcp   *server.MCPServer
}

// NewServer builds an MCP server around the given client pool. dc may be nil to
// disable caching.
func NewServer(ctx context.Context, pool ClientPool, dc *cache.DataCache) *Server {
	s := &Server{
		ctx:   ctx,
		pool:  pool,
		cache: dc,
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
