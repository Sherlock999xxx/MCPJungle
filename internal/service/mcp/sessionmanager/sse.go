package sessionmanager

import (
	"context"
	"fmt"
	"sync"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/mcpjungle/mcpjungle/internal/model"
)

// upstreamMCPServerConnections maps MCP server names to the connection objects that lets us interact them.
// One map manages connections for one downstream client session.
type upstreamMCPServerConnections map[string]*client.Client

// SSESessionManager manages SSE sessions for mcpjungle.
// It keeps track of all connections with downstream sse clients and upstream sse servers.
// It also maps the downstream clients to upstream servers.
type SSESessionManager struct {
	mu       sync.Mutex
	sessions map[string]upstreamMCPServerConnections
}

// ErrSessionNotFound is returned when a session is not found in the session manager.
var ErrSessionNotFound = fmt.Errorf("session not found")

func NewSSESessionManager() *SSESessionManager {
	return &SSESessionManager{
		sessions: make(map[string]upstreamMCPServerConnections),
	}
}

// OnRegisterSession is the callback function called by mcp-go hook when a new client registers with mcpjungle SSE mcp.
func (m *SSESessionManager) OnRegisterSession(ctx context.Context, sess server.ClientSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sess.SessionID()] = make(upstreamMCPServerConnections)
}

// OnUnregisterSession is the callback function called by mcp-go hook when a client deregisters from mcpjungle SSE mcp.
// It closes all upstream MCP server connections associated with this downstream client session.
func (m *SSESessionManager) OnUnregisterSession(ctx context.Context, sess server.ClientSession) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.sessions[sess.SessionID()]; !ok {
		// session not found, nothing to do
		return
	}

	// close all upstream MCP server connections associated with this downstream client session
	for _, cli := range m.sessions[sess.SessionID()] {
		_ = cli.Close()
		// TODO: if there's an error, log it but continue closing the others
	}

	delete(m.sessions, sess.SessionID())
}

// GetClient returns an existing connection to the given MCP server for the given downstream client session.
// If no such connection exists, it creates a new one and caches it for subsequent calls.
// The caller should not close the returned client. The session manager will close it when the downstream
// client session is unregistered.
func (m *SSESessionManager) GetClient(ctx context.Context, sessionID string, mcpServer *model.McpServer) (*client.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sessConns, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}

	cli, ok := sessConns[mcpServer.Name]
	if ok {
		return cli, nil
	}

	// create a new connection
	cli, err := m.createSseClient(ctx, mcpServer)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new sse client: %w", err)
	}

	// cache the client for subsequent use
	sessConns[mcpServer.Name] = cli

	return cli, nil
}

func (m *SSESessionManager) createSseClient(ctx context.Context, mcpServer *model.McpServer) (*client.Client, error) {
	conf, err := mcpServer.GetSSEConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get SSE transport config for MCP server: %w", err)
	}

	var opts []transport.ClientOption
	if conf.BearerToken != "" {
		// If bearer token is provided, set the Authorization header
		o := transport.WithHeaders(map[string]string{
			"Authorization": "Bearer " + conf.BearerToken,
		})
		opts = append(opts, o)
	}

	c, err := client.NewSSEMCPClient(conf.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSE client for MCP server: %w", err)
	}

	c.OnNotification(func(notification mcp.JSONRPCNotification) {
		// TODO: send notification to downstream client session id
	})

	if err = c.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start SSE transport for MCP server: %w", err)
	}

	initReq := mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: "2025-06-18",
			Capabilities:    mcp.ClientCapabilities{},
			ClientInfo:      mcp.Implementation{Name: "mcpjungle-sse-proxy-client", Version: "0.1.0"},
		},
	}
	_, err = c.Initialize(ctx, initReq)
	if err != nil {
		return nil, fmt.Errorf("client failed to initialize connection with SSE MCP server: %w", err)
	}

	return c, nil
}
