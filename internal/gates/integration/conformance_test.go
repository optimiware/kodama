package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const conformanceOutput = "outputs: {conformance: {type: artifact, path: /kodama/outputs/conformance.json}}"

// agentFlow is implement(agent writer) -> review(gate) -> deploy.
type agentFlow struct {
	declare bool
	// report renders the report artifact from the pinned baseline, the
	// published candidate and the supporting artifacts the attempt produced.
	report func(base runtimev1.DesignBaseline, candidate string, support []domain.ArtifactID) []byte
	// supports are the supporting artifacts the attempt keeps.
	supports int
	// publish is the commit the writer publishes.
	publish string
}

var requiredChecks = []string{"module-boundaries", "tenant-authority"}

func passedChecks(support []domain.ArtifactID) []runtimev1.ConformanceCheck {
	var out []runtimev1.ConformanceCheck
	for i, id := range requiredChecks {
		var ev []domain.ArtifactID
		if i < len(support) {
			ev = []domain.ArtifactID{support[i]}
		}
		if len(ev) == 0 {
			ev = []domain.ArtifactID{"art_none"}
		}
		out = append(out, runtimev1.ConformanceCheck{ID: id, Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: ev})
	}
	return out
}

func goodReport(base runtimev1.DesignBaseline, candidate string, support []domain.ArtifactID) []byte {
	b, _ := json.Marshal(gatestest.Report(base, candidate, passedChecks(support)...))
	return b
}

// runAgent runs the agent step to completion and returns the run with its
// requested gate.
func runAgent(t *testing.T, e *gatestest.Env, f agentFlow) (*execution.Run, *gates.Gate) {
	t.Helper()
	run := startAgent(t, e, f)
	if rep := reconcile(t, e, run.Scope.TenantID); rep.Requested != 1 {
		t.Fatalf("the gate of an agent candidate was not requested: %+v", rep)
	}
	return run, gateOf(t, e, run, "review")
}

// startAgent runs the agent step to completion without requesting the gate.
func startAgent(t *testing.T, e *gatestest.Env, f agentFlow) *execution.Run {
	t.Helper()
	p := e.X.NewProject(gatestest.ScopeA)
	extra := []string{}
	if f.declare {
		extra = append(extra, conformanceOutput)
	}
	def := e.Define(p.Scope, gatestest.Agent("implement", extra...), gatestest.Gate("review", []string{"implement"}), pipelinetest.Container("deploy", "needs: [review]", readFrom("implement")))
	run := e.MustAdmit(p, def, nil)
	cancelAtEnd(t, e, run)
	e.X.Sources.History[gatestest.CommitB] = true
	e.X.Sources.History[gatestest.CommitC] = true
	in := e.MustClaim(p.Scope.TenantID)
	if in.StepID != "implement" {
		t.Fatalf("claimed %s", in.StepID)
	}
	var support []domain.ArtifactID
	for i := 0; i < f.supports; i++ {
		support = append(support, e.Artifact(in, "notes-"+string(rune('a'+i)), []byte("review notes "+string(rune('a'+i))), artifacts.RoleOutput))
	}
	publish := f.publish
	if publish == "" {
		publish = gatestest.CommitB
	}
	var outs []gatestest.Output
	if f.declare {
		var data []byte
		if f.report != nil {
			e.Mu().Lock()
			base := e.Baseline()
			e.Mu().Unlock()
			var b runtimev1.DesignBaseline
			if base != nil {
				b = *base
			}
			data = f.report(b, publish, support)
		}
		outs = append(outs, gatestest.Output{Name: "conformance", Data: data})
	}
	e.Finish(in, publish, outs...)
	return run
}

func conformanceCode(t *testing.T, err error) string {
	t.Helper()
	var ce *gates.ConformanceError
	if !errors.As(err, &ce) {
		t.Fatalf("not a conformance denial: %v", err)
	}
	return ce.Code
}

