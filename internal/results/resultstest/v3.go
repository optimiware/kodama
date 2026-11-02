package resultstest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// V3World is the development fixture of the source-set result route (T-150): the
// attempts of source-set runs with their pinned contexts, the push records the Git
// service holds, the runtime-issued grants the control plane recorded, the artifact
// evidence and an execution finalizer that models the contract points results
// depends on (receipt first, a different result conflicts, a cancelled attempt is
// revoked, outputs checked against the grants). It is a stand-in: it proves what
// this module asks of its ports and what it refuses before asking, not execution's
// or the Git service's behavior. Every read a test forbids fails the test.
type V3World struct {
	T     testing.TB
	Clock *Clock

	mu       sync.Mutex
	attempts map[domain.AttemptID]*v3Attempt
	records  map[string]gitv1.PushRecord
	issued   map[domain.AttemptID][]gitv1.GrantClaims
	evidence map[domain.ArtifactID]results.ArtifactEvidence

	// PushErr and IssuedErr make the reads fail (an outage, not an answer).
	PushErr, IssuedErr error
	// CommitErr fails the next commit before it takes effect (an unknown outcome
	// or the retryable "not settled yet" of a checkpoint); LoseAck loses that many
	// acknowledgments after the commit took effect.
	CommitErr error
	LoseAck   int
	// ForbidReads makes any read of a push record, the issued grants, the pinned
	// context or artifact evidence a test failure: the receipt-first path reads none.
	ForbidReads bool

	// Observations.
	PushReads, IssuedReads, ContextReads, EvidenceReads int
	Pinned                                              []domain.ArtifactID
	Commits                                             []results.CommitV3
}

type v3Attempt struct {
	scope    domain.Scope
	run      domain.RunID
	id       domain.AttemptID
	token    string
	wire     int64
	context  runtimev3.ContextV3
	status   string // active, cancelled
	hash     runtimev1.Digest
	ack      results.Acknowledgment
	revoked  bool // the grant was revoked: even a replay is denied
	finished bool
}

// NewV3World returns an empty fixture.
func NewV3World(t testing.TB) *V3World {
	return &V3World{
		T: t, Clock: NewClock(), attempts: map[domain.AttemptID]*v3Attempt{}, records: map[string]gitv1.PushRecord{},
		issued: map[domain.AttemptID][]gitv1.GrantClaims{}, evidence: map[domain.ArtifactID]results.ArtifactEvidence{},
	}
}

// V3Attempt is a handle on one attempt of the world.
type V3Attempt struct {
	ID      domain.AttemptID
	Run     domain.RunID
	Scope   domain.Scope
	Context runtimev3.ContextV3
	Token   string
}

// NewAttempt creates an active attempt of a source-set run with the context
// fixture (rtest.WriterContext or rtest.ProvisionContext).
func (w *V3World) NewAttempt(fixture string) V3Attempt {
	w.T.Helper()
	c := rtest.V3Context(w.T, fixture)
	a := &v3Attempt{scope: c.Scope(), run: c.RunID, id: c.AttemptID, token: "tok_" + string(c.AttemptID), wire: results.WireV3, context: c, status: "active"}
	w.mu.Lock()
	w.attempts[a.id] = a
	w.mu.Unlock()
	return V3Attempt{ID: a.id, Run: a.run, Scope: a.scope, Context: c, Token: a.token}
}

// NewV1Attempt creates an attempt of another wire: the source-set route must
// refuse it as a version mismatch.
func (w *V3World) NewV1Attempt(id domain.AttemptID) V3Attempt {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	a := &v3Attempt{scope: scope, run: "run_v1", id: id, token: "tok_" + string(id), wire: 1, status: "active"}
	w.mu.Lock()
	w.attempts[id] = a
	w.mu.Unlock()
	return V3Attempt{ID: id, Run: a.run, Scope: scope, Token: a.token}
}

