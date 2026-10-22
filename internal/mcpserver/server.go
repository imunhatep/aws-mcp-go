// Package mcpserver wires the awslib client pool and resource cache into an MCP
// server that exposes tools for listing AWS resources.
package mcpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
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
func (s *Server) poolClients(ctx context.Context, accountID string, regions []ptypes.AwsRegion) ([]*v3.Client, error) {
	pool, err := s.clientPool(ctx)
	if err != nil {
		return nil, err
	}

	if accountID == "" {
		return pool.GetClients(regions...)
	}

	return pool.GetAccountClients(ptypes.AwsAccountID(accountID), regions...)
}

// clientPool returns the pool a request runs against: the server's own, or — in
// credential-passthrough mode — the one built from the credentials that request
// carried. Every handler must reach AWS through this (via poolClients where it
// can), since in passthrough mode s.pool is nil and the caller's identity lives
// only in the request context.
func (s *Server) clientPool(ctx context.Context) (ClientPool, error) {
	if s.passthrough == nil {
		return s.pool, nil
	}

	req, ok := passthroughFromContext(ctx)
	if !ok || !req.present {
		return nil, errNoPassthroughCredentials
	}

	if req.err != nil {
		return nil, req.err
	}

	return s.passthrough.Pool(req.key, req.creds), nil
}

// Server wires the AWS client pool and resource cache into an MCP server that
// exposes tools for listing AWS resources.
type Server struct {
	ctx     context.Context
	pool    ClientPool
	cache   *cache.DataCache
	regions *regionCache
	mcp     *server.MCPServer

	// login and profiles back the aws_auth_status / aws_sso_login tools. Both
	// are optional: with no login manager the tools still report credential
	// state, they just cannot start a login.
	login    *SSOLoginManager
	profiles []string

	// passthrough is set in credential-passthrough mode, where every request
	// brings its own AWS credentials and pool is nil. See passthrough.go.
	passthrough *PassthroughPools

	// readWrite registers the write tools (write_tools.go). Off by default: a
	// server nobody configured for writes exposes no tool that can change AWS.
	readWrite bool

	// newCloudControl and newTagger override how the write tools build their
	// AWS repositories; nil means awslib's. Tests set them to run the write
	// handlers without AWS.
	newCloudControl func(ctx context.Context, client *v3.Client) cloudControlWriter
	newTagger       func(ctx context.Context, client *v3.Client) resourceTagger
}

// Option configures a Server at construction. It exists for the settings that
// decide which tools are registered and what the initialize instructions say,
// neither of which can change once NewServer has returned.
type Option func(*Server)

// EnableWrites registers the write tools alongside the read-only ones. Without
// it the server exposes no tool that can create, change, tag or delete an AWS
// resource.
func EnableWrites() Option {
	return func(s *Server) { s.readWrite = true }
}

// ReadWrite reports whether the write tools are registered.
func (s *Server) ReadWrite() bool {
	return s.readWrite
}

// readOnlyTool marks a tool as one that never changes its environment.
//
// mcp.NewTool defaults every tool to readOnlyHint=false, destructiveHint=true,
// so without this a client that gates on annotations would treat a resource
// listing exactly like a delete.
func readOnlyTool(tool mcp.Tool) mcp.Tool {
	tool.Annotations.ReadOnlyHint = mcp.ToBoolPtr(true)
	tool.Annotations.DestructiveHint = mcp.ToBoolPtr(false)
	tool.Annotations.IdempotentHint = mcp.ToBoolPtr(true)

	return tool
}

// addReadOnlyTool registers a tool that reads AWS or local state and changes
// neither. Every tool registered in read-only mode goes through this or
// addLocalStateTool; TestReadOnlyModeRegistersOnlyReadOnlyTools pins it.
func (s *Server) addReadOnlyTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	s.mcp.AddTool(readOnlyTool(tool), handler)
}

