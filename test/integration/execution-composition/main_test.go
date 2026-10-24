package composition_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Resource prefix of this task's test resources (S3 subprefix, databases).
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", "t062")
	}
	os.Exit(m.Run())
}
