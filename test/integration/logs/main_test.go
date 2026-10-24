package logs_test

import (
	"os"
	"testing"
)

// Prefix is this task's resource prefix: namespaces, images, fixture
// identifiers and S3 subprefixes carry it.
const Prefix = "t064"

// TestMain makes the real services mandatory (a missing PostgreSQL, S3 or
// Kubernetes fails the tests instead of skipping them) and removes this
// invocation's fixture resources afterwards.
func TestMain(m *testing.M) {
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", Prefix)
	}
	for _, k := range []string{"KODAMA_REQUIRE_POSTGRES", "KODAMA_REQUIRE_S3", "KODAMA_REQUIRE_KUBERNETES"} {
		if os.Getenv(k) == "" {
			os.Setenv(k, "1")
		}
	}
	code := m.Run()
	runCleanups()
	os.Exit(code)
}
