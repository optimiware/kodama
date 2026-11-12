//go:build qualification

package production

import (
	"os"
	"testing"
)

// TestMain closes the process-wide recovery port-forward after the last probe,
// so a forward opened by the controller probe keeps the stage API reachable for
// the probes that follow without leaking a child process.
func TestMain(main *testing.M) {
	code := main.Run()
	resilienceCloseSharedAccess()
	os.Exit(code)
}
