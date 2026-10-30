package postgres_test

import (
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

const lineageRef = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

func shared(t testing.TB, name string, p artifacts.Provenance) artifacts.Artifact {
	t.Helper()
	data := []byte(name)
	size := int64(len(data))
	id := artifacts.RandomIDs{}.NewArtifactID()
	now := at()
	res := artifacts.Reservation{Scope: scopeA, Attempt: "att_shared", Run: "run_1", Step: "build", Name: name, Kind: artifacts.KindFile, Role: artifacts.RoleOutput,
		Digest: artifacts.DigestOf(data), MediaType: "text/plain", Size: &size, Provenance: p}
	a, err := artifacts.NewFile(id, res, artifacts.ObjectKey(scopeA, id, artifacts.SegmentStaging, name), now.Add(60e9), now)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestWorkspaceProvenanceRoundTrip: the workspace state the server recorded for a
// run_shared producer is kept with the artifact (write-once, tenant-scoped) and
// returned by every read; an artifact without it reads as before; a database that
// never applied the migration refuses to record it.
func TestWorkspaceProvenanceRoundTrip(t *testing.T) {
	env := artifactstest.NewDB(t, artifactstest.Options{Extra: []storage.Contribution{artifactspg.ProvenanceMigrations()}})
	ctx := env.Ctx()
	src := artifactstest.Source
	for _, tc := range []struct {
		name string
		p    artifacts.Provenance
	}{
		{"clean", artifacts.Provenance{RepositoryURL: src.RepositoryURL, Commit: src.Commit, WorkspaceState: artifacts.WorkspaceClean, Lineage: lineageRef}},
		{"modified", artifacts.Provenance{RepositoryURL: src.RepositoryURL, Commit: src.Commit, WorkspaceState: artifacts.WorkspaceModified, IgnoredPresent: true, Lineage: lineageRef}},
		{"private", src},
	} {
		a := shared(t, tc.name, tc.p)
		res, err := env.Store.Reserve(ctx, reserveCmd(a, bigOK))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for what, get := range map[string]func() (artifacts.Artifact, error){
			"get":    func() (artifacts.Artifact, error) { return env.Store.Get(ctx, scopeA, a.ID()) },
			"byname": func() (artifacts.Artifact, error) { return env.Store.FindByName(ctx, scopeA, "att_shared", tc.name) },
			"replay": func() (artifacts.Artifact, error) {
				r, err := env.Store.Reserve(ctx, reserveCmd(shared(t, tc.name, tc.p), bigOK))
				return r.Artifact, err
			},
		} {
			got, err := get()
			if err != nil || got.Provenance() != tc.p {
				t.Fatalf("%s/%s: provenance %+v, %v; want %+v", tc.name, what, got.Provenance(), err, tc.p)
			}
		}
		if res.Artifact.Provenance() != tc.p {
			t.Fatalf("%s: reserved with %+v", tc.name, res.Artifact.Provenance())
		}
	}
	// Another tenant's runtime session reads nothing of it.
	if _, err := env.Store.Get(ctx, scopeB, shared(t, "x", src).ID()); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("a foreign read: %v", err)
	}
	// The record is write-once.
	for _, stmt := range []string{
		`UPDATE artifact_workspace_provenance SET workspace_state = 'clean'`,
		`DELETE FROM artifact_workspace_provenance`,
	} {
		if _, err := env.Owner.Exec(ctx, stmt); err == nil {
			t.Fatalf("%s succeeded", stmt)
		}
	}
}

// TestWorkspaceProvenanceNeedsItsMigration: over a database without the table an
// attempt_private artifact reserves and reads exactly as before, and a
// run_shared producer's state is refused rather than dropped.
func TestWorkspaceProvenanceNeedsItsMigration(t *testing.T) {
	env := artifactstest.NewDB(t, artifactstest.Options{})
	ctx := env.Ctx()
	src := artifactstest.Source
	if _, err := env.Store.Reserve(ctx, reserveCmd(shared(t, "plain", src), bigOK)); err != nil {
		t.Fatalf("an attempt_private artifact: %v", err)
	}
	p := artifacts.Provenance{RepositoryURL: src.RepositoryURL, Commit: src.Commit, WorkspaceState: artifacts.WorkspaceClean}
	if _, err := env.Store.Reserve(ctx, reserveCmd(shared(t, "kept", p), bigOK)); !errors.Is(err, artifacts.ErrUnavailable) {
		t.Fatalf("a run_shared artifact without the table: %v", err)
	}
	if _, err := env.Store.FindByName(ctx, scopeA, "att_shared", "kept"); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("the refused reservation left a row: %v", err)
	}
}
