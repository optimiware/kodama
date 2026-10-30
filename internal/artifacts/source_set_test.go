package artifacts_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	setDigest = "sha256:" + strings.Repeat("d", 64)
	commitX   = strings.Repeat("1", 40)
	commitY   = strings.Repeat("2", 40)
)

func twoSources(t *testing.T) artifacts.SourceSet {
	t.Helper()
	set, err := artifacts.NewSourceSet(setDigest, []artifacts.SourceCommit{{RepositoryID: "repo_web", Commit: commitY}, {RepositoryID: "repo_api", Commit: commitX}})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// Test_unit_artifact_provenance_source_set (DQ-17, DQ-A13): the provenance of an
// artifact of a multi-repository run is the digest of the run's source set and
// one commit per repository, never a single repository URL and commit; it is
// canonical, validated, immutable with the artifact and part of no replay key.
func Test_unit_artifact_provenance_source_set(t *testing.T) {
	set := twoSources(t)
	setProv := artifacts.Provenance{SourceSet: set}

	t.Run("a source set is canonical: ordered by repository, one entry each, comparable", func(t *testing.T) {
		got := set.Commits()
		if len(got) != 2 || got[0].RepositoryID != "repo_api" || got[0].Commit != commitX || got[1].RepositoryID != "repo_web" || got[1].Commit != commitY {
			t.Fatalf("commits: %+v", got)
		}
		reordered, err := artifacts.NewSourceSet(setDigest, []artifacts.SourceCommit{{RepositoryID: "repo_api", Commit: commitX}, {RepositoryID: "repo_web", Commit: commitY}})
		if err != nil || reordered != set {
			t.Fatalf("the input order is not part of the identity: %v", err)
		}
		other, _ := artifacts.NewSourceSet(setDigest, []artifacts.SourceCommit{{RepositoryID: "repo_api", Commit: commitY}, {RepositoryID: "repo_web", Commit: commitY}})
		if other == set {
			t.Fatal("a different commit is a different set")
		}
		if (artifacts.SourceSet{}).Commits() != nil || !(artifacts.SourceSet{}).IsZero() || set.IsZero() {
			t.Fatal("the zero set is none")
		}
	})

	t.Run("a malformed source set is refused", func(t *testing.T) {
		ok := []artifacts.SourceCommit{{RepositoryID: "repo_api", Commit: commitX}}
		many := make([]artifacts.SourceCommit, artifacts.MaxSourceCommits+1)
		for i := range many {
			many[i] = artifacts.SourceCommit{RepositoryID: "repo_" + strings.Repeat("a", i+1), Commit: commitX}
		}
		for name, c := range map[string]struct {
			digest  string
			commits []artifacts.SourceCommit
		}{
			"no digest":          {"", ok},
			"a short digest":     {"sha256:abc", ok},
			"no commits":         {setDigest, nil},
			"a duplicate repo":   {setDigest, []artifacts.SourceCommit{ok[0], ok[0]}},
			"a branch as commit": {setDigest, []artifacts.SourceCommit{{RepositoryID: "repo_api", Commit: "main"}}},
			"a bad repository":   {setDigest, []artifacts.SourceCommit{{RepositoryID: "repo/api", Commit: commitX}}},
			"too many commits":   {setDigest, many},
		} {
			if _, err := artifacts.NewSourceSet(c.digest, c.commits); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := (artifacts.SourceSet{Digest: setDigest}).Validate(); err == nil {
			t.Error("a set that was not built canonically is invalid")
		}
	})

	t.Run("provenance is nothing, one source or one source set", func(t *testing.T) {
		if err := setProv.Validate(); err != nil {
			t.Fatal(err)
		}
		if setProv.IsZero() || (artifacts.Provenance{}).Validate() != nil || prov.Validate() != nil {
			t.Fatal("zero, single and set provenance are valid")
		}
		both := setProv
		both.RepositoryURL, both.Commit = prov.RepositoryURL, prov.Commit
		if err := both.Validate(); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("a single source beside a source set: %v", err)
		}
		half := artifacts.Provenance{SourceSet: set, Commit: commitX}
		if err := half.Validate(); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("half a single source beside a set: %v", err)
		}
	})

	t.Run("file and OCI artifacts of a source-set run carry the set", func(t *testing.T) {
		f := fileRes()
		f.Provenance = setProv
		file, err := artifacts.NewFile("art_set_f", f, "artifacts/t_a/p_1/art_set_f/staging/n", t0.Add(60e9), t0)
		if err != nil || file.Provenance() != setProv {
			t.Fatalf("file: %v %+v", err, file.Provenance())
		}
		o := ociRes()
		o.Provenance = setProv
		v := artifacts.OCIVerification{Repository: "registry.example.test/team/app", Digest: digest, ManifestSize: 100, Provenance: setProv}
		img, err := artifacts.NewRegistered("art_set_o", o, v, t0)
		if err != nil || !img.Usable() || img.Provenance() != setProv || img.Reference().Provenance != setProv {
			t.Fatalf("registration: %v %+v", err, img.Provenance())
		}
		// The registry evidence must name the same set the registration binds.
		moved := v
		moved.Provenance = artifacts.Provenance{SourceSet: func() artifacts.SourceSet {
			s, _ := artifacts.NewSourceSet(setDigest, []artifacts.SourceCommit{{RepositoryID: "repo_api", Commit: commitX}, {RepositoryID: "repo_web", Commit: commitX}})
			return s
		}()}
		if _, err := artifacts.NewRegistered("art_set_o", o, moved, t0); !errors.Is(err, artifacts.ErrRegistryContent) {
			t.Fatalf("a registration whose evidence names other commits: %v", err)
		}
		single := v
		single.Provenance = prov
		if _, err := artifacts.NewRegistered("art_set_o", o, single, t0); !errors.Is(err, artifacts.ErrRegistryContent) {
			t.Fatalf("a single source cannot stand for a source set: %v", err)
		}
		// Restoring a stored row re-validates the provenance.
		bad := o
		bad.Provenance = artifacts.Provenance{SourceSet: artifacts.SourceSet{Digest: setDigest}}
		if _, err := artifacts.NewRegistered("art_set_o", bad, artifacts.OCIVerification{Repository: "registry.example.test/team/app", Digest: digest, ManifestSize: 1, Provenance: bad.Provenance}, t0); err == nil {
			t.Fatal("a non-canonical stored set was accepted")
		}
	})

	t.Run("an OCI registration still needs provenance and a replay ignores it", func(t *testing.T) {
		o := ociRes()
		o.Provenance = artifacts.Provenance{}
		if err := o.Validate(); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("an OCI registration without provenance: %v", err)
		}
		a := newFile(t)
		same := fileRes()
		same.Provenance = setProv // server-derived, not compared
		if !a.Matches(same) {
			t.Fatal("provenance is not part of the replay key")
		}
	})
}