// TestConformanceRequired (DQ-14, SD-08, DQ-A10): approval and progression need
// verified conformance evidence for an agent-produced candidate. Missing report,
// missing baseline and missing required-check coverage block them; a verified,
// applicable report and its supporting artifacts, bound by ID and digest into
// the subject, permit ordinary human review and never automatic approval.
func TestConformanceRequired(t *testing.T) {
	e := gatestest.New(t, gatestest.Options{S3: true})
	e.GrantAll()
	ctx := context.Background()
	base := gatestest.NewBaseline(t, requiredChecks...)

	blocked := func(t *testing.T, g *gates.Gate, run *execution.Run, wantCode string) {
		t.Helper()
		if g.Required.Conformance == nil || g.Required.Conformance.Status != gates.ConformanceBlocked || g.Required.Conformance.Code != wantCode {
			t.Fatalf("snapshot %+v, want blocked %s", g.Required.Conformance, wantCode)
		}
		_, err := approve(t, e, g, gatestest.Reviewer)
		if got := conformanceCode(t, err); got != wantCode {
			t.Fatalf("approval denial %s, want %s", got, wantCode)
		}
		cur := gateByID(t, e, g)
		if cur.Status != gates.StatusPending || cur.Decision != nil || cur.Version != g.Version {
			t.Fatalf("a refused approval changed the gate: %+v", cur)
		}
		if _, err := e.Claim(run.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("progression without applicable evidence: %v", err)
		}
		if got := stepStatus(t, e, run, "review"); got != execution.StepPending {
			t.Fatalf("gate step %s", got)
		}
	}

	t.Run("a missing report blocks", func(t *testing.T) {
		e.SetBaseline(&base)
		run, g := runAgent(t, e, agentFlow{declare: false})
		blocked(t, g, run, gates.CodeConformanceMissing)
		// The reviewer can still reject, preserving the evidence.
		if got, err := reject(t, e, g, gatestest.Reviewer); err != nil || got.Status != gates.StatusRejected {
			t.Fatalf("rejection of blocked evidence: %v", err)
		}
	})
	t.Run("no gate exists before the producer has finished", func(t *testing.T) {
		e.SetBaseline(&base)
		// The producer started but has not delivered: nothing is ready, so no
		// gate (and no report check) exists yet.
		p := e.X.NewProject(gatestest.ScopeA)
		def := e.Define(p.Scope, gatestest.Agent("implement", conformanceOutput), gatestest.Gate("review", []string{"implement"}))
		run := e.MustAdmit(p, def, nil)
		cancelAtEnd(t, e, run)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		if rep := reconcile(t, e, p.Scope.TenantID); rep.Requested != 0 {
			t.Fatalf("a gate for a producer that has not finished: %+v", rep)
		}
	})
	t.Run("a missing baseline blocks", func(t *testing.T) {
		e.SetBaseline(nil)
		run, g := runAgent(t, e, agentFlow{declare: true, supports: 2, report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			return goodReport(base, c, s)
		}})
		blocked(t, g, run, gates.CodeConformanceBaselineMissing)
	})
	t.Run("missing required-check coverage blocks", func(t *testing.T) {
		e.SetBaseline(&base)
		run, g := runAgent(t, e, agentFlow{declare: true, supports: 2, report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			// The agent reports only a subset of the pinned required checks.
			r := gatestest.Report(b, c, runtimev1.ConformanceCheck{ID: "module-boundaries", Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: []domain.ArtifactID{s[0]}})
			out, _ := json.Marshal(r)
			return out
		}})
		blocked(t, g, run, string(runtimev1.CodeConformanceIncomplete))
	})
	t.Run("a verified applicable report permits human review only", func(t *testing.T) {
		e.SetBaseline(&base)
		run, g := runAgent(t, e, agentFlow{declare: true, supports: 2, report: goodReport})
		c := g.Required.Conformance
		if c == nil || c.Status != gates.ConformanceVerified || c.Code != "" || c.BaselineDigest != string(base.Digest) || len(c.RequiredChecks) != 2 {
			t.Fatalf("snapshot %+v", c)
		}
		// The report and its supporting artifacts are bound by ID and digest.
		roles := map[gates.EvidenceRole]int{}
		bound := map[domain.ArtifactID]string{}
		for _, r := range g.Required.Evidence {
			roles[r.Role]++
			bound[r.ID] = r.Digest
		}
		if roles[gates.RoleReport] != 1 || roles[gates.RoleSupport] != 2 {
			t.Fatalf("evidence roles %v", roles)
		}
		for _, ref := range g.Subject.Evidence() {
			if bound[ref.ID] != ref.Digest {
				t.Fatalf("subject evidence %s is not the snapshot's", ref.ID)
			}
		}
		if c.ReportID == "" || bound[c.ReportID] != c.ReportDigest {
			t.Fatalf("the report is not bound into the subject: %+v", c)
		}
		// Verified evidence is not an approval: the gate waits for a human.
		if g.Status != gates.StatusPending {
			t.Fatalf("status %s", g.Status)
		}
		if rep := reconcile(t, e, run.Scope.TenantID); rep.Completed != 0 {
			t.Fatalf("progression without a human decision: %+v", rep)
		}
		if _, err := e.Claim(run.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("claim before the decision: %v", err)
		}
		// The evidence is pinned for the gate.
		var pins int
		e.X.Scalar(&pins, `SELECT count(*) FROM artifact_pins WHERE owner_kind = 'gate' AND owner_id = $1`, string(g.ID))
		if pins != 3 {
			t.Fatalf("%d pins for the gate's evidence", pins)
		}
		if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		if rep := reconcile(t, e, run.Scope.TenantID); rep.Completed != 1 {
			t.Fatalf("after the human approval: %+v", rep)
		}
		if in := e.MustClaim(run.Scope.TenantID); in.StepID != "deploy" {
			t.Fatalf("claimed %s", in.StepID)
		}
		_ = ctx
	})
	t.Run("a non-agent candidate needs no report", func(t *testing.T) {
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		reconcile(t, e, p.Scope.TenantID)
		g := gateOf(t, e, run, "review")
		if g.Required.Conformance != nil {
			t.Fatalf("conformance required for a human-or-original candidate: %+v", g.Required.Conformance)
		}
		if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
	})
}

