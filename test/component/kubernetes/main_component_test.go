//go:build component

package kubernetesconformance

import (
	"os"
	"testing"

	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
)

// TestMain installs kind-cluster signal cleanup and the next-run dead-owner
// sweep. It never downloads kind during cleanup and never touches a cluster
// outside the generated kodama-t-<pid>-<random> namespace, so the existing
// kodama-probe cluster and concurrent shards survive.
func TestMain(m *testing.M) {
	os.Exit(kubesupport.Main(m))
}

// Scope note (documented, not a test): tenant NetworkPolicy allow/deny and
// resource-limit enforcement are not exercised here. They require the deployed
// tenant isolation and qualification environment. This suite covers the
// runtime driver and adapter behaviour only.
