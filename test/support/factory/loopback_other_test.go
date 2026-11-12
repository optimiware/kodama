//go:build !linux

package factory

import (
	"fmt"
	"os"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// ensureLoopback is only implemented on Linux. Elsewhere the loopback-only
// relay tests follow the shared infrastructure policy instead of assuming a
// platform-specific interface setup.
func ensureLoopback(t testing.TB) {
	t.Helper()
	if loopbackUsable() {
		return
	}
	harness.SkipOrFail(t, os.Getenv("KODAMA_REQUIRE_INFRA") == "1",
		fmt.Errorf("loopback is unusable and cannot be enabled on this platform"))
}
