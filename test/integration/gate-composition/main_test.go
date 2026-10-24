package gatecomposition_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Resource prefix of this task's test resources (databases, S3 subprefix).
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", "t073")
	}
	os.Exit(m.Run())
}
