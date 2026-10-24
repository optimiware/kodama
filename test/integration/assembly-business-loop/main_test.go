package assemblybusinessloop_test

import (
	"flag"
	"os"
	"testing"

	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// The checks of this package run gate pipelines on the pinned Alpine image and
// need neither the deployment-tools image nor a cluster. The M2 kit's Main
// builds and pushes both when the whole suite runs (no -run filter) and removes
// them by name when the process ends; the e2e package of the same check builds
// and pushes its own under the same names, so the package that finishes first
// would remove what the other still runs. A filter that matches every test of
// the package keeps the build on demand (never, here).
func TestMain(m *testing.M) {
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() == "" {
		_ = f.Value.Set("^Test")
	}
	os.Exit(m2.Main(m))
}
