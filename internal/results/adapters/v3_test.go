package adapters

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

var v3scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

type fakeCommitter struct {
	err  error
	seen execution.ResultCommitV3
}

func (f *fakeCommitter) CommitResultV3(_ context.Context, in execution.ResultCommitV3) (execution.ResultAck, error) {
	f.seen = in
	return execution.ResultAck{AttemptID: in.AttemptID, ResultHash: string(in.Request.ResultHash)}, f.err
}

type fakeRuns struct{ agg *execution.Aggregate }

func (f fakeRuns) RunOf(_ context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error) {
	if at := f.agg.Attempt(attempt); at != nil && at.Scope == scope {
		return f.agg.Run.ID, nil
	}
	return "", execution.ErrNotFound
}

func (f fakeRuns) Get(_ context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	if f.agg.Run.Scope != scope || f.agg.Run.ID != id {
		return nil, execution.ErrNotFound
	}
	return f.agg, nil
}

func v3Aggregate(t *testing.T, contextDoc []byte) (*execution.Aggregate, runtimev3.ContextV3) {
	t.Helper()
	c := rtest.V3Context(t, rtest.WriterContext)
	if contextDoc == nil {
		var err error
		if _, contextDoc, err = runtimeauth.PinContextV3(c); err != nil {
			t.Fatal(err)
		}
	}
	return &execution.Aggregate{
		Run:      &execution.Run{ID: c.RunID, Scope: c.Scope(), Status: execution.RunRunning},
		Attempts: []*execution.Attempt{{Scope: c.Scope(), ID: c.AttemptID, RunID: c.RunID, Status: execution.AttemptRunning, Context: contextDoc}},
	}, c
}

func TestExecutionV3CommitsWithTheLoadedGrantsAndAcknowledgesFromTheRecord(t *testing.T) {
	agg, c := v3Aggregate(t, nil)
	committer := &fakeCommitter{}
	e, err := NewExecutionV3(committer, fakeRuns{agg})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := e.PinnedV3(context.Background(), v3scope, c.AttemptID)
	if err != nil || pinned.AttemptID != c.AttemptID || pinned.Git == nil {
		t.Fatalf("pinned: %+v %v", pinned, err)
	}
	if _, found, err := e.Accepted(context.Background(), v3scope, c.AttemptID); err != nil || found {
		t.Fatalf("accepted: %v %v", found, err)
	}
	issued := []gitv1.GrantClaims{rtest.IssuedClaims(gitv1.GrantIntegration, v3scope, c.RunID, c.AttemptID, "gg_int01", "repo_01")}
	req := runtimev3.ResultRequestV3{ResultHash: "sha256:" + runtimev3.Digest(strings.Repeat("a", 64))}
	// The commit carries the Issued claims the module loaded; the acknowledgment is
	// the persisted attempt's (status, acceptance time), never the request's.
	at := agg.Attempt(c.AttemptID)
	finished := time.Date(2026, 10, 30, 10, 30, 0, 0, time.UTC)
	at.ResultHash, at.Status, at.FinishedAt = string(req.ResultHash), execution.AttemptSucceeded, &finished
	ack, err := e.CommitV3(context.Background(), results.CommitV3{Scope: v3scope, AttemptID: c.AttemptID, Request: req, Issued: issued})
	if err != nil || ack.Status != runtimev1.AttemptSucceeded || !ack.AcceptedAt.Equal(finished) || ack.ResultHash != req.ResultHash {
		t.Fatalf("ack: %+v %v", ack, err)
	}
	if len(committer.seen.Issued) != 1 || committer.seen.Issued[0].GrantID != "gg_int01" || committer.seen.Scope != v3scope {
		t.Fatalf("execution did not receive the loaded claims: %+v", committer.seen)
	}
	if h, found, err := e.Accepted(context.Background(), v3scope, c.AttemptID); err != nil || !found || h != req.ResultHash {
		t.Fatalf("accepted after: %v %v %v", h, found, err)
	}
	// A hash execution accepted that is not the one asked is not acknowledged.
	at.ResultHash = "sha256:" + strings.Repeat("b", 64)
	if _, err := e.CommitV3(context.Background(), results.CommitV3{Scope: v3scope, AttemptID: c.AttemptID, Request: req}); !errors.Is(err, results.ErrUnavailable) {
		t.Fatalf("a mismatched receipt: %v", err)
	}
}

