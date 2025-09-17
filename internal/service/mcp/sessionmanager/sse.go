package sessionmanager

import (
	"context"
	"sync"

	"github.com/mark3labs/mcp-go/server"
)

// SSESessionManager manages SSE sessions for mcpjungle.
// It keeps track of all connections with downstream sse clients and upstream sse servers.
// It also maps the downstream clients to upstream servers.
type SSESessionManager struct {
	mu       sync.Mutex
	sessions map[string]struct{} // session id -> blank
}

func NewSSESessionManager() *SSESessionManager {
	return &SSESessionManager{
		sessions: make(map[string]struct{}),
	}
}

// OnRegisterSession is the callback function called by mcp-go hook when a new client registers with mcpjungle SSE mcp.
func (m *SSESessionManager) OnRegisterSession(ctx context.Context, sess server.ClientSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sess.SessionID()] = struct{}{}
}

// OnUnregisterSession is the callback function called by mcp-go hook when a client deregisters from mcpjungle SSE mcp.
func (m *SSESessionManager) OnUnregisterSession(ctx context.Context, sess server.ClientSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sess.SessionID())
}
