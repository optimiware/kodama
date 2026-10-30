// Package mcpserver implements `kodama mcp serve`, the OAuth-protected MCP
// resource server over the Kodama public HTTP API.
//
// It is deliberately not the in-product MCP proxy: the proxy exposes approved
// external tools to Kodama attempts, while this package exposes Kodama's own
// public operations to external MCP clients. It exchanges the MCP-bound token
// for a separate delegated API token, holds no database/broker/KMS credential,
// and stores no access token.
package mcpserver