func TestExecutionV3Classification(t *testing.T) {
	for _, c := range []struct{ in, want error }{
		{execution.ErrContextVersionMismatch, results.ErrContextVersionMismatch},
		{fmt.Errorf("%w: pending", execution.ErrBusy), results.ErrUnavailable},
		{fmt.Errorf("%w: unsettled", execution.ErrStale), results.ErrUnavailable},
		{execution.ErrAttemptRevoked, results.ErrAttemptRevoked},
		{execution.ErrResultConflict, results.ErrResultConflict},
		{execution.ErrNotFound, results.ErrNotFound},
		{errors.New("driver: commit status unknown"), results.ErrUnavailable},
	} {
		if got := mapExecutionV3(c.in); !errors.Is(got, c.want) {
			t.Errorf("%v -> %v, want %v", c.in, got, c.want)
		}
	}
	// An attempt of another wire is a mismatch, not an unreadable context.
	agg, c := v3Aggregate(t, []byte(`{"contextVersion":2}`))
	e, _ := NewExecutionV3(&fakeCommitter{}, fakeRuns{agg})
	if _, err := e.PinnedV3(context.Background(), v3scope, c.AttemptID); !errors.Is(err, results.ErrContextVersionMismatch) {
		t.Fatalf("a v2 attempt: %v", err)
	}
	agg, c = v3Aggregate(t, []byte(`{"contextVersion":3}`))
	e, _ = NewExecutionV3(&fakeCommitter{}, fakeRuns{agg})
	if _, err := e.PinnedV3(context.Background(), v3scope, c.AttemptID); !errors.Is(err, results.ErrUnavailable) {
		t.Fatalf("an unreadable v3 context: %v", err)
	}
	if _, err := NewExecutionV3(nil, nil); err == nil {
		t.Error("execution without dependencies")
	}
}

type fakePushes struct {
	rec gitv1.PushRecord
	err error
}

func (f fakePushes) Reload(context.Context, domain.Scope, string, string) (gitv1.PushRecord, error) {
	return f.rec, f.err
}

func TestPushesMapTheGitServicesAnswers(t *testing.T) {
	rec := gitv1.PushRecord{PushID: "pu_07", RepositoryID: "repo_01"}
	p, err := NewPushes(fakePushes{rec: rec})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.Push(context.Background(), v3scope, "repo_01", "pu_07"); err != nil || got.PushID != "pu_07" {
		t.Fatalf("%+v %v", got, err)
	}
	p, _ = NewPushes(fakePushes{err: fmt.Errorf("%w: gone", gitpush.ErrNotFound)})
	if _, err := p.Push(context.Background(), v3scope, "repo_01", "pu_07"); !errors.Is(err, results.ErrNotFound) {
		t.Fatalf("unknown record: %v", err)
	}
	for _, e := range []error{gitpush.ErrUnavailable, errors.New("driver: reset")} {
		p, _ = NewPushes(fakePushes{err: e})
		if _, err := p.Push(context.Background(), v3scope, "repo_01", "pu_07"); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("%v: %v", e, err)
		}
	}
	if _, err := NewPushes(nil); err == nil {
		t.Error("pushes without a service")
	}
}

type fakeRecords struct {
	claims []gitv1.GrantClaims
	err    error
}

func (f fakeRecords) IssuedGitGrants(context.Context, domain.Scope, domain.AttemptID) ([]gitv1.GrantClaims, error) {
	return f.claims, f.err
}

func TestIssuedReadsTheControlPlanesRecords(t *testing.T) {
	claims := []gitv1.GrantClaims{rtest.IssuedClaims(gitv1.GrantSeed, v3scope, "run_01", "att_01", "gg_s", "repo_new")}
	i, err := NewIssued(fakeRecords{claims: claims})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := i.Issued(context.Background(), v3scope, "att_01"); err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	i, _ = NewIssued(fakeRecords{err: runtimeauth.ErrNotFound})
	if _, err := i.Issued(context.Background(), v3scope, "att_01"); !errors.Is(err, results.ErrUnavailable) {
		t.Fatalf("an unreadable record set is never read as empty: %v", err)
	}
	if _, err := NewIssued(nil); err == nil {
		t.Error("issued without a service")
	}
}

type fakeLookup struct {
	refs map[domain.ArtifactID]artifacts.Reference
}

func (f fakeLookup) References(context.Context, domain.Scope, domain.AttemptID, []domain.ArtifactID) (map[domain.ArtifactID]artifacts.Reference, error) {
	return f.refs, nil
}

func (f fakeLookup) Pin(context.Context, domain.Scope, domain.ArtifactID, artifacts.PinOwner) error {
	return nil
}