// Cancel ends the attempt's authority (state changed; revoke also revokes the
// grant, which denies even a result replay).
func (w *V3World) Cancel(a V3Attempt, revoke bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	at := w.attempts[a.ID]
	at.status = "cancelled"
	at.revoked = revoke
}

// SetPush stores (or replaces) a push record of the repository.
func (w *V3World) SetPush(rec gitv1.PushRecord) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records[rec.RepositoryID+"/"+rec.PushID] = rec
}

// SetIssued records the runtime-issued grants of an attempt.
func (w *V3World) SetIssued(a V3Attempt, claims ...gitv1.GrantClaims) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.issued[a.ID] = claims
}

// AddArtifact registers a usable file artifact of the attempt with the output role.
func (w *V3World) AddArtifact(a V3Attempt, id domain.ArtifactID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.evidence[id] = results.ArtifactEvidence{ID: id, Scope: a.Scope, Attempt: a.ID, Kind: runtimev1.KindFile, Role: runtimev1.RoleOutput, Usable: true}
}

// PushOf builds a valid push record made by the attempt's workload.
func PushOf(a V3Attempt, repository, pushID, ref, commit string, state gitv1.EntryState) gitv1.PushRecord {
	run, attempt := string(a.Run), string(a.ID)
	e := gitv1.PushEntry{Ref: ref, Old: strings.Repeat("0", 40), New: commit, Kind: gitv1.KindCreate, State: state}
	switch state {
	case gitv1.EntryRefused:
		r := gitv1.ReasonRefProtected
		e.Reason = &r
	case gitv1.EntryUnknown:
		r := gitv1.ReasonJournalUnreadable
		e.Reason = &r
	}
	return gitv1.PushRecord{
		PushID: pushID, RepositoryID: repository, ReceiveID: "rcv_" + pushID,
		Actor:   gitv1.Actor{Type: gitv1.ActorWorkload, ID: attempt, RunID: &run, AttemptID: &attempt},
		GrantID: "gg_" + attempt, OriginRunID: &run, Status: gitv1.DeriveStatus([]gitv1.PushEntry{e}), Entries: []gitv1.PushEntry{e},
		CreatedAt: gitv1.Timestamp(time.Date(2026, 10, 30, 10, 5, 0, 0, time.UTC).Format(time.RFC3339)),
	}
}

