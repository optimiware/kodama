// Package assembly_execution_test is the integrated acceptance of the execution
// assembly (T-071's three transferred scenarios, owned by T-026): the actual
// composition (internal/app, KODAMA_ENV=test) over real PostgreSQL, JetStream,
// S3, Git and OIDC, running the real `kodama step` entrypoint in real containers
// whose callbacks (admission, context, artifact upload, result, token renewal)
// reach the real runtime API. The fixture is the M1 kit (test/e2e/m1/kit).
//
// The scenarios are written against a backend table: the Podman runtime is the
// backend of this checkpoint, and T-089 adds the Kubernetes backend to the same
// table so these tests run again, unchanged, against it.
package assembly_execution_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

func TestMain(m *testing.M) { os.Exit(kit.Main(m)) }

// backend is one workload runtime under test.
type backend struct {
	// Name is the runtime's backend identity (execution.Runtime.Capabilities).
	Name string
}

// backends lists the runtimes the scenarios run against.
var backends = []backend{{Name: "podman"}}

func mustJSON(t testing.TB, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
}
