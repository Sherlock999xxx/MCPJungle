// Package connectionmanager provides session management for MCP Jungle,
// handling connections between downstream clients and upstream MCP servers.
// This allows mcpjungle to support long-lived, "keep-alive" connections between mcp clients & servers.
// The connections are managed in-memory and are not persistent across restarts.
package connectionmanager
