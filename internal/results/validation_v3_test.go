package results_test

import (
	"errors"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
	rt "github.com/optimiware/kodama/internal/results/resultstest"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

const (
	task = "refs/heads/kodama/work/wi_1042"
	c3   = "3333333333333333333333333333333333333333"
)

func attemptV3(t *testing.T) rt.V3Attempt {
	t.Helper()
	return rt.NewV3World(t).NewAttempt(rtest.WriterContext)
}

func Test_unit_check_push_verdicts(t *testing.T) {
	a := attemptV3(t)
	out := runtimev3.CommitOutput{RepositoryID: "repo_01", Ref: task, Commit: c3, PushID: "pu_07"}
	rec := func(mut func(*gitv1.PushRecord), state gitv1.EntryState) gitv1.PushRecord {
		r := rt.PushOf(a, "repo_01", "pu_07", task, c3, state)
		if mut != nil {
			mut(&r)
		}
		return r
	}
	for name, c := range map[string]struct {
		rec  gitv1.PushRecord
		want results.PushVerdict
	}{
		"applied":                            {rec(nil, gitv1.EntryApplied), results.PushApplied},
		"authorized":                         {rec(nil, gitv1.EntryAuthorized), results.PushPending},
		"committing":                         {rec(nil, gitv1.EntryCommitting), results.PushPending},
		"unknown":                            {rec(nil, gitv1.EntryUnknown), results.PushPending},
		"refused":                            {rec(nil, gitv1.EntryRefused), results.PushRefused},
		"a state this version does not know": {rec(func(r *gitv1.PushRecord) { r.Entries[0].State = "teleported" }, gitv1.EntryApplied), results.PushPending},
		"another push id":                    {rec(func(r *gitv1.PushRecord) { r.PushID = "pu_99" }, gitv1.EntryApplied), results.PushRefused},
		"another repository":                 {rec(func(r *gitv1.PushRecord) { r.RepositoryID = "repo_02" }, gitv1.EntryApplied), results.PushRefused},
		"a person":                           {rec(func(r *gitv1.PushRecord) { r.Actor = gitv1.Actor{Type: gitv1.ActorUser, ID: "u_1"} }, gitv1.EntryApplied), results.PushRefused},
		"another attempt":                    {rec(func(r *gitv1.PushRecord) { x := "att_other"; r.Actor.AttemptID = &x }, gitv1.EntryApplied), results.PushRefused},
		"a workload without attempt":         {rec(func(r *gitv1.PushRecord) { r.Actor.AttemptID = nil }, gitv1.EntryApplied), results.PushRefused},
		"another run":                        {rec(func(r *gitv1.PushRecord) { x := "run_other"; r.Actor.RunID = &x }, gitv1.EntryApplied), results.PushRefused},
		"another origin run":                 {rec(func(r *gitv1.PushRecord) { x := "run_other"; r.OriginRunID = &x }, gitv1.EntryApplied), results.PushRefused},
		"no entry for the ref":               {rec(func(r *gitv1.PushRecord) { r.Entries[0].Ref = "refs/heads/kodama/work/wi_9" }, gitv1.EntryApplied), results.PushRefused},
		"another commit":                     {rec(func(r *gitv1.PushRecord) { r.Entries[0].New = strings.Repeat("4", 40) }, gitv1.EntryApplied), results.PushRefused},
		"a deletion":                         {rec(func(r *gitv1.PushRecord) { r.Entries[0].New = strings.Repeat("0", 40) }, gitv1.EntryApplied), results.PushRefused},
		"two entries for the ref":            {rec(func(r *gitv1.PushRecord) { r.Entries = append(r.Entries, r.Entries[0]) }, gitv1.EntryApplied), results.PushRefused},
	} {
		if got := results.CheckPush(out, c.rec, a.Run, a.ID); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	// Another entry beside the output's is irrelevant to it.
	multi := rec(func(r *gitv1.PushRecord) {
		r.Entries = append([]gitv1.PushEntry{{Ref: "refs/heads/kodama/agents/wi_1042/run_01/x", Old: strings.Repeat("0", 40), New: strings.Repeat("5", 40), Kind: gitv1.KindCreate, State: gitv1.EntryApplied}}, r.Entries...)
	}, gitv1.EntryCommitting)
	if got := results.CheckPush(out, multi, a.Run, a.ID); got != results.PushPending {
		t.Errorf("a multi-ref push is judged by the output's own ref: %s", got)
	}
}

func Test_unit_verify_commits_classes(t *testing.T) {
	a := attemptV3(t)
	res := runtimev3.ResultV3{Commits: []runtimev3.CommitOutput{{RepositoryID: "repo_01", Ref: task, Commit: c3, PushID: "pu_07"}}}
	lookup := func(rec gitv1.PushRecord, err error) func(runtimev3.CommitOutput) (gitv1.PushRecord, error) {
		return func(runtimev3.CommitOutput) (gitv1.PushRecord, error) { return rec, err }
	}
	applied := rt.PushOf(a, "repo_01", "pu_07", task, c3, gitv1.EntryApplied)
	if err := results.VerifyCommits(res, a.Run, a.ID, lookup(applied, nil)); err != nil {
		t.Fatal(err)
	}
	if err := results.VerifyCommits(runtimev3.ResultV3{}, a.Run, a.ID, lookup(gitv1.PushRecord{}, errors.New("must not be called"))); err != nil {
		t.Fatalf("no outputs, nothing to verify: %v", err)
	}
	if err := results.VerifyCommits(res, a.Run, a.ID, lookup(gitv1.PushRecord{}, results.Fail(results.ErrNotFound, nil, "none"))); !errors.Is(err, results.ErrOutputInvalid) {
		t.Fatalf("no record: %v", err)
	}
	boom := errors.New("down")
	if err := results.VerifyCommits(res, a.Run, a.ID, lookup(gitv1.PushRecord{}, boom)); !errors.Is(err, boom) {
		t.Fatalf("an outage keeps its class: %v", err)
	}
	if err := results.VerifyCommits(res, a.Run, a.ID, lookup(rt.PushOf(a, "repo_01", "pu_07", task, c3, gitv1.EntryAuthorized), nil)); !errors.Is(err, results.ErrUnavailable) {
		t.Fatalf("pending: %v", err)
	}
}

func Test_unit_validate_v3_trusts_the_pinned_input_only(t *testing.T) {
	a := attemptV3(t)
	digest := string(*a.Context.Workspace.SourceSetDigest)
	src := a.Context.Workspace.Sources[0]
	res := runtimev3.ResultV3{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Commits: []runtimev3.CommitOutput{}, DiagnosticArtifactIDs: []domain.ArtifactID{},
		Outputs: map[string]runtimev1.Value{"report": runtimev1.StringValue("art_0001")}}
	ev := func(p results.Provenance, role runtimev1.ArtifactRole) map[domain.ArtifactID]results.ArtifactEvidence {
		return map[domain.ArtifactID]results.ArtifactEvidence{"art_0001": {ID: "art_0001", Scope: a.Scope, Attempt: a.ID, Kind: runtimev1.KindFile, Role: role, Usable: true, Provenance: p}}
	}
	for name, c := range map[string]struct {
		prov  results.Provenance
		role  runtimev1.ArtifactRole
		trust bool
	}{
		"no source":                 {results.Provenance{}, runtimev1.RoleOutput, true},
		"the pinned source set":     {results.Provenance{SourceSet: digest}, runtimev1.RoleOutput, true},
		"one pinned source":         {results.Provenance{RepositoryURL: src.RepositoryURL, Commit: src.Commit}, runtimev1.RoleOutput, true},
		"another source set":        {results.Provenance{SourceSet: "sha256:" + strings.Repeat("9", 64)}, runtimev1.RoleOutput, false},
		"a set mixed with a commit": {results.Provenance{SourceSet: digest, Commit: src.Commit}, runtimev1.RoleOutput, false},
		"a newly pushed commit":     {results.Provenance{RepositoryURL: src.RepositoryURL, Commit: strings.Repeat("7", 40)}, runtimev1.RoleOutput, false},
		"another repository":        {results.Provenance{RepositoryURL: "https://git.example.invalid/git/other.git", Commit: src.Commit}, runtimev1.RoleOutput, false},
		"a diagnostic role":         {results.Provenance{}, runtimev1.RoleDiagnostic, false},
	} {
		v, err := results.ValidateV3(res, a.Context, ev(c.prov, c.role), nil)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if trusted := v.OutputViolation == nil; trusted != c.trust {
			t.Errorf("%s: trusted = %v, want %v (%v)", name, trusted, c.trust, v.OutputViolation)
		}
	}
	// An artifact of another attempt, an unfinished one and a missing one keep the
	// order of the earlier wires' answers.
	foreign := ev(results.Provenance{}, runtimev1.RoleOutput)
	e := foreign["art_0001"]
	e.Attempt = "att_other"
	foreign["art_0001"] = e
	if _, err := results.ValidateV3(res, a.Context, foreign, nil); !errors.Is(err, results.ErrNotFound) {
		t.Fatalf("foreign artifact: %v", err)
	}
	pending := ev(results.Provenance{}, runtimev1.RoleOutput)
	e = pending["art_0001"]
	e.Usable = false
	pending["art_0001"] = e
	if _, err := results.ValidateV3(res, a.Context, pending, nil); !errors.Is(err, results.ErrArtifactIncomplete) {
		t.Fatalf("unfinished artifact: %v", err)
	}
	if ids := results.ReferencedArtifactsV3(res, a.Context); len(ids) != 1 || ids[0] != "art_0001" {
		t.Fatalf("referenced: %v", ids)
	}
	// A hash-less or malformed result is refused as the contract says.
	bad := res
	bad.Outcome = "exploded"
	if _, err := results.ValidateV3(bad, a.Context, nil, nil); !errors.Is(err, results.ErrInvalidRequest) {
		t.Fatalf("malformed: %v", err)
	}
}