// An artifact produced from a source set carries the set's digest into the
// evidence the result module judges (it must not read as "no source").
func TestSourceSetProvenanceIsCarriedFromTheArtifactOwner(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	set, err := artifacts.NewSourceSet(digest, []artifacts.SourceCommit{{RepositoryID: "repo_01", Commit: strings.Repeat("1", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewArtifacts(fakeLookup{refs: map[domain.ArtifactID]artifacts.Reference{
		"art_set":  {ID: "art_set", Scope: v3scope, Attempt: "att_01", Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Usable: true, Provenance: artifacts.Provenance{SourceSet: set}},
		"art_one":  {ID: "art_one", Scope: v3scope, Attempt: "att_01", Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Usable: true, Provenance: artifacts.Provenance{RepositoryURL: "https://git.example.invalid/x.git", Commit: strings.Repeat("2", 40)}},
		"art_none": {ID: "art_none", Scope: v3scope, Attempt: "att_01", Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Usable: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.References(context.Background(), v3scope, "att_01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := got["art_set"].Provenance; p.SourceSet != digest || p.RepositoryURL != "" || p.Commit != "" || p.IsZero() {
		t.Errorf("a source-set artifact: %+v", p)
	}
	if p := got["art_one"].Provenance; p.SourceSet != "" || p.Commit != strings.Repeat("2", 40) {
		t.Errorf("a single-source artifact: %+v", p)
	}
	if !got["art_none"].Provenance.IsZero() {
		t.Errorf("an artifact without source: %+v", got["art_none"].Provenance)
	}
}

func TestAuthorityMapsTheVersionMismatch(t *testing.T) {
	got := MapAuthority(errors.Join(runtimeauth.ErrContextVersionMismatch, errors.New("another version")))
	if !errors.Is(got, results.ErrContextVersionMismatch) {
		t.Fatalf("a version mismatch mapped to %v", got)
	}
}

// The authority adapter asserts the wire of the route: the source-set result
// route authenticates only attempts of the source-set wire, the earlier routes keep
// asserting nothing (their own guard and execution refuse another wire's attempt).
func TestAuthorityAssertsTheWireOfTheRoute(t *testing.T) {
	m := rtest.NewV3MemStack(t)
	bg := context.Background()
	c := rtest.V3Context(t, rtest.WriterContext)
	a3 := m.SeedAttemptV3(c, rtest.AllActions())
	cred3, _ := runtimeauth.GenerateCredential(nil)
	if _, err := m.RegisterV3With(a3, c, cred3, rtest.AllActions()); err != nil {
		t.Fatal(err)
	}
	r3, err := m.Service.AdmitV3(bg, c.AttemptID, m.AdmitRequest(a3, cred3, "nnnnnnnnnnnnnnnnnnnnnn"))
	if err != nil {
		t.Fatal(err)
	}
	other := rtest.NewV3MemStack(t) // a v1 attempt in its own stack (same tenant, own grants)
	v1, req := other.HappyContext()
	a1 := other.SeedAttempt(v1, rtest.AllActions())
	cred1, _, err := other.Register(a1, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	req.AdmissionCredential = cred1.Reveal()
	r1, err := other.Service.Admit(bg, v1.AttemptID, req)
	if err != nil {
		t.Fatal(err)
	}

	auth3, err := NewAuthority(m.Verifier, m.Ring, m.Env.Grants, m.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := auth3.Authenticate(bg, results.AuthRequest{Attempt: c.AttemptID, Bearer: r3.Token.Token, Action: results.ActionResultSubmit, Wire: results.WireV3}); err != nil || p.AttemptID != c.AttemptID || p.RunID != c.RunID {
		t.Fatalf("a v3 attempt on the v3 route: %+v %v", p, err)
	}
	// Without a stated wire the earlier behavior holds (no assertion).
	if _, err := auth3.Authenticate(bg, results.AuthRequest{Attempt: c.AttemptID, Bearer: r3.Token.Token, Action: results.ActionResultSubmit}); err != nil {
		t.Fatalf("an unasserted wire: %v", err)
	}
	auth1, err := NewAuthority(other.Verifier, other.Ring, other.Env.Grants, other.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth1.Authenticate(bg, results.AuthRequest{Attempt: v1.AttemptID, Bearer: r1.Token.Token, Action: results.ActionResultSubmit, Wire: results.WireV3}); !errors.Is(err, results.ErrContextVersionMismatch) {
		t.Fatalf("a v1 attempt on the v3 route: %v", err)
	}
	if _, err := auth1.Authenticate(bg, results.AuthRequest{Attempt: v1.AttemptID, Bearer: r1.Token.Token, Action: results.ActionResultSubmit}); err != nil {
		t.Fatalf("the earlier route is unchanged: %v", err)
	}
	// A bad token is the same answer as ever, before any wire talk.
	if _, err := auth3.Authenticate(bg, results.AuthRequest{Attempt: c.AttemptID, Bearer: "nope", Action: results.ActionResultSubmit, Wire: results.WireV3}); !errors.Is(err, results.ErrUnauthenticated) {
		t.Fatalf("a bad token: %v", err)
	}
}