// Test_unit_single_source_subjects_unchanged (T-149): an artifact of a
// single-repository run keeps its provenance, its reservation shape and its
// public representation exactly; the source set adds members only to artifacts
// of source-set runs.
func Test_unit_single_source_subjects_unchanged(t *testing.T) {
	if prov.RepositoryURL == "" || prov.Commit == "" || !prov.SourceSet.IsZero() || prov.Validate() != nil {
		t.Fatalf("single provenance: %+v", prov)
	}
	if artifactstest.Source.SourceSet != (artifacts.SourceSet{}) || artifactstest.Source.RepositoryURL == "" {
		t.Fatalf("the default fixture provenance is a single source: %+v", artifactstest.Source)
	}
	a := newFile(t)
	if a.Provenance() != prov || a.Reference().Provenance != prov {
		t.Fatalf("a file's provenance and consumer view: %+v", a.Provenance())
	}
	// A single source is a comparable struct literal of exactly its two members:
	// code outside the module that builds it and compares it still compiles and
	// agrees.
	lit := artifacts.Provenance{RepositoryURL: prov.RepositoryURL, Commit: prov.Commit}
	if lit != prov || !(artifacts.Provenance{}).IsZero() {
		t.Fatal("a single provenance literal is the provenance")
	}
	if got := domain.ArtifactID("art_1"); a.ID() != got {
		t.Fatalf("id %s", a.ID())
	}
}