// addLocalStateTool registers a tool that changes local state on the server
// host — the SSO token cache — but never an AWS resource, which is why it is
// available in read-only mode.
func (s *Server) addLocalStateTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	tool.Annotations.ReadOnlyHint = mcp.ToBoolPtr(false)
	tool.Annotations.DestructiveHint = mcp.ToBoolPtr(false)
	tool.Annotations.IdempotentHint = mcp.ToBoolPtr(true)

	s.mcp.AddTool(tool, handler)
}

// WithPassthrough switches the server to credential-passthrough mode: AWS is
// called with the credentials each request carries in CredentialsHeader, and a
// request without them is refused instead of falling back to anything local.
func (s *Server) WithPassthrough(pools *PassthroughPools) *Server {
	s.passthrough = pools

	return s
}

// WithAuth attaches the SSO login manager and the configured profile names, which
// is what the aws_auth_status and aws_sso_login tools report on. profiles may be
// a single empty string, meaning the default credential chain.
func (s *Server) WithAuth(login *SSOLoginManager, profiles []string) *Server {
	s.login = login
	s.profiles = profiles

	return s
}

// NewServer builds an MCP server around the given client pool. dc may be nil to
// disable caching. With no options the server is read-only: EnableWrites is the
// only way to register a tool that changes AWS.
func NewServer(ctx context.Context, pool ClientPool, dc *cache.DataCache, opts ...Option) *Server {
	s := &Server{
		ctx:     ctx,
		pool:    pool,
		cache:   dc,
		regions: newRegionCache(DefaultCacheTTL),
	}

	for _, opt := range opts {
		opt(s)
	}

	instructions := serverInstructions
	if s.readWrite {
		instructions = readWriteInstructions
	}

	s.mcp = server.NewMCPServer(
		serverName,
		version.Version,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithLogging(),
		server.WithInstructions(instructions),
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

// httpHandler builds the handler mounted at mcpEndpointPath, and returns the
// underlying transport so its sessions can be shut down. In passthrough mode the
// request's credentials are parsed into the context the tool handlers see, and
// expired ones are answered with a 401 so the MCP client refreshes them.
func (s *Server) httpHandler() (http.Handler, *server.StreamableHTTPServer) {
	opts := []server.StreamableHTTPOption{
		server.WithEndpointPath(mcpEndpointPath),
		server.WithHeartbeatInterval(30 * time.Second),
	}

	if s.passthrough != nil {
		opts = append(opts, server.WithHTTPContextFunc(withPassthroughRequest))
	}

	transport := server.NewStreamableHTTPServer(s.mcp, opts...)

	if s.passthrough != nil {
		return refreshExpiredCredentials(transport), transport
	}

	return transport, transport
}

// ServeHTTP starts the MCP server over the streamable-HTTP transport, blocking
// until ctx is cancelled or the server fails. addr is the listen address
// (e.g. ":3040"); the MCP endpoint is exposed at the "/mcp" path. When ctx is
// cancelled the server is shut down gracefully.
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	handler, mcpHandler := s.httpHandler()

	// Routing is done here rather than by the transport's own Start, because in
	// passthrough mode the handler is wrapped before it is mounted.
	mux := http.NewServeMux()
	mux.Handle(mcpEndpointPath, handler)

	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 30 * time.Second}

	log.Info().
		Str("addr", addr).
		Str("endpoint", mcpEndpointPath).
		Bool("credentials_passthrough", s.passthrough != nil).
		Bool("read_write", s.readWrite).
		Msg("[mcpserver.ServeHTTP] starting MCP streamable-HTTP server")

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		log.Info().Msg("[mcpserver.ServeHTTP] shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// The transport owns the MCP sessions and the http.Server owns the
		// listener; both have to be told.
		if err := mcpHandler.Shutdown(shutdownCtx); err != nil {
			log.Warn().Err(err).Msg("[mcpserver.ServeHTTP] closing mcp sessions failed")
		}
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