// TestConformanceStaleOrBlocked (DQ-14, SD-08, SD-A06, DQ-A10): a report for
// the wrong candidate or baseline, a malformed or forged one, failed or
// not-run checks, unresolved deviations and foreign supporting evidence all
// block approval and progression; a new pinned baseline or candidate needs
// fresh evidence.
func TestConformanceStaleOrBlocked(t *testing.T) {
	e := gatestest.New(t, gatestest.Options{S3: true})
	e.GrantAll()
	base := gatestest.NewBaseline(t, requiredChecks...)
	otherBase, err := runtimev1.NewDesignBaseline([]string{"SD-02"}, []string{"C-16"},
		[]runtimev1.SourceRef{{Repository: "https://git.example.test/acme/design.git", Revision: gatestest.CommitA, Path: "docs/specs/software-design.md"}}, requiredChecks)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		code   string
		report func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte
	}{
		{"wrong candidate", string(runtimev1.CodeConformanceCandidateMismatch), func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			return goodReport(b, gatestest.CommitC, s)
		}},
		{"wrong baseline", string(runtimev1.CodeConformanceBaselineMismatch), func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			return goodReport(otherBase, c, s)
		}},
		{"malformed schema", gates.CodeConformanceMalformed, func(runtimev1.DesignBaseline, string, []domain.ArtifactID) []byte {
			return []byte(`{"disposition":"conforms"}`)
		}},
		{"not json", gates.CodeConformanceMalformed, func(runtimev1.DesignBaseline, string, []domain.ArtifactID) []byte {
			return []byte("conforms, trust me")
		}},
		{"failed check", string(runtimev1.CodeConformanceBlocked), func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			cs := passedChecks(s)
			cs[1] = runtimev1.ConformanceCheck{ID: cs[1].ID, Outcome: runtimev1.CheckFailed, EvidenceArtifactIDs: []domain.ArtifactID{}}
			out, _ := json.Marshal(gatestest.Report(b, c, cs...))
			return out
		}},
		{"not run check", string(runtimev1.CodeConformanceBlocked), func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			cs := passedChecks(s)
			cs[0] = runtimev1.ConformanceCheck{ID: cs[0].ID, Outcome: runtimev1.CheckNotRun, EvidenceArtifactIDs: []domain.ArtifactID{}}
			out, _ := json.Marshal(gatestest.Report(b, c, cs...))
			return out
		}},
		{"unresolved deviation", string(runtimev1.CodeConformanceBlocked), func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			r := gatestest.Report(b, c, passedChecks(s)...)
			r.UnresolvedDeviations = []runtimev1.Deviation{{Requirement: "SD-04", Reason: "persistence code lives in the domain package"}}
			r.Disposition = runtimev1.DispBlocked
			out, _ := json.Marshal(r)
			return out
		}},
		{"forged disposition", gates.CodeConformanceMalformed, func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			cs := passedChecks(s)
			cs[1] = runtimev1.ConformanceCheck{ID: cs[1].ID, Outcome: runtimev1.CheckFailed, EvidenceArtifactIDs: []domain.ArtifactID{}}
			r := gatestest.Report(b, c, cs...)
			r.Disposition = runtimev1.Conforms // the agent claims success its checks contradict
			out, _ := json.Marshal(r)
			return out
		}},
		{"forged deviation-free disposition", gates.CodeConformanceMalformed, func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			r := gatestest.Report(b, c, passedChecks(s)...)
			r.UnresolvedDeviations = []runtimev1.Deviation{{Requirement: "SD-04", Reason: "hidden"}}
			out, _ := json.Marshal(r) // still says conforms
			return out
		}},
		{"foreign supporting evidence", gates.CodeConformanceEvidenceInvalid, func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			return goodReport(b, c, []domain.ArtifactID{"art_not_in_this_project", s[1]})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.SetBaseline(&base)
			run, g := runAgent(t, e, agentFlow{declare: true, supports: 2, report: tc.report})
			if c := g.Required.Conformance; c == nil || c.Status != gates.ConformanceBlocked || c.Code != tc.code {
				t.Fatalf("snapshot %+v, want blocked %s", c, tc.code)
			}
			_, err := approve(t, e, g, gatestest.Reviewer)
			if got := conformanceCode(t, err); got != tc.code {
				t.Fatalf("approval denial %s, want %s", got, tc.code)
			}
			if _, err := e.Claim(run.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("progression with %s evidence: %v", tc.name, err)
			}
			if cur := gateByID(t, e, g); cur.Status != gates.StatusPending || cur.Decision != nil {
				t.Fatalf("gate %+v", cur)
			}
		})
	}

	t.Run("supporting evidence of another run", func(t *testing.T) {
		e.SetBaseline(&base)
		// An artifact of a different run in the same project.
		p2 := e.X.NewProject(gatestest.ScopeA)
		def2 := e.Define(p2.Scope, pipelinetest.Container("other", readRun))
		run2 := e.MustAdmit(p2, def2, nil)
		cancelAtEnd(t, e, run2)
		in2 := e.MustClaim(p2.Scope.TenantID)
		foreign := e.Artifact(in2, "foreign", []byte("evidence of another run"), artifacts.RoleOutput)
		e.Start(in2)
		run, g := runAgent(t, e, agentFlow{declare: true, supports: 1, report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			return goodReport(b, c, []domain.ArtifactID{s[0], foreign})
		}})
		_ = run
		if c := g.Required.Conformance; c == nil || c.Status != gates.ConformanceBlocked || c.Code != gates.CodeConformanceEvidenceInvalid {
			t.Fatalf("snapshot %+v", c)
		}
		for _, r := range g.Required.Evidence {
			if r.ID == foreign {
				t.Fatalf("foreign evidence was bound into the subject")
			}
		}
	})

	t.Run("a new pinned baseline needs fresh evidence", func(t *testing.T) {
		// The earlier report cannot be reused under a new baseline: a new run
		// pins the new baseline, and a report that still names the old digest
		// is stale.
		e.SetBaseline(&otherBase)
		_, g := runAgent(t, e, agentFlow{declare: true, supports: 2, report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
			return goodReport(base, c, s) // the old baseline's report
		}})
		if c := g.Required.Conformance; c == nil || c.Code != string(runtimev1.CodeConformanceBaselineMismatch) {
			t.Fatalf("snapshot %+v", c)
		}
		// Fresh evidence assessed against the new baseline is accepted.
		_, fresh := runAgent(t, e, agentFlow{declare: true, supports: 2, report: goodReport})
		if c := fresh.Required.Conformance; c == nil || c.Status != gates.ConformanceVerified {
			t.Fatalf("fresh evidence snapshot %+v", c)
		}
		if _, err := approve(t, e, fresh, gatestest.Reviewer); err != nil {
			t.Fatalf("approving fresh evidence: %v", err)
		}
	})
}

