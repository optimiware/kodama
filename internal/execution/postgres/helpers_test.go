package postgres_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const (
	readRun  = "workspace: {access: read, from: run}"
	writeRun = "workspace: {access: write, from: run}"
)

// claimAll runs rounds of n concurrent ClaimRunnable calls for the tenant until
// a round claims nothing, and returns every intent that won. Callers that lose
// to a contended run row skip it (ErrBusy), so one round may claim fewer steps
// than are runnable; no step is ever claimed twice.
func claimAll(t *testing.T, e *executiontest.Env, tenant domain.TenantID, n int) []*execution.AttemptSpec {
	t.Helper()
	var all []*execution.AttemptSpec
	for round := 0; round < 20; round++ {
		won := claimRound(t, e, tenant, n)
		if len(won) == 0 {
			return all
		}
		all = append(all, won...)
	}
	t.Fatal("claims never became idle")
	return nil
}

func claimRound(t *testing.T, e *executiontest.Env, tenant domain.TenantID, n int) []*execution.AttemptSpec {
	t.Helper()
	var mu sync.Mutex
	var won []*execution.AttemptSpec
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, err := e.Claim(tenant)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won = append(won, in)
			case errors.Is(err, execution.ErrNothingRunnable):
			default:
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	return won
}

func reservationsHeld(e *executiontest.Env) int {
	return e.Count(`SELECT count(*) FROM execution_reservations WHERE released_at IS NULL`)
}

func attemptCount(e *executiontest.Env, run domain.RunID) int {
	return e.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run))
}

// job builds a container step without the shared fixture's default timeout, so
// tests control timeoutSeconds themselves.
func job(id string, extra ...string) string {
	s := "{id: " + id + ", kind: container, image: " + pipelinetest.Image + ", command: [/bin/sh, -ec]"
	for _, x := range extra {
		s += ", " + x
	}
	return s + "}"
}

func ptr[T any](v T) *T { return &v }

type pgxTx = pgx.Tx
