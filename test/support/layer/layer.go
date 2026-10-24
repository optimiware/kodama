// Package layer marks the infrastructure-free unit layer.
//
// `make check-task` builds tests with `-tags unit`. Every fixture that needs a
// real service (PostgreSQL, NATS, S3, OIDC, registry, container runtime) calls
// SkipService first, so the unit layer can never start or reach one, even when
// the caller's environment points at a service or marks it required. Skipped
// service tests are not acceptance evidence. Passing unit acceptance qualifies
// component tasks; real-service acceptance belongs to explicit checkpoints.
package layer

import "testing"

// SkipService skips the test in the unit layer.
func SkipService(t testing.TB, service string) {
	t.Helper()
	if Unit {
		t.Skipf("%s needs a real service: skipped in the unit layer (-tags unit)", service)
	}
}
