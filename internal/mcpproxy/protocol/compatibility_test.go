package protocol_test

import (
	"runtime/debug"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimiware/kodama/internal/mcpproxy/protocol"
)

// Test_compatibility_pin: the recorded SDK version is the one linked, the
// pinned protocol revision is supported by it, and the proxy narrows both legs
// to that single revision instead of inheriting the SDK's newest default.
func Test_compatibility_pin(t *testing.T) {
	if err := protocol.VerifyCompatibility(); err != nil {
		t.Fatal(err)
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info")
	}
	found := false
	for _, m := range info.Deps {
		if m.Path == protocol.SDKModule {
			found = true
			if m.Version != protocol.SDKVersion {
				t.Fatalf("linked SDK %s, recorded pin %s", m.Version, protocol.SDKVersion)
			}
		}
	}
	if !found {
		t.Fatal("the SDK is not linked")
	}
	supported := mcp.SupportedProtocolVersions()
	if !slices.Contains(supported, protocol.ProtocolRevision) {
		t.Fatalf("SDK supports %v", supported)
	}
	if supported[0] == protocol.ProtocolRevision {
		t.Log("the pinned revision is also the SDK's newest")
	} else {
		t.Logf("the SDK's newest revision %s is deliberately not used; the proxy pins %s", supported[0], protocol.ProtocolRevision)
	}
}
