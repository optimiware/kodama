package delivery_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	setDigest = "sha256:" + strings.Repeat("d", 64)
	commitA   = strings.Repeat("1", 40)
	commitB   = strings.Repeat("2", 40)
)

func mustSet(t testing.TB, digest string, commits ...delivery.SourceCommit) delivery.SourceSet {
	t.Helper()
	s, err := delivery.NewSourceSet(digest, commits)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func twoRepos(t testing.TB) delivery.SourceSet {
	return mustSet(t, setDigest, delivery.SourceCommit{RepositoryID: "repo_web", Commit: commitB}, delivery.SourceCommit{RepositoryID: "repo_api", Commit: commitA})
}

// ---- doubles for the registration path -----------------------------------------

// acceptingStore records the registration it is asked to commit.
type acceptingStore struct {
	*fakeStore
	registered []delivery.RegisterCommand
}

func (s *acceptingStore) Register(_ context.Context, cmd delivery.RegisterCommand) (delivery.RegisterResult, error) {
	s.registered = append(s.registered, cmd)
	return delivery.RegisterResult{Deployment: cmd.Deployment, Created: true}, nil
}

// evidencePeers answers the artifact and producer questions with fixed evidence.
type evidencePeers struct {
	source   delivery.Source
	protects int
	scope    domain.Scope
}

func (p *evidencePeers) Evidence(_ context.Context, sc domain.Scope, id domain.ArtifactID) (delivery.ImageEvidence, error) {
	return delivery.ImageEvidence{ID: id, Scope: sc, Attempt: "att_build", Kind: runtimev1.KindOCI, Role: runtimev1.RoleOutput, Usable: true,
		Digest: registration().Image.Digest, Provenance: p.source}, nil
}
func (p *evidencePeers) Protect(context.Context, domain.Scope, domain.ArtifactID, delivery.DeploymentID) error {
	p.protects++
	return nil
}
func (p *evidencePeers) Release(context.Context, domain.Scope, domain.ArtifactID, delivery.DeploymentID) error {
	return nil
}
func (p *evidencePeers) Produced(_ context.Context, sc domain.Scope, attempt domain.AttemptID, _ domain.ArtifactID) (delivery.ProducedOutput, error) {
	r := registration()
	return delivery.ProducedOutput{Scope: sc, Attempt: attempt, Run: r.Producer.Run, Step: r.Producer.Step, ToolImage: r.Producer.ToolImage, Accepted: true}, nil
}

// primaryURL is the repository URL of the fixture set's primary source (repo_api,
// the smaller repository ID, when neither is writable).
const primaryURL = "https://git.example.invalid/git/t/p/api.git"

// pinnedSets answers PinnedSourceSet with the set and its primary source (the
// first commit's repository).
type pinnedSets struct {
	set   delivery.SourceSet
	found bool
	err   error
}

func (p pinnedSets) PinnedSourceSet(context.Context, domain.Scope, domain.AttemptID) (delivery.PinnedSource, bool, error) {
	var pin delivery.PinnedSource
	if !p.set.IsZero() {
		pin = delivery.PinnedSource{Set: p.set, PrimaryURL: primaryURL, PrimaryCommit: p.set.Commits()[0].Commit}
	}
	return pin, p.found, p.err
}

// recordForm is a set source as the record keeps it: the set and its primary
// source's repository and commit.
func recordForm(set delivery.SourceSet) delivery.Source {
	return delivery.Source{RepositoryURL: primaryURL, Commit: set.Commits()[0].Commit, SourceSet: set}
}

type setRig struct {
	f     *fixture
	store *acceptingStore
	peers *evidencePeers
}

// newSetRig builds the registration path of a deploy attempt whose pinned context
// is the fixture's, with sets as the attempt's source set source (nil: none).
func newSetRig(t *testing.T, evidence delivery.Source, sets delivery.PinnedSourceSets) (*setRig, *delivery.Service) {
	return newRig(t, evidence, sets, true)
}

// newRig is newSetRig for an attempt whose pinned context is a source-backed
// context without a single source (the runtime v3 projection: access read) or the
// fixture's own context (no workspace access: the single-source rule).
func newRig(t *testing.T, evidence delivery.Source, sets delivery.PinnedSourceSets, setContext bool) (*setRig, *delivery.Service) {
	t.Helper()
	f := newFixture(t)
	if setContext {
		f.pinned.Workspace = runtimev1.Workspace{Access: runtimev1.AccessRead}
	}
	store := &acceptingStore{fakeStore: f.store}
	peers := &evidencePeers{source: evidence}
	svc, err := delivery.NewService(delivery.Config{
		Store: store, Access: allowAll{}, Credentials: noCredentials{t}, Contexts: fakeContexts{c: f.pinned}, Producers: peers, Artifacts: peers, SourceSets: sets,
		Authority: fakeAuthority{f.principal}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &setRig{f: f, store: store, peers: peers}, svc
}

func (r *setRig) register(svc *delivery.Service, src delivery.Source) (delivery.Receipt, error) {
	reg := registration()
	return svc.RegisterRuntime(context.Background(), r.f.principal.Attempt, "tok", delivery.RegistrationRequest{Source: src, Image: reg.Image})
}

// Test_unit_deployment_provenance_source_set (DQ-17, DQ-A13): a deployment of a
// run with a source set records the digest and one commit per repository in place
// of a single repository and commit; the set is part of the logical request, and
// registration needs the attempt, the image's provenance and the request to
// name the same set.
func Test_unit_deployment_provenance_source_set(t *testing.T) {
	set := twoRepos(t)

	t.Run("a source set is canonical and replaces the single source", func(t *testing.T) {
		got := set.Commits()
		if len(got) != 2 || got[0].RepositoryID != "repo_api" || got[1].RepositoryID != "repo_web" {
			t.Fatalf("commits ordered by repository: %+v", got)
		}
		again := mustSet(t, setDigest, delivery.SourceCommit{RepositoryID: "repo_api", Commit: commitA}, delivery.SourceCommit{RepositoryID: "repo_web", Commit: commitB})
		if again != set {
			t.Fatal("the input order is not part of the identity")
		}
		if err := (delivery.Source{SourceSet: set}).Validate(); err != nil {
			t.Fatal(err)
		}
		if err := recordForm(set).Validate(); err != nil {
			t.Fatalf("the record form: %v", err)
		}
		url := registration().Source.RepositoryURL
		for name, s := range map[string]delivery.Source{"neither": {}, "url only": {RepositoryURL: url}, "set and a stray commit": {Commit: commitA, SourceSet: set},
			"a commit outside the set": {RepositoryURL: url, Commit: strings.Repeat("3", 40), SourceSet: set}} {
			if err := s.Validate(); !errors.Is(err, delivery.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		for name, build := range map[string]func() error{
			"no commits": func() error { _, err := delivery.NewSourceSet(setDigest, nil); return err },
			"a bad digest": func() error {
				_, err := delivery.NewSourceSet("sha256:x", []delivery.SourceCommit{{RepositoryID: "r", Commit: commitA}})
				return err
			},
			"a repeated repo": func() error {
				_, err := delivery.NewSourceSet(setDigest, []delivery.SourceCommit{{RepositoryID: "r", Commit: commitA}, {RepositoryID: "r", Commit: commitB}})
				return err
			},
			"a branch": func() error {
				_, err := delivery.NewSourceSet(setDigest, []delivery.SourceCommit{{RepositoryID: "r", Commit: "main"}})
				return err
			},
			"a non canonical":  func() error { return delivery.SourceSet{Digest: setDigest}.Validate() },
			"too many commits": func() error { return tooMany() },
		} {
			if err := build(); !errors.Is(err, delivery.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("the set round-trips through JSON and is part of the fingerprint", func(t *testing.T) {
		raw, err := json.Marshal(recordForm(set))
		if err != nil {
			t.Fatal(err)
		}
		want := `{"repositoryUrl":"` + primaryURL + `","commit":"` + commitA + `","sourceSet":{"digest":"` + setDigest + `","commits":[{"repositoryId":"repo_api","commit":"` + commitA + `"},{"repositoryId":"repo_web","commit":"` + commitB + `"}]}}`
		if string(raw) != want {
			t.Fatalf("encoding:\n got %s\nwant %s", raw, want)
		}
		var back delivery.Source
		if err := json.Unmarshal(raw, &back); err != nil || back != recordForm(set) {
			t.Fatalf("decoding: %v %+v", err, back)
		}
		if err := json.Unmarshal([]byte(`{"sourceSet":{"digest":"`+setDigest+`","commits":[]}}`), &back); err == nil {
			t.Fatal("an empty set was decoded")
		}
		r := registration()
		single, err := r.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		r.Source = recordForm(set)
		withSet, err := r.Fingerprint()
		if err != nil || withSet == single {
			t.Fatalf("a set is another request: %v", err)
		}
		r2 := r
		r2.Source = recordForm(mustSet(t, setDigest, delivery.SourceCommit{RepositoryID: "repo_api", Commit: commitB}, delivery.SourceCommit{RepositoryID: "repo_web", Commit: commitB}))
		if fp, _ := r2.Fingerprint(); fp == withSet {
			t.Fatal("changing one repository's commit is another request")
		}
		if again, _ := r.Fingerprint(); again != withSet {
			t.Fatal("the fingerprint is deterministic")
		}
	})

	t.Run("a deployment records the set it was registered with", func(t *testing.T) {
		reg := registration()
		reg.Source = recordForm(set)
		d, err := delivery.NewDeployment(reg, now)
		if err != nil {
			t.Fatal(err)
		}
		rec := d.Record()
		if rec.Source != recordForm(set) {
			t.Fatalf("source: %+v", rec.Source)
		}
		restored, err := delivery.RestoreDeployment(rec)
		if err != nil || restored.Record().Source != rec.Source {
			t.Fatalf("restore: %v", err)
		}
		for name, src := range map[string]delivery.Source{
			"a commit that is not the set's": {RepositoryURL: primaryURL, Commit: strings.Repeat("3", 40), SourceSet: set},
			"a set without its primary":      {SourceSet: set},
			"a primary without a URL":        {Commit: commitA, SourceSet: set},
			"a primary with a bad URL":       {RepositoryURL: "git@host:r.git", Commit: commitA, SourceSet: set},
		} {
			bad := rec
			bad.Source = src
			if _, err := delivery.RestoreDeployment(bad); !errors.Is(err, delivery.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("registration commits the set the attempt, the image and the request agree on", func(t *testing.T) {
		rig, svc := newSetRig(t, delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true})
		rc, err := rig.register(svc, delivery.Source{SourceSet: set})
		if err != nil || !rc.Created {
			t.Fatalf("register: %+v %v", rc, err)
		}
		if len(rig.store.registered) != 1 || rig.store.registered[0].Deployment.Record().Source != recordForm(set) {
			t.Fatalf("the committed record: %+v", rig.store.registered)
		}
		if rig.peers.protects != 1 {
			t.Fatalf("the image was protected %d times", rig.peers.protects)
		}
	})
}

func tooMany() error {
	var commits []delivery.SourceCommit
	for i := 0; i <= delivery.MaxSourceCommits; i++ {
		commits = append(commits, delivery.SourceCommit{RepositoryID: "repo_" + strings.Repeat("a", i+1), Commit: commitA})
	}
	_, err := delivery.NewSourceSet(setDigest, commits)
	return err
}

// Test_unit_deployment_mismatch_rejected (DQ-A13): a deployment whose provenance
// does not match every repository of the attempt's source set is rejected as
// image provenance before anything is written or pinned: another commit of either
// repository, another set, a single commit for a set and the converse, an image
// built from other commits, and a set that cannot be checked.
func Test_unit_deployment_mismatch_rejected(t *testing.T) {
	set := twoRepos(t)
	moveOne := func(repo string) delivery.SourceSet {
		commits := set.Commits()
		for i := range commits {
			if commits[i].RepositoryID == repo {
				commits[i].Commit = strings.Repeat("3", 40)
			}
		}
		return mustSet(t, setDigest, commits...)
	}
	otherDigest := mustSet(t, "sha256:"+strings.Repeat("e", 64), set.Commits()...)
	oneRepo := mustSet(t, setDigest, set.Commits()[0])
	single := registration().Source

	cases := []struct {
		name     string
		evidence delivery.Source
		sets     delivery.PinnedSourceSets
		request  delivery.Source
		want     error
	}{
		{"another commit of the first repository", delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true}, delivery.Source{SourceSet: moveOne("repo_api")}, delivery.ErrProvenance},
		{"another commit of the second repository", delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true}, delivery.Source{SourceSet: moveOne("repo_web")}, delivery.ErrProvenance},
		{"another digest", delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true}, delivery.Source{SourceSet: otherDigest}, delivery.ErrProvenance},
		{"a set missing a repository", delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true}, delivery.Source{SourceSet: oneRepo}, delivery.ErrProvenance},
		{"a single commit for an attempt with a set", delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true}, single, delivery.ErrProvenance},
		{"a set for an attempt without one", delivery.Source{SourceSet: set}, pinnedSets{}, delivery.Source{SourceSet: set}, delivery.ErrProvenance},
		{"a set when no source set reader is configured", delivery.Source{SourceSet: set}, nil, delivery.Source{SourceSet: set}, delivery.ErrNotConfigured},
		{"a single commit when no source set reader is configured", delivery.Source{SourceSet: set}, nil, single, delivery.ErrNotConfigured},
		{"a single commit that is the primary source's", delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true}, delivery.Source{RepositoryURL: primaryURL, Commit: commitA}, delivery.ErrProvenance},
		{"a set naming a primary of its own", delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true}, recordForm(set), delivery.ErrProvenance},
		{"an attempt the reader does not know", delivery.Source{SourceSet: set}, pinnedSets{err: delivery.Fail(delivery.ErrNotFound, nil, "nf")}, delivery.Source{SourceSet: set}, delivery.ErrProvenance},
		{"an unreadable attempt context", delivery.Source{SourceSet: set}, pinnedSets{err: errors.New("db down")}, delivery.Source{SourceSet: set}, delivery.ErrUnavailable},
		{"an image built from another commit of one repository", delivery.Source{SourceSet: moveOne("repo_web")}, pinnedSets{set: set, found: true}, delivery.Source{SourceSet: set}, delivery.ErrProvenance},
		{"an image built from another set", delivery.Source{SourceSet: otherDigest}, pinnedSets{set: set, found: true}, delivery.Source{SourceSet: set}, delivery.ErrProvenance},
		{"an image built from a single commit", delivery.Source{RepositoryURL: single.RepositoryURL, Commit: commitA}, pinnedSets{set: set, found: true}, delivery.Source{SourceSet: set}, delivery.ErrProvenance},
		{"an image built from a set for a single-source deployment", delivery.Source{SourceSet: set}, pinnedSets{}, single, delivery.ErrProvenance},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig, svc := newSetRig(t, c.evidence, c.sets)
			_, err := rig.register(svc, c.request)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if len(rig.store.registered) != 0 || rig.store.writes != 0 || rig.peers.protects != 0 {
				t.Fatalf("a rejected registration wrote %d records, %d writes and pinned %d times", len(rig.store.registered), rig.store.writes, rig.peers.protects)
			}
		})
	}

	t.Run("a source set for an attempt that is not a source-set attempt is refused", func(t *testing.T) {
		rig, svc := newRig(t, delivery.Source{SourceSet: set}, pinnedSets{set: set, found: true}, false)
		if _, err := rig.register(svc, delivery.Source{SourceSet: set}); !errors.Is(err, delivery.ErrProvenance) {
			t.Fatalf("a set for the single-source context: %v", err)
		}
		if len(rig.store.registered) != 0 || rig.peers.protects != 0 {
			t.Fatal("a refused registration wrote")
		}
	})

	t.Run("VerifyImage rejects evidence of another source set", func(t *testing.T) {
		reg := registration()
		ev := delivery.ImageEvidence{ID: reg.Image.ArtifactID, Scope: scope, Attempt: "att_build", Kind: runtimev1.KindOCI, Role: runtimev1.RoleOutput, Usable: true,
			Digest: reg.Image.Digest, Provenance: delivery.Source{SourceSet: set}}
		out := delivery.ProducedOutput{Scope: scope, Attempt: "att_build", Run: reg.Producer.Run, Step: reg.Producer.Step, ToolImage: reg.Producer.ToolImage, Accepted: true}
		if _, err := delivery.VerifyImage(scope, delivery.Source{SourceSet: set}, reg.Image, ev, out); err != nil {
			t.Fatalf("the same set verifies: %v", err)
		}
		for name, src := range map[string]delivery.Source{"moved": {SourceSet: moveOne("repo_api")}, "single": single, "none": {}} {
			if _, err := delivery.VerifyImage(scope, src, reg.Image, ev, out); !errors.Is(err, delivery.ErrProvenance) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

// Test_unit_single_source_subjects_unchanged (T-149): a deployment of a
// single-repository run keeps its source, its logical-request fingerprint and its
// identity exactly. The values were captured from the commit before the source
// set existed.
func Test_unit_single_source_subjects_unchanged(t *testing.T) {
	const (
		wantFingerprint = "sha256:510c0f5e8cb0fd47d622aaa0abd812a2af258637afcb00ff166dfea2bbb8b4ff"
		wantSource      = `{"repositoryUrl":"https://git.example.invalid/r.git","commit":"1111111111111111111111111111111111111111"}`
		wantID          = "dep_ac85dd16ecde2322a2b02f2d2caa9c53"
	)
	r := registration()
	fp, err := r.Fingerprint()
	if err != nil || string(fp) != wantFingerprint {
		t.Fatalf("fingerprint %s (%v), want %s", fp, err, wantFingerprint)
	}
	raw, err := json.Marshal(r.Source)
	if err != nil || string(raw) != wantSource {
		t.Fatalf("source JSON %s (%v), want %s", raw, err, wantSource)
	}
	if id := delivery.DeploymentIDFor(scope, r.ActionKey); string(id) != wantID {
		t.Fatalf("deployment ID %s, want %s", id, wantID)
	}
	d := newDeployment(t)
	if d.ID() != delivery.DeploymentID(wantID) || d.Record().Fingerprint != runtimev1.Digest(wantFingerprint) || !d.Record().Source.SourceSet.IsZero() {
		t.Fatalf("the record of a single-source deployment: %+v", d.Record())
	}
	// A single source registers exactly as before, with or without a source set reader.
	for _, sets := range []delivery.PinnedSourceSets{nil, pinnedSets{}} {
		rig, svc := newRig(t, r.Source, sets, false)
		rc, err := rig.register(svc, r.Source)
		if err != nil || !rc.Created || len(rig.store.registered) != 1 || rig.store.registered[0].Deployment.Record().Source != r.Source {
			t.Fatalf("a single-source registration: %+v %v", rc, err)
		}
	}
}