// Completed3 returns a completed v3 result with the given commit outputs and
// declared outputs, with its request.
func Completed3(tb testing.TB, outputs map[string]runtimev1.Value, commits ...runtimev3.CommitOutput) runtimev3.ResultRequestV3 {
	tb.Helper()
	if commits == nil {
		commits = []runtimev3.CommitOutput{}
	}
	if outputs == nil {
		outputs = map[string]runtimev1.Value{}
	}
	zero := int64(0)
	res := runtimev3.ResultV3{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Commits: commits, Outputs: outputs, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	h, err := runtimev3.ResultHash(res)
	if err != nil {
		tb.Fatal(err)
	}
	return runtimev3.ResultRequestV3{ResultHash: h, Result: res}
}

// Deps returns the ports of a Service over the world: the source-set route is
// configured, and the v1 ports exist but fail closed (this fixture serves no v1
// attempt).
func (w *V3World) Deps() results.Dependencies {
	return results.Dependencies{
		Authority: w, Evidence: w, Contexts: v1Stub{}, Finalizer: v1Stub{}, Source: v1Stub{},
		V3: &results.DependenciesV3{Contexts: w, Pushes: w, Issued: w, Finalizer: w},
	}
}

// Service builds the Service.
func (w *V3World) Service() *results.Service {
	svc, err := results.NewService(w.Deps(), results.Options{})
	if err != nil {
		w.T.Fatal(err)
	}
	return svc
}

// Submit is one POST /result of the source-set route with the attempt's token.
func (w *V3World) Submit(svc *results.Service, a V3Attempt, req runtimev3.ResultRequestV3) (runtimev1.ResultResponse, error) {
	return svc.SubmitV3(context.Background(), results.SubmitV3Command{Attempt: a.ID, Bearer: a.Token, Request: req})
}

type v1Stub struct{}

func (v1Stub) Pinned(context.Context, domain.Scope, domain.AttemptID) (runtimev1.Context, error) {
	return runtimev1.Context{}, results.Fail(results.ErrUnavailable, nil, "no v1 attempts in this fixture")
}
func (v1Stub) Accepted(context.Context, domain.Scope, domain.AttemptID) (runtimev1.Digest, bool, error) {
	return "", false, results.Fail(results.ErrUnavailable, nil, "no v1 attempts in this fixture")
}
func (v1Stub) Commit(context.Context, results.Commit) (results.Acknowledgment, error) {
	return results.Acknowledgment{}, results.Fail(results.ErrUnavailable, nil, "no v1 attempts in this fixture")
}
func (v1Stub) Propose(context.Context, domain.Scope, domain.AttemptID, runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	return results.SourceOutcome{}, results.Fail(results.ErrUnavailable, nil, "no v1 attempts in this fixture")
}

func (w *V3World) forbid(what string) {
	if w.ForbidReads {
		w.T.Errorf("the %s was read on a path that must not read it", what)
	}
}

// Authenticate implements results.Authority: the token names the attempt, the
// route's wire must be the attempt's, and result.submit keeps its receipt
// exception (an unrevoked attempt that is no longer active still reaches the
// finalizer, which answers the original acknowledgment or a conflict).
func (w *V3World) Authenticate(_ context.Context, req results.AuthRequest) (results.Principal, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.attempts[req.Attempt]
	if !ok || a.token != req.Bearer {
		return results.Principal{}, results.Fail(results.ErrUnauthenticated, nil, "the credential is not valid")
	}
	if req.Wire != 0 && req.Wire != a.wire {
		return results.Principal{}, results.Fail(results.ErrContextVersionMismatch, nil, "the attempt belongs to another runtime version")
	}
	if a.revoked || (a.status != "active" && !(a.hash != "" && req.Action == results.ActionResultSubmit)) {
		return results.Principal{}, results.Fail(results.ErrAttemptRevoked, nil, "the attempt is no longer authorized")
	}
	return results.Principal{Scope: a.scope, RunID: a.run, AttemptID: a.id, Action: req.Action, Proof: a.id}, nil
}

// Bind implements results.Authority.
func (w *V3World) Bind(ctx context.Context, p results.Principal) (context.Context, error) {
	if id, ok := p.Proof.(domain.AttemptID); !ok || id != p.AttemptID {
		return ctx, results.Fail(results.ErrUnauthenticated, nil, "the principal was not issued by this authority")
	}
	return ctx, nil
}

// PinnedV3 implements results.ContextsV3.
func (w *V3World) PinnedV3(_ context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimev3.ContextV3, error) {
	w.forbid("pinned context")
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ContextReads++
	a, ok := w.attempts[attempt]
	if !ok || a.scope != scope {
		return runtimev3.ContextV3{}, results.Fail(results.ErrNotFound, nil, "the attempt was not found")
	}
	if a.wire != results.WireV3 {
		return runtimev3.ContextV3{}, results.Fail(results.ErrContextVersionMismatch, nil, "the attempt belongs to another runtime version")
	}
	return a.context, nil
}

// References implements results.Evidence.
func (w *V3World) References(_ context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]results.ArtifactEvidence, error) {
	w.forbid("artifact evidence")
	w.mu.Lock()
	defer w.mu.Unlock()
	w.EvidenceReads++
	out := map[domain.ArtifactID]results.ArtifactEvidence{}
	for _, id := range ids {
		if ev, ok := w.evidence[id]; ok && ev.Scope == scope && ev.Attempt == attempt {
			out[id] = ev
		}
	}
	return out, nil
}