// substitutingEvidence answers a description request for one artifact with the
// description of another, as a confused or hostile evidence store would.
type substitutingEvidence struct {
	gates.Evidence
	mu       sync.Mutex
	redirect map[domain.ArtifactID]domain.ArtifactID
}

func (s *substitutingEvidence) set(from, to domain.ArtifactID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirect = map[domain.ArtifactID]domain.ArtifactID{from: to}
}

func (s *substitutingEvidence) Describe(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (gates.EvidenceMeta, error) {
	s.mu.Lock()
	to, ok := s.redirect[id]
	s.mu.Unlock()
	if ok {
		id = to
	}
	return s.Evidence.Describe(ctx, scope, id)
}

// TestConformanceEvidenceIdentitySubstitution (DQ-14, SD-08): conformance
// evidence is bound by the identity that was asked for. A store that answers
// for the report or a cited supporting artifact with another artifact's
// otherwise valid description blocks approval, and the substituted artifact is
// never bound into the subject or pinned as if it were the requested evidence.
func TestConformanceEvidenceIdentitySubstitution(t *testing.T) {
	var sub *substitutingEvidence
	e := gatestest.New(t, gatestest.Options{S3: true, WrapEvidence: func(inner gates.Evidence) gates.Evidence {
		sub = &substitutingEvidence{Evidence: inner}
		return sub
	}})
	e.GrantAll()
	base := gatestest.NewBaseline(t, requiredChecks...)

	artifactOf := func(run *execution.Run, name string) domain.ArtifactID {
		var id string
		e.X.Scalar(&id, `SELECT id FROM artifacts WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND name = $4`,
			string(run.Scope.TenantID), string(run.Scope.ProjectID), string(run.ID), name)
		if id == "" {
			t.Fatalf("no artifact %s in run %s", name, run.ID)
		}
		return domain.ArtifactID(id)
	}
	// notes-c is produced by the attempt but cited by no check.
	flow := agentFlow{declare: true, supports: 3, report: goodReport}

	for name, tc := range map[string]struct {
		from, to string
	}{
		"the report is answered with a supporting artifact":     {"conformance", "notes-c"},
		"a cited artifact is answered with an uncited artifact": {"notes-a", "notes-c"},
		"a cited artifact is answered with the report":          {"notes-b", "conformance"},
	} {
		t.Run(name, func(t *testing.T) {
			e.SetBaseline(&base)
			run := startAgent(t, e, flow)
			from, to := artifactOf(run, tc.from), artifactOf(run, tc.to)
			sub.set(from, to)
			defer sub.set("", "")
			if rep := reconcile(t, e, run.Scope.TenantID); rep.Requested != 1 {
				t.Fatalf("the gate was not requested: %+v", rep)
			}
			g := gateOf(t, e, run, "review")
			c := g.Required.Conformance
			if c == nil || c.Status != gates.ConformanceBlocked || c.Code != gates.CodeConformanceEvidenceInvalid {
				t.Fatalf("snapshot %+v, want blocked %s", c, gates.CodeConformanceEvidenceInvalid)
			}
			bound := map[domain.ArtifactID]bool{}
			for _, r := range g.Required.Evidence {
				bound[r.ID] = true
			}
			for _, ref := range g.Subject.Evidence() {
				bound[ref.ID] = true
			}
			if bound[from] || (tc.to == "notes-c" && bound[to]) {
				t.Fatalf("the substituted evidence was bound: %v (from %s, to %s)", bound, from, to)
			}
			var pinned int
			e.X.Scalar(&pinned, `SELECT count(*) FROM artifact_pins WHERE owner_kind = 'gate' AND owner_id = $1 AND artifact_id = $2`, string(g.ID), string(from))
			if pinned != 0 {
				t.Fatalf("the requested artifact %s was pinned from a substituted answer", from)
			}
			if tc.from == "conformance" {
				e.X.Scalar(&pinned, `SELECT count(*) FROM artifact_pins WHERE owner_kind = 'gate' AND owner_id = $1 AND artifact_id = $2`, string(g.ID), string(to))
				if pinned != 0 {
					t.Fatalf("the substituted artifact %s was pinned as the report", to)
				}
			}
			_, err := approve(t, e, g, gatestest.Reviewer)
			if got := conformanceCode(t, err); got != gates.CodeConformanceEvidenceInvalid {
				t.Fatalf("approval denial %s", got)
			}
			if _, err := e.Claim(run.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("progression over substituted evidence: %v", err)
			}
			if cur := gateByID(t, e, g); cur.Status != gates.StatusPending || cur.Decision != nil {
				t.Fatalf("gate %+v", cur)
			}
		})
	}
}
