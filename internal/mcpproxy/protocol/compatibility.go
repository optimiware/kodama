// Package protocol is the MCP protocol adapter of the proxy. It speaks to remote
// servers as an MCP client and to the workload as an MCP server, both through
// the official Go SDK, and it enforces the approved-destination rules of
// docs/specs/ai-gateway-and-mcp-proxy.md (GM-09): HTTPS only, approved hosts,
// validated DNS answers dialed directly, no redirects, no ambient proxies.
//
// No SDK, HTTP or network type crosses into the mcpproxy package: it receives
// only the RemoteTools port this package implements.
package protocol

import (
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Pinned compatibility target (GM-12). The SDK version is recorded in go.mod and
// verified by the tests against the build info; the protocol revision is the one
// revision this proxy speaks on both legs. The SDK supports newer revisions by
// default, which is not evidence of support here: the proxy narrows both legs to
// this one and rejects a remote that negotiates another.
const (
	// SDKModule and SDKVersion are the pinned official Go SDK release.
	SDKModule  = "github.com/modelcontextprotocol/go-sdk"
	SDKVersion = "v1.8.0"
	// ProtocolRevision is the one supported MCP protocol revision.
	ProtocolRevision = "2025-11-25"
	// Transport is the only supported transport.
	Transport = "streamable-http"
)

// VerifyCompatibility checks that the linked SDK still supports the pinned
// protocol revision. It is a startup and test check, not a negotiation.
func VerifyCompatibility() error {
	if !slices.Contains(mcp.SupportedProtocolVersions(), ProtocolRevision) {
		return fmt.Errorf("protocol: the linked SDK does not support MCP revision %s", ProtocolRevision)
	}
	return nil
}
