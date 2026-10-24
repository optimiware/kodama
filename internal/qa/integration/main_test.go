// Package integration_test holds the QA mapping's component tests over real
// PostgreSQL and S3: the real execution, artifact, delivery and work stores and
// the module's real adapters. Only the browser/model (the test writing the
// report and screenshots) and fault injection at owner boundaries are
// controlled. They prove the mapping and authority behavior, not the real
// browser image or profile (T-069).
//
// Resource prefix t040.
package integration_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", "t040")
	}
	os.Exit(m.Run())
}
