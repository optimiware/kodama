package postgres_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Resource prefix of this task's test resources.
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", "t033")
	}
	os.Exit(m.Run())
}
