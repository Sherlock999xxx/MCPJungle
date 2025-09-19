package connectionmanager

import (
	"context"
	"fmt"
	"log"
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

// SSEConnectionManager manages long-lived SSE connections for mcpjungle.
// It keeps track of all connections with downstream sse clients and upstream sse servers.
// It also maps the clients to servers to ensure that:
//   - any request sent by client reaches the server
//   - any response or notifications send by server reached the client.
type SSEConnectionManager struct {
	mu       sync.Mutex
	sessions map[string]upstreamMCPServerConnections
}

// ErrSessionNotFound is returned when a session is not found in the session manager.
var ErrSessionNotFound = fmt.Errorf("session not found")

func NewSSEConnectionManager() *SSEConnectionManager {
	return &SSEConnectionManager{
		sessions: make(map[string]upstreamMCPServerConnections),
	}
}

// OnRegisterSession is the callback function called by mcp-go hook when a new client registers with mcpjungle SSE mcp.
// When a client just registers, it is not possible for mcpjungle to know which upstream server it wants to connect to.
// So no connection is created at this stage. mcpjungle simply starts tracking the session.
func (m *SSEConnectionManager) OnRegisterSession(ctx context.Context, sess server.ClientSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sess.SessionID()] = make(upstreamMCPServerConnections)
}

// OnUnregisterSession is the callback function called by mcp-go hook when a client deregisters from mcpjungle SSE mcp.
// It closes all upstream MCP server connections associated with this downstream client session.
func (m *SSEConnectionManager) OnUnregisterSession(ctx context.Context, sess server.ClientSession) {
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

// GetClient returns an existing connection that proxies messages between the given upstream MCP server and downstream client session.
// If no such connection exists, this method creates a new one and caches it for subsequent requests.
// The caller should not close the returned client. The session manager is responsible for the client's lifecycle.
func (m *SSEConnectionManager) GetClient(clientSessionID string, mcpServerModel *model.McpServer, mcpServer *server.MCPServer) (*client.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sessConns, ok := m.sessions[clientSessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}

	cli, ok := sessConns[mcpServerModel.Name]
	if ok {
		return cli, nil
	}

	// create a new connection
	cli, err := m.createSseClient(clientSessionID, mcpServerModel, mcpServer)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new sse client: %w", err)
	}

	// cache the client for subsequent use
	sessConns[mcpServerModel.Name] = cli

	return cli, nil
}

func (m *SSEConnectionManager) createSseClient(sessionID string, mcpServerModel *model.McpServer, mcpServer *server.MCPServer) (*client.Client, error) {
	conf, err := mcpServerModel.GetSSEConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get SSE transport config for MCP server: %w", err)
	}

	var opts []transport.ClientOption

	// If bearer token is provided, set the Authorization header
	if conf.BearerToken != "" {
		o := transport.WithHeaders(map[string]string{
			"Authorization": "Bearer " + conf.BearerToken,
		})
		opts = append(opts, o)
	}

	cli, err := client.NewSSEMCPClient(conf.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSE client for MCP server: %w", err)
	}

	// when the client receives a notification from the upstream server,
	// forward it to the downstream client session that the notification is meant for.
	cli.OnNotification(func(notification mcp.JSONRPCNotification) {
		err := mcpServer.SendNotificationToSpecificClient(sessionID, notification.Method, notification.Params.AdditionalFields)
		if err != nil {
			log.Printf(
				"failed to send notification to downstream client (session: %s, mcp server: %s): %v\n",
				sessionID,
				mcpServerModel.Name,
				err,
			)
		}
	})

	// use the background context to create the connection to avoid any ctx from closing the conn due to cancellation.
	// The client connection should remain valid until the session is unregistered.
	// The session manager will close the client then.
	if err = cli.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to start the SSE client for upstream MCP server: %w", err)
	}

	initReq := mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: "2025-06-18",
			Capabilities:    mcp.ClientCapabilities{},
			ClientInfo:      mcp.Implementation{Name: "mcpjungle-sse-proxy-client", Version: "0.1.0"},
		},
	}
	_, err = cli.Initialize(context.Background(), initReq)
	if err != nil {
		return nil, fmt.Errorf("client failed to initialize connection with SSE MCP server: %w", err)
	}

	return cli, nil
}
