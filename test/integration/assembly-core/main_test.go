package assembly_test

import (
	"os"
	"testing"

	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

func TestMain(m *testing.M) {
	// Resource prefix of this task's test resources: the S3 subprefix, database
	// names' companions, tenant and project identifiers and the JetStream stream.
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", "t070")
	}
	// kit.Main requires the services the acceptance names (a missing one
	// fails instead of skipping) and hosts the object-store forward the real
	// container steps of core_assembly_current_runtime need.
	os.Exit(kit.Main(m))
}
