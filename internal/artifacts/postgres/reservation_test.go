package postgres_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
)

// TestReservationConcurrency: identical concurrent reservations produce one
// identity; aggregate limits stay atomic under concurrent reservations of one
// attempt; limits of different roles and attempts are independent.
func TestReservationConcurrency(t *testing.T) {
	env := artifactstest.NewDB(t, artifactstest.Options{RuntimeConns: 12})
	ctx := env.Ctx()
	store := env.Store

	t.Run("identical concurrent requests produce one identity", func(t *testing.T) {
		const n = 16
		var created atomic.Int32
		ids := make(chan domain.ArtifactID, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a := fileArtifact(t, scopeA, "att_same", "report", []byte("same bytes"), artifacts.RoleOutput)
				<-start
				res, err := store.Reserve(ctx, reserveCmd(a, bigOK))
				if err != nil {
					t.Error(err)
					return
				}
				if res.Created {
					created.Add(1)
				}
				ids <- res.Artifact.ID()
			}()
		}
		close(start)
		wg.Wait()
		close(ids)
		first := domain.ArtifactID("")
		for id := range ids {
			if first == "" {
				first = id
			}
			if id != first {
				t.Fatalf("two identities for one reservation: %s and %s", first, id)
			}
		}
		if created.Load() != 1 {
			t.Fatalf("%d requests created the artifact", created.Load())
		}
		if got := env.Count(`SELECT count(*) FROM artifacts WHERE attempt_id = 'att_same'`) + env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1`, string(first)); got != 2 {
			t.Fatalf("rows for one reservation: %d (want 1 artifact + 1 staging record)", got)
		}
		if got := env.Count(`SELECT count(*) FROM audit WHERE resource_id = $1`, string(first)); got != 1 {
			t.Fatalf("%d audit records for one creation", got)
		}
	})

	t.Run("a repeat with different metadata sees the stored declaration", func(t *testing.T) {
		other := fileArtifact(t, scopeA, "att_same", "report", []byte("DIFFERENT"), artifacts.RoleOutput)
		res, err := store.Reserve(ctx, reserveCmd(other, bigOK))
		if err != nil || res.Created {
			t.Fatalf("a second reservation must not be created: %+v %v", res, err)
		}
		if res.Artifact.Matches(other.Reservation()) {
			t.Fatal("the stored declaration must differ so the caller can report the conflict")
		}
	})

	t.Run("file count limit is atomic", func(t *testing.T) {
		lim := artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 3, DiagnosticBytes: 1 << 20, DiagnosticFiles: 3}
		results := runParallel(12, func(i int) error {
			a := fileArtifact(t, scopeA, "att_count", fmt.Sprintf("out-%d", i), []byte("x"), artifacts.RoleOutput)
			_, err := store.Reserve(ctx, reserveCmd(a, lim))
			return err
		})
		assertLimit(t, results, 3, 12)
		if n := env.Count(`SELECT count(*) FROM artifacts WHERE attempt_id = 'att_count'`); n != 3 {
			t.Fatalf("%d rows stored", n)
		}
	})

	t.Run("byte limit is atomic", func(t *testing.T) {
		lim := artifacts.Limits{OutputBytes: 10, OutputFiles: 100}
		results := runParallel(10, func(i int) error {
			a := fileArtifact(t, scopeA, "att_bytes", fmt.Sprintf("out-%d", i), []byte("1234"), artifacts.RoleOutput)
			_, err := store.Reserve(ctx, reserveCmd(a, lim))
			return err
		})
		assertLimit(t, results, 2, 10)
		if n := env.Count(`SELECT coalesce(sum(size_bytes), 0) FROM artifacts WHERE attempt_id = 'att_bytes'`); n != 8 {
			t.Fatalf("%d bytes reserved", n)
		}
	})

	t.Run("roles and attempts are limited independently", func(t *testing.T) {
		lim := artifacts.Limits{OutputBytes: 5, OutputFiles: 1, DiagnosticBytes: 5, DiagnosticFiles: 1, BundleBytes: 5, BundleFiles: 1}
		for _, role := range []artifacts.Role{artifacts.RoleOutput, artifacts.RoleDiagnostic, artifacts.RoleBundle} {
			a := fileArtifact(t, scopeA, "att_roles", "first-"+string(role), []byte("12345"), role)
			if _, err := store.Reserve(ctx, reserveCmd(a, lim)); err != nil {
				t.Fatalf("%s: %v", role, err)
			}
			b := fileArtifact(t, scopeA, "att_roles", "second-"+string(role), []byte("1"), role)
			if _, err := store.Reserve(ctx, reserveCmd(b, lim)); !errors.Is(err, artifacts.ErrLimitExceeded) {
				t.Fatalf("%s second: %v", role, err)
			}
		}
		other := fileArtifact(t, scopeA, "att_roles_2", "first-output", []byte("12345"), artifacts.RoleOutput)
		if _, err := store.Reserve(ctx, reserveCmd(other, lim)); err != nil {
			t.Fatalf("another attempt has its own limit: %v", err)
		}
		// Zero limits permit nothing.
		z := fileArtifact(t, scopeA, "att_zero", "x", []byte(""), artifacts.RoleOutput)
		if _, err := store.Reserve(ctx, reserveCmd(z, artifacts.Limits{})); !errors.Is(err, artifacts.ErrLimitExceeded) {
			t.Fatalf("zero limit: %v", err)
		}
	})

	t.Run("an abandoned reservation frees its allowance", func(t *testing.T) {
		lim := artifacts.Limits{OutputBytes: 100, OutputFiles: 1}
		a := fileArtifact(t, scopeA, "att_free", "one", []byte("x"), artifacts.RoleOutput)
		if _, err := store.Reserve(ctx, reserveCmd(a, lim)); err != nil {
			t.Fatal(err)
		}
		b := fileArtifact(t, scopeA, "att_free", "two", []byte("x"), artifacts.RoleOutput)
		if _, err := store.Reserve(ctx, reserveCmd(b, lim)); !errors.Is(err, artifacts.ErrLimitExceeded) {
			t.Fatalf("limit: %v", err)
		}
		env.Exec(`UPDATE artifacts SET disposition = 'abandoned', version = version + 1 WHERE id = $1`, string(a.ID()))
		if _, err := store.Reserve(ctx, reserveCmd(b, lim)); err != nil {
			t.Fatalf("an abandoned reservation must not count: %v", err)
		}
	})

	t.Run("concurrent attempts do not block each other's limits", func(t *testing.T) {
		lim := artifacts.Limits{OutputBytes: 100, OutputFiles: 2}
		results := runParallel(8, func(i int) error {
			a := fileArtifact(t, scopeA, domain.AttemptID(fmt.Sprintf("att_par_%d", i)), "out", []byte("x"), artifacts.RoleOutput)
			_, err := store.Reserve(ctx, reserveCmd(a, lim))
			return err
		})
		for i, err := range results {
			if err != nil {
				t.Errorf("attempt %d: %v", i, err)
			}
		}
	})
}

func runParallel(n int, fn func(i int) error) []error {
	out := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i] = fn(i)
		}()
	}
	close(start)
	wg.Wait()
	return out
}

func assertLimit(t *testing.T, results []error, wantOK, total int) {
	t.Helper()
	ok, limited := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, artifacts.ErrLimitExceeded):
			limited++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != wantOK || ok+limited != total {
		t.Fatalf("%d accepted, %d refused of %d (want %d accepted)", ok, limited, total, wantOK)
	}
}
