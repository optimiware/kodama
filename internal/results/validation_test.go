package results_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
)

// happyContext is the frozen context of the contract's happy-path scenario: a
// reader attempt declaring one artifact output "report".
func happyContext(t testing.TB) runtimev1.Context {
	t.Helper()
	data, err := os.ReadFile("../../contracts/runtime/v1/fixtures/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var all []struct {
		Name  string `json:"name"`
		Steps []struct {
			Op   string          `json:"op"`
			Body json.RawMessage `json:"body"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.Name == "happy_path" {
			var resp runtimev1.AdmitResponse
			if err := json.Unmarshal(s.Steps[0].Body, &resp); err != nil {
				t.Fatal(err)
			}
			return resp.Context
		}
	}
	t.Fatal("no happy_path scenario")
	return runtimev1.Context{}
}

func ptr[T any](v T) *T { return &v }

func completed(outputs map[string]runtimev1.Value, diag ...domain.ArtifactID) runtimev1.Result {
	if diag == nil {
		diag = []domain.ArtifactID{}
	}
	return runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: outputs, DiagnosticArtifactIDs: diag}
}

func fileEvidence(c runtimev1.Context, id domain.ArtifactID, role runtimev1.ArtifactRole) results.ArtifactEvidence {
	return results.ArtifactEvidence{ID: id, Scope: c.Scope(), Attempt: c.AttemptID, Kind: runtimev1.KindFile, Role: role, Usable: true,
		Digest: "sha256:" + runtimev1.Digest("0000000000000000000000000000000000000000000000000000000000000000")}
}

func evidence(es ...results.ArtifactEvidence) map[domain.ArtifactID]results.ArtifactEvidence {
	m := map[domain.ArtifactID]results.ArtifactEvidence{}
	for _, e := range es {
		m[e.ID] = e
	}
	return m
}

// TestEvidenceValidation covers PX-10 (digest/type/provenance validation,
// required-output gating) and WS-04 (source rules): scalar versus artifact
// outputs, wrong kinds and provenance, independent diagnostic checks, nonwriter
// source rejection and the failed/needs-input envelopes.
func TestEvidenceValidation(t *testing.T) {
	c := happyContext(t)
	report := map[string]runtimev1.Value{"report": runtimev1.StringValue("art_tests")}

	t.Run("only declared artifact outputs and diagnostics are artifact references", func(t *testing.T) {
		scalar := c
		scalar.Outputs = []runtimev1.OutputDecl{{Name: "label", Type: runtimev1.OutputString}, {Name: "report", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/r")}}
		res := completed(map[string]runtimev1.Value{"label": runtimev1.StringValue("art_looks_like_an_id"), "report": runtimev1.StringValue("art_tests")}, "art_log")
		got := results.ReferencedArtifacts(res, scalar)
		if len(got) != 2 || got[0] != "art_log" || got[1] != "art_tests" {
			t.Fatalf("references = %v", got)
		}
		// A failed result's outputs are empty by contract, so only diagnostics remain.
		failed := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("command_failed"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{"art_log"}}
		if got := results.ReferencedArtifacts(failed, scalar); len(got) != 1 || got[0] != "art_log" {
			t.Fatalf("failed references = %v", got)
		}
	})

	t.Run("a valid completed result yields trusted metadata and protection", func(t *testing.T) {
		v, err := results.Validate(completed(report, "art_log"), c, evidence(fileEvidence(c, "art_tests", runtimev1.RoleOutput), fileEvidence(c, "art_log", runtimev1.RoleDiagnostic)))
		if err != nil || v.OutputViolation != nil || len(v.Protect) != 2 || len(v.Artifacts) != 2 {
			t.Fatalf("%+v %v", v, err)
		}
	})

	t.Run("a missing, foreign or other attempt's artifact is one not_found", func(t *testing.T) {
		other := fileEvidence(c, "art_tests", runtimev1.RoleOutput)
		other.Attempt = "att_other"
		foreign := fileEvidence(c, "art_tests", runtimev1.RoleOutput)
		foreign.Scope.TenantID = "t_globex"
		foreignProject := fileEvidence(c, "art_tests", runtimev1.RoleOutput)
		foreignProject.Scope.ProjectID = "p_other"
		for name, ev := range map[string]map[domain.ArtifactID]results.ArtifactEvidence{
			"absent": evidence(), "other attempt": evidence(other), "other tenant": evidence(foreign), "other project": evidence(foreignProject),
		} {
			_, err := results.Validate(completed(report), c, ev)
			if !errors.Is(err, results.ErrNotFound) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})

	t.Run("an unfinished upload is incomplete, and is reported before a missing output", func(t *testing.T) {
		ev := fileEvidence(c, "art_tests", runtimev1.RoleOutput)
		ev.Usable = false
		if _, err := results.Validate(completed(report), c, evidence(ev)); !errors.Is(err, results.ErrArtifactIncomplete) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("a wrong kind, role or provenance is a durable output violation, not a request error", func(t *testing.T) {
		oci := fileEvidence(c, "art_tests", runtimev1.RoleOutput)
		oci.Kind = runtimev1.KindOCI
		oci.Provenance = results.Provenance{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: "1111111111111111111111111111111111111111"}
		role := fileEvidence(c, "art_tests", runtimev1.RoleBundle)
		moved := fileEvidence(c, "art_tests", runtimev1.RoleOutput)
		moved.Provenance = results.Provenance{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: "2222222222222222222222222222222222222222"}
		for name, ev := range map[string]results.ArtifactEvidence{"kind": oci, "role": role, "provenance": moved} {
			v, err := results.Validate(completed(report), c, evidence(ev))
			if err != nil || !errors.Is(v.OutputViolation, results.ErrOutputInvalid) {
				t.Fatalf("%s: %+v %v", name, v, err)
			}
			if len(v.Protect) != 0 {
				t.Fatalf("%s: the outputs of a violating result must not be protected as consumed", name)
			}
			// Execution classifies the distrusted reference as a wrong output itself.
			if cerr := runtimev1.CheckResult(completed(report), c, v.Artifacts); cerr == nil {
				t.Fatalf("%s: execution would accept the distrusted artifact", name)
			}
		}
		// Provenance equal to the pinned input source is trusted.
		good := fileEvidence(c, "art_tests", runtimev1.RoleOutput)
		good.Provenance = results.Provenance{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: "1111111111111111111111111111111111111111"}
		if v, err := results.Validate(completed(report), c, evidence(good)); err != nil || v.OutputViolation != nil {
			t.Fatalf("%+v %v", v, err)
		}
	})

	t.Run("missing and mistyped outputs fail finalization but keep verified diagnostics", func(t *testing.T) {
		log := fileEvidence(c, "art_log", runtimev1.RoleDiagnostic)
		v, err := results.Validate(completed(map[string]runtimev1.Value{}, "art_log"), c, evidence(log))
		if err != nil || v.OutputViolation == nil {
			t.Fatalf("%+v %v", v, err)
		}
		if len(v.Protect) != 1 || v.Protect[0] != "art_log" {
			t.Fatalf("protect = %v", v.Protect)
		}
		v, err = results.Validate(completed(map[string]runtimev1.Value{"report": runtimev1.IntValue(1)}, "art_log"), c, evidence(log))
		if err != nil || v.OutputViolation == nil {
			t.Fatalf("%+v %v", v, err)
		}
	})

	t.Run("a diagnostic is never a trusted output, even when listed as both", func(t *testing.T) {
		log := fileEvidence(c, "art_log", runtimev1.RoleDiagnostic)
		res := completed(map[string]runtimev1.Value{"report": runtimev1.StringValue("art_log")}, "art_log")
		v, err := results.Validate(res, c, evidence(log))
		if err != nil || v.OutputViolation == nil {
			t.Fatalf("%+v %v", v, err)
		}
		// Execution's own check rejects the output as a wrong output and the
		// verified diagnostic stays protected.
		var ce *runtimev1.ContractError
		if cerr := runtimev1.CheckResult(res, c, v.Artifacts); !errors.As(cerr, &ce) || ce.Code != runtimev1.CodeOutputInvalid {
			t.Fatalf("execution would accept the diagnostic as the output: %v", cerr)
		}
		if len(v.Protect) != 1 || v.Protect[0] != "art_log" {
			t.Fatalf("protect = %v", v.Protect)
		}
	})

	t.Run("diagnostics are verified independently of outputs", func(t *testing.T) {
		// An early output violation must not let an unverified diagnostic through.
		if _, err := results.Validate(completed(map[string]runtimev1.Value{}, "art_foreign"), c, evidence()); !errors.Is(err, results.ErrNotFound) {
			t.Fatalf("foreign diagnostic with a missing output: %v", err)
		}
		notDiag := fileEvidence(c, "art_log", runtimev1.RoleOutput)
		if _, err := results.Validate(completed(map[string]runtimev1.Value{}, "art_log"), c, evidence(notDiag)); !errors.Is(err, results.ErrOutputInvalid) {
			t.Fatalf("an output used as a diagnostic: %v", err)
		}
		unfinished := fileEvidence(c, "art_log", runtimev1.RoleDiagnostic)
		unfinished.Usable = false
		if _, err := results.Validate(completed(map[string]runtimev1.Value{}, "art_log"), c, evidence(unfinished)); !errors.Is(err, results.ErrArtifactIncomplete) {
			t.Fatalf("unfinished diagnostic: %v", err)
		}
	})

	t.Run("the writer source rule: a nonwriter reports no commit, a writer must", func(t *testing.T) {
		withCommit := completed(report)
		withCommit.SourceCommit = ptr(runtimev1.GitCommit("3333333333333333333333333333333333333333"))
		ev := evidence(fileEvidence(c, "art_tests", runtimev1.RoleOutput))
		if v, err := results.Validate(withCommit, c, ev); err != nil || v.OutputViolation == nil {
			t.Fatalf("nonwriter source: %+v %v", v, err)
		}
		writer := c
		writer.Workspace.Access = runtimev1.AccessWrite
		writer.Workspace.Source = &runtimev1.Source{
			RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: "1111111111111111111111111111111111111111",
			Branch: ptr("kodama/wi_1"), ExpectedTip: ptr(runtimev1.GitCommit("1111111111111111111111111111111111111111")),
		}
		if v, err := results.Validate(completed(report), writer, ev); err != nil || v.OutputViolation == nil {
			t.Fatalf("writer without a source commit: %+v %v", v, err)
		}
		if v, err := results.Validate(withCommit, writer, ev); err != nil || v.OutputViolation != nil {
			t.Fatalf("writer with a source commit: %+v %v", v, err)
		}
	})

	t.Run("failed and needs-input envelopes keep diagnostics and expose neither outputs nor source", func(t *testing.T) {
		failed := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("command_failed"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{"art_log"}}
		log := evidence(fileEvidence(c, "art_log", runtimev1.RoleDiagnostic))
		if v, err := results.Validate(failed, c, log); err != nil || v.OutputViolation != nil || len(v.Protect) != 1 {
			t.Fatalf("%+v %v", v, err)
		}
		needs := runtimev1.Result{Outcome: runtimev1.OutcomeNeedsInput, Question: ptr("Which environment?"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		if _, err := results.Validate(needs, c, nil); err != nil {
			t.Fatal(err)
		}
		withOutputs := failed
		withOutputs.Outputs = report
		if _, err := results.Validate(withOutputs, c, log); !errors.Is(err, results.ErrInvalidRequest) {
			t.Fatalf("a failed result with success outputs: %v", err)
		}
		withSource := needs
		withSource.SourceCommit = ptr(runtimev1.GitCommit("3333333333333333333333333333333333333333"))
		if _, err := results.Validate(withSource, c, nil); !errors.Is(err, results.ErrInvalidRequest) {
			t.Fatalf("a needs-input result with a source: %v", err)
		}
		// A foreign diagnostic of a failed result is the same not_found.
		if _, err := results.Validate(failed, c, evidence()); !errors.Is(err, results.ErrNotFound) {
			t.Fatalf("foreign diagnostic: %v", err)
		}
	})
}

// ---- service ordering ------------------------------------------------------

type fakes struct {
	mu       sync.Mutex
	calls    []string
	authErr  error
	bindErr  error
	pinned   runtimev1.Context
	refs     map[domain.ArtifactID]results.ArtifactEvidence
	refsErr  error
	accepted runtimev1.Digest
	commits  []results.Commit
	commitFn func(results.Commit) (results.Acknowledgment, error)
	protect  []domain.ArtifactID
	source   func() (results.SourceOutcome, error)
}

func (f *fakes) note(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakes) Authenticate(_ context.Context, r results.AuthRequest) (results.Principal, error) {
	f.note("authenticate")
	if f.authErr != nil {
		return results.Principal{}, f.authErr
	}
	return results.Principal{Scope: f.pinned.Scope(), RunID: f.pinned.RunID, AttemptID: r.Attempt, Action: r.Action, Proof: "p"}, nil
}
func (f *fakes) Bind(ctx context.Context, _ results.Principal) (context.Context, error) {
	f.note("bind")
	return ctx, f.bindErr
}
func (f *fakes) Pinned(context.Context, domain.Scope, domain.AttemptID) (runtimev1.Context, error) {
	f.note("pinned")
	return f.pinned, nil
}
func (f *fakes) References(_ context.Context, _ domain.Scope, _ domain.AttemptID, _ []domain.ArtifactID) (map[domain.ArtifactID]results.ArtifactEvidence, error) {
	f.note("references")
	return f.refs, f.refsErr
}
func (f *fakes) Protect(_ context.Context, _ domain.Scope, _ domain.AttemptID, ids []domain.ArtifactID) error {
	f.note("protect")
	f.protect = append(f.protect, ids...)
	return nil
}
func (f *fakes) Accepted(context.Context, domain.Scope, domain.AttemptID) (runtimev1.Digest, bool, error) {
	f.note("accepted")
	return f.accepted, f.accepted != "", nil
}
func (f *fakes) Commit(_ context.Context, c results.Commit) (results.Acknowledgment, error) {
	f.note("commit")
	f.commits = append(f.commits, c)
	if f.commitFn != nil {
		return f.commitFn(c)
	}
	return results.Acknowledgment{AttemptID: c.AttemptID, ResultHash: c.Request.ResultHash, Status: runtimev1.AttemptSucceeded, AcceptedAt: time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)}, nil
}
func (f *fakes) Propose(context.Context, domain.Scope, domain.AttemptID, runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	f.note("propose")
	return f.source()
}

func newService(t *testing.T, f *fakes) *results.Service {
	t.Helper()
	svc, err := results.NewService(results.Dependencies{Authority: f, Contexts: f, Evidence: f, Finalizer: f, Source: f}, results.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func request(t *testing.T, res runtimev1.Result) runtimev1.ResultRequest {
	t.Helper()
	h, err := runtimev1.ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	return runtimev1.ResultRequest{ResultHash: h, Result: res}
}

func TestSubmitOrdering(t *testing.T) {
	c := happyContext(t)
	report := map[string]runtimev1.Value{"report": runtimev1.StringValue("art_tests")}
	good := fileEvidence(c, "art_tests", runtimev1.RoleOutput)
	submit := func(f *fakes, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
		return newService(t, f).Submit(context.Background(), results.SubmitCommand{Attempt: c.AttemptID, Bearer: "tok", Request: req})
	}

	t.Run("the frozen order for a new result", func(t *testing.T) {
		f := &fakes{pinned: c, refs: evidence(good)}
		resp, err := submit(f, request(t, completed(report)))
		if err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded || resp.AcceptedAt == "" {
			t.Fatalf("%+v %v", resp, err)
		}
		want := []string{"authenticate", "bind", "accepted", "pinned", "references", "protect", "commit"}
		if len(f.calls) != len(want) {
			t.Fatalf("calls %v", f.calls)
		}
		for i := range want {
			if f.calls[i] != want[i] {
				t.Fatalf("calls %v, want %v", f.calls, want)
			}
		}
		if len(f.commits[0].Artifacts) != 1 {
			t.Fatalf("execution receives the verified metadata: %+v", f.commits[0].Artifacts)
		}
	})

	t.Run("an already accepted attempt reaches execution without touching artifacts", func(t *testing.T) {
		req := request(t, completed(report))
		f := &fakes{pinned: c, accepted: req.ResultHash, refsErr: errors.New("artifact store outage")}
		if _, err := submit(f, req); err != nil {
			t.Fatalf("a replay must not depend on the artifact store: %v", err)
		}
		other := request(t, completed(map[string]runtimev1.Value{"report": runtimev1.StringValue("art_other")}))
		f = &fakes{pinned: c, accepted: req.ResultHash, refsErr: errors.New("artifact store outage"),
			commitFn: func(results.Commit) (results.Acknowledgment, error) {
				return results.Acknowledgment{}, results.Fail(results.ErrResultConflict, nil, "conflict")
			}}
		if _, err := submit(f, other); !errors.Is(err, results.ErrResultConflict) {
			t.Fatalf("a different hash: %v", err)
		}
		for _, call := range f.calls {
			if call == "references" || call == "pinned" || call == "protect" {
				t.Fatalf("receipt decisions must not read artifacts: %v", f.calls)
			}
		}
	})

	t.Run("an unauthenticated or unbound request does nothing else", func(t *testing.T) {
		f := &fakes{pinned: c, authErr: results.Fail(results.ErrUnauthenticated, nil, "no")}
		if _, err := submit(f, request(t, completed(report))); !errors.Is(err, results.ErrUnauthenticated) || len(f.calls) != 1 {
			t.Fatalf("%v %v", err, f.calls)
		}
		f = &fakes{pinned: c, bindErr: results.Fail(results.ErrUnauthenticated, nil, "no")}
		if _, err := submit(f, request(t, completed(report))); !errors.Is(err, results.ErrUnauthenticated) || len(f.commits) != 0 {
			t.Fatalf("%v %v", err, f.calls)
		}
	})

	t.Run("a hash that does not cover the result is rejected before any decision", func(t *testing.T) {
		req := request(t, completed(report))
		req.ResultHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		f := &fakes{pinned: c, refs: evidence(good)}
		if _, err := submit(f, req); !errors.Is(err, results.ErrResultHashMismatch) {
			t.Fatalf("%v", err)
		}
		if len(f.commits) != 0 || len(f.protect) != 0 {
			t.Fatalf("calls %v", f.calls)
		}
	})

	t.Run("evidence errors stay precise and never reach execution", func(t *testing.T) {
		f := &fakes{pinned: c, refs: evidence()}
		if _, err := submit(f, request(t, completed(report))); !errors.Is(err, results.ErrNotFound) || len(f.commits) != 0 {
			t.Fatalf("%v", err)
		}
		f = &fakes{pinned: c, refsErr: errors.New("outage")}
		if _, err := submit(f, request(t, completed(report))); !errors.Is(err, results.ErrUnavailable) || len(f.commits) != 0 {
			t.Fatalf("an artifact outage is unavailable: %v", err)
		}
	})

	t.Run("a violating completed result still reaches execution for the durable failure", func(t *testing.T) {
		f := &fakes{pinned: c, refs: evidence()}
		if _, err := submit(f, request(t, completed(map[string]runtimev1.Value{}))); err != nil {
			t.Fatal(err)
		}
		if len(f.commits) != 1 {
			t.Fatalf("calls %v", f.calls)
		}
	})

	t.Run("an acknowledgment that does not match the request is never delivered", func(t *testing.T) {
		f := &fakes{pinned: c, refs: evidence(good), commitFn: func(c results.Commit) (results.Acknowledgment, error) {
			return results.Acknowledgment{AttemptID: c.AttemptID, ResultHash: "sha256:" + runtimev1.Digest("1111111111111111111111111111111111111111111111111111111111111111"), Status: runtimev1.AttemptSucceeded, AcceptedAt: time.Now()}, nil
		}}
		if _, err := submit(f, request(t, completed(report))); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		f = &fakes{pinned: c, refs: evidence(good), commitFn: func(c results.Commit) (results.Acknowledgment, error) {
			return results.Acknowledgment{AttemptID: c.AttemptID, ResultHash: c.Request.ResultHash, Status: "weird", AcceptedAt: time.Now()}, nil
		}}
		if _, err := submit(f, request(t, completed(report))); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("an unknown status: %v", err)
		}
	})

	t.Run("an unclassified dependency error is unavailable", func(t *testing.T) {
		f := &fakes{pinned: c, refs: evidence(good), commitFn: func(results.Commit) (results.Acknowledgment, error) {
			return results.Acknowledgment{}, errors.New("commit status unknown")
		}}
		if _, err := submit(f, request(t, completed(report))); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
	})
}

func TestProposeSourceCoordination(t *testing.T) {
	c := happyContext(t)
	req := runtimev1.SourceProposalRequest{
		Commit: "3333333333333333333333333333333333333333", ExpectedTip: "1111111111111111111111111111111111111111", BundleArtifactID: "art_bundle",
	}
	run := func(f *fakes) (results.SourceOutcome, error) {
		return newService(t, f).ProposeSource(context.Background(), results.ProposeCommand{Attempt: c.AttemptID, Bearer: "tok", Request: req})
	}
	t.Run("the bundle is protected before the intent is proposed", func(t *testing.T) {
		accepted := runtimev1.GitCommit(req.Commit)
		f := &fakes{pinned: c, source: func() (results.SourceOutcome, error) {
			return results.SourceOutcome{HTTPStatus: 200, Response: runtimev1.SourceProposalResponse{Status: runtimev1.PublishAccepted, Commit: req.Commit, AcceptedCommit: &accepted}}, nil
		}}
		out, err := run(f)
		if err != nil || out.HTTPStatus != 200 {
			t.Fatalf("%+v %v", out, err)
		}
		want := []string{"authenticate", "bind", "protect", "propose"}
		for i, w := range want {
			if f.calls[i] != w {
				t.Fatalf("calls %v", f.calls)
			}
		}
	})
	t.Run("conflicts keep the observed tip", func(t *testing.T) {
		f := &fakes{pinned: c, source: func() (results.SourceOutcome, error) {
			return results.SourceOutcome{}, &results.PublicationConflict{Reason: "tip_moved", ObservedTip: "2222222222222222222222222222222222222222"}
		}}
		_, err := run(f)
		var pc *results.PublicationConflict
		if !errors.As(err, &pc) || pc.ObservedTip == "" || !errors.Is(err, results.ErrPublicationConflict) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an invalid outcome from the owner is unavailable, never success", func(t *testing.T) {
		f := &fakes{pinned: c, source: func() (results.SourceOutcome, error) {
			return results.SourceOutcome{HTTPStatus: 200, Response: runtimev1.SourceProposalResponse{Status: "weird"}}, nil
		}}
		if _, err := run(f); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a denied principal reaches nothing", func(t *testing.T) {
		f := &fakes{pinned: c, authErr: results.Fail(results.ErrAttemptRevoked, nil, "revoked")}
		if _, err := run(f); !errors.Is(err, results.ErrAttemptRevoked) || len(f.calls) != 1 {
			t.Fatalf("%v %v", err, f.calls)
		}
	})
}

func TestNewServiceFailsClosed(t *testing.T) {
	f := &fakes{}
	for name, d := range map[string]results.Dependencies{
		"authority": {Contexts: f, Evidence: f, Finalizer: f, Source: f},
		"contexts":  {Authority: f, Evidence: f, Finalizer: f, Source: f},
		"evidence":  {Authority: f, Contexts: f, Finalizer: f, Source: f},
		"finalizer": {Authority: f, Contexts: f, Evidence: f, Source: f},
		"source":    {Authority: f, Contexts: f, Evidence: f, Finalizer: f},
	} {
		if _, err := results.NewService(d, results.Options{}); err == nil {
			t.Errorf("a service without %s was constructed", name)
		}
	}
}

// TestFailedResultReasonValidation: the reasons the entrypoint reports for an
// output failure are valid failed results whose diagnostics are still verified
// like any other; a reason that is not a reason code never reaches execution.
func TestFailedResultReasonValidation(t *testing.T) {
	c := happyContext(t)
	failed := func(reason string, diag ...domain.ArtifactID) runtimev1.Result {
		if diag == nil {
			diag = []domain.ArtifactID{}
		}
		return runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(0)), Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: diag}
	}
	const log = domain.ArtifactID("art_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	for _, reason := range []string{"output_invalid", "output_upload_failed"} {
		v, err := results.Validate(failed(reason, log), c, evidence(fileEvidence(c, log, runtimev1.RoleDiagnostic)))
		if err != nil || v.OutputViolation != nil || len(v.Protect) != 1 || v.Protect[0] != log {
			t.Errorf("%s: %+v %v: the diagnostic must be verified and retained", reason, v, err)
		}
		// The diagnostics of a failed result are verified like any other: a
		// non-diagnostic artifact is refused whatever the reason.
		if _, err := results.Validate(failed(reason, log), c, evidence(fileEvidence(c, log, runtimev1.RoleOutput))); !errors.Is(err, results.ErrOutputInvalid) {
			t.Errorf("%s with an output-role diagnostic: %v", reason, err)
		}
		if _, err := results.Validate(failed(reason, log), c, nil); !errors.Is(err, results.ErrNotFound) {
			t.Errorf("%s with an absent diagnostic: %v", reason, err)
		}
	}
	for _, bad := range []string{"Output Invalid!", "", "output invalid", "OUTPUT_INVALID"} {
		res := failed(bad)
		if _, err := results.Validate(res, c, nil); !errors.Is(err, results.ErrInvalidRequest) {
			t.Errorf("reason %q: %v, want an invalid request", bad, err)
		}
	}
}