// Protect implements results.Evidence.
func (w *V3World) Protect(_ context.Context, _ domain.Scope, _ domain.AttemptID, ids []domain.ArtifactID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Pinned = append(w.Pinned, ids...)
	return nil
}

// Push implements results.Pushes.
func (w *V3World) Push(_ context.Context, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error) {
	w.forbid("push record")
	w.mu.Lock()
	defer w.mu.Unlock()
	w.PushReads++
	if w.PushErr != nil {
		return gitv1.PushRecord{}, results.Fail(results.ErrUnavailable, w.PushErr, "the push record could not be read")
	}
	rec, ok := w.records[repositoryID+"/"+pushID]
	if !ok {
		return gitv1.PushRecord{}, results.Fail(results.ErrNotFound, nil, "push record not found")
	}
	return rec, nil
}

// Issued implements results.IssuedGrants.
func (w *V3World) Issued(_ context.Context, scope domain.Scope, attempt domain.AttemptID) ([]gitv1.GrantClaims, error) {
	w.forbid("issued grants")
	w.mu.Lock()
	defer w.mu.Unlock()
	w.IssuedReads++
	if w.IssuedErr != nil {
		return nil, results.Fail(results.ErrUnavailable, w.IssuedErr, "the issued grants could not be read")
	}
	return append([]gitv1.GrantClaims(nil), w.issued[attempt]...), nil
}

// Accepted implements results.FinalizerV3.
func (w *V3World) Accepted(_ context.Context, _ domain.Scope, attempt domain.AttemptID) (runtimev1.Digest, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.attempts[attempt]
	if a == nil || a.hash == "" {
		return "", false, nil
	}
	return a.hash, true, nil
}

// CommitV3 implements results.FinalizerV3 with the contract points results relies
// on: receipt first, a different result conflicts, an ended attempt is revoked,
// and the outputs are checked again against the pinned context and the grants the
// caller loaded (a violation is recorded as a failed attempt, as execution does).
func (w *V3World) CommitV3(_ context.Context, c results.CommitV3) (results.Acknowledgment, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.attempts[c.AttemptID]
	if a == nil || a.scope != c.Scope {
		return results.Acknowledgment{}, results.Fail(results.ErrNotFound, nil, "the attempt was not found")
	}
	if a.hash != "" {
		if a.hash != c.Request.ResultHash {
			return results.Acknowledgment{}, results.Fail(results.ErrResultConflict, nil, "a different result was already accepted")
		}
		return a.ack, w.loseLocked()
	}
	if a.status != "active" {
		return results.Acknowledgment{}, results.Fail(results.ErrAttemptRevoked, nil, "the attempt is no longer authorized")
	}
	w.Commits = append(w.Commits, c)
	if w.CommitErr != nil {
		err := w.CommitErr
		w.CommitErr = nil
		return results.Acknowledgment{}, err
	}
	status := runtimev1.AttemptSucceeded
	if err := runtimev3.CheckResult(c.Request.Result, a.context, c.Artifacts, c.Issued...); err != nil {
		status = runtimev1.AttemptFailed
	}
	a.hash, a.status = c.Request.ResultHash, "finished"
	a.ack = results.Acknowledgment{AttemptID: a.id, ResultHash: a.hash, Status: status, AcceptedAt: w.Clock.Now()}
	return a.ack, w.loseLocked()
}

func (w *V3World) loseLocked() error {
	if w.LoseAck > 0 {
		w.LoseAck--
		return results.Fail(results.ErrUnavailable, errors.New("acknowledgment lost"), "the acknowledgment was lost")
	}
	return nil
}

// Accept reports whether the attempt has an accepted hash and which.
func (w *V3World) Accept(a V3Attempt) (runtimev1.Digest, bool) {
	h, ok, _ := w.Accepted(context.Background(), a.Scope, a.ID)
	return h, ok
}
