package postgres_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/logstest"
	"github.com/optimiware/kodama/test/support/layer"
)

// pgWorld is the PostgreSQL composition's fixtures for the shared Contract:
// real execution attempts and the wrapper's runtime upload through the real
// artifact service.
type pgWorld struct{ e *logstest.Env }

func (w pgWorld) Attempt(tb testing.TB, scope domain.Scope) logs.AttemptInfo {
	tb.Helper()
	return w.e.Info(w.e.NewAttempt(scope, true))
}

func (w pgWorld) Upload(tb testing.TB, a logs.AttemptInfo, name, media string, data []byte) domain.ArtifactID {
	tb.Helper()
	return w.e.WrapperUpload(logstest.Attempt{Scope: a.Scope, Run: a.Run, ID: a.ID}, name, media, data)
}

func (w pgWorld) Audits(tb testing.TB, scope domain.Scope, id logs.RecordID) int {
	tb.Helper()
	return w.e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'log_record' AND resource_id = $1`, string(id))
}

// Remove reports false: retention of real objects is the artifact module's
// reconciler, exercised by its own checkpoint.
func (pgWorld) Remove(testing.TB, logs.AttemptInfo, domain.ArtifactID) bool { return false }

// TestPostgresContract runs the same Contract as the memory adapters against
// the real PostgreSQL store and artifact-backed retained logs (SD-13, SD-A11).
// It needs the `test` environment's services and skips in the unit layer.
func TestPostgresContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	logstest.Contract(t, func(tb testing.TB) logstest.Subject {
		e := logstest.New(tb)
		// The scopes are the ones the execution fixtures seeded (tenants,
		// projects and the engineer's memberships), not the memory subject's.
		return logstest.Subject{Store: e.Logs, Retained: e.Retained, World: pgWorld{e},
			Scopes: logstest.Scopes{A: logstest.ScopeA, A2: logstest.ScopeA2, B: logstest.ScopeB}}
	})
}
