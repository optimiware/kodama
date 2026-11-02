package rtest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/test/support/layer"
)

// TestPostgresRuntimeauthContract runs the same contract on the PostgreSQL
// adapter. It is recorded in the `test` environment (T-026) and skips in the
// unit layer and without PostgreSQL.
func TestPostgresRuntimeauthContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	rtest.Contract(t, rtest.OpenPostgres)
}

// TestPostgresIssuedGrantsContract runs the contract of the runtime-issued grant
// records on the PostgreSQL adapter (migration 014_attempt_git.sql).
func TestPostgresIssuedGrantsContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	rtest.IssuedGrantsContract(t, func(t testing.TB) (runtimeauth.IssuedGrants, func(domain.Scope)) {
		s := rtest.New(t, rtest.Options{AttemptGit: true})
		g, err := rtpg.NewIssuedGrants(s.Runtime, s.Clock.Now)
		if err != nil {
			t.Fatal(err)
		}
		return g, s.SeedProject
	})
}
