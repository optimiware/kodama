package gatescontract

import (
	"encoding/json"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
)

// AgentCommit is the commit the agent writer of an agent case publishes: the
// candidate under review.
const AgentCommit = "2222222222222222222222222222222222222222"

// SourceCommit is the commit the agent worked from (the run's pinned source).
const SourceCommit = "1111111111111111111111111111111111111111"

// RequiredChecks are the checks the contract's baseline requires.
var RequiredChecks = []string{"module-boundaries", "tenant-authority"}

// AgentSpec describes the producing attempt of an agent case.
type AgentSpec struct {
	// Baseline is pinned into the producing attempt's immutable context; nil
	// pins none.
	Baseline *runtimev1.DesignBaseline
	// Declare makes the producer declare the report output and deliver Report.
	Declare bool
	// Supports is the number of supporting artifacts the attempt keeps.
	Supports int
	// Report renders the report artifact from the pinned baseline, the
	// published candidate and the supporting artifacts. nil delivers an empty
	// artifact.
	Report func(base runtimev1.DesignBaseline, candidate string, support []domain.ArtifactID) []byte
}

func mustBaseline(t testing.TB, requirement string) runtimev1.DesignBaseline {
	t.Helper()
	b, err := runtimev1.NewDesignBaseline([]string{requirement, "DQ-14"}, []string{"C-16"},
		[]runtimev1.SourceRef{{Repository: "https://git.example.test/acme/design.git", Revision: SourceCommit, Path: "docs/specs/software-design.md"}}, RequiredChecks)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func passed(support []domain.ArtifactID) []runtimev1.ConformanceCheck {
	var out []runtimev1.ConformanceCheck
	for i, id := range RequiredChecks {
		ev := []domain.ArtifactID{"art_none"}
		if i < len(support) {
			ev = []domain.ArtifactID{support[i]}
		}
		out = append(out, runtimev1.ConformanceCheck{ID: id, Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: ev})
	}
	return out
}

func report(base runtimev1.DesignBaseline, candidate string, checks ...runtimev1.ConformanceCheck) runtimev1.ConformanceReport {
	r := runtimev1.ConformanceReport{
		BaselineDigest: base.Digest, CandidateCommit: runtimev1.GitCommit(candidate), Requirements: []string{"SD-01"},
		Checks: checks, UnresolvedDeviations: []runtimev1.Deviation{}, Disposition: runtimev1.Conforms,
	}
	for _, c := range checks {
		if c.Outcome != runtimev1.CheckPassed {
			r.Disposition = runtimev1.DispBlocked
		}
	}
	return r
}

func render(r runtimev1.ConformanceReport) []byte {
	b, _ := json.Marshal(r)
	return b
}

func good(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
	return render(report(b, c, passed(s)...))
}

func conformanceCode(t *testing.T, err error) string {
	t.Helper()
	var ce *gates.ConformanceError
	if !errors.As(err, &ce) || !errors.Is(err, gates.ErrConformance) {
		t.Fatalf("not a conformance denial: %v", err)
	}
	return ce.Code
}

// conformance (DQ-14, SD-08): approval and progression need verified
// conformance evidence for an agent-produced candidate. A missing baseline or
// report, a report for another candidate or baseline, a malformed or forged
// one, missing required-check coverage, failed or not-run checks, unresolved
// deviations and foreign supporting evidence all leave a gate whose approval
// and progression are refused (rejection stays possible); a verified, applicable
// report permits ordinary human review and never an automatic approval.
func conformance(t *testing.T, open Open) {
	base := mustBaseline(t, "SD-01")
	other := mustBaseline(t, "SD-02")
	noop := func(runtimev1.DesignBaseline, string, []domain.ArtifactID) []byte { return nil }

	blockedCases := []struct {
		name string
		code string
		spec AgentSpec
	}{
		{"a missing report", gates.CodeConformanceMissing, AgentSpec{Baseline: &base, Declare: false}},
		{"a missing baseline", gates.CodeConformanceBaselineMissing, AgentSpec{Declare: true, Supports: 2, Report: good}},
		{"missing required-check coverage", string(runtimev1.CodeConformanceIncomplete), AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
				return render(report(b, c, runtimev1.ConformanceCheck{ID: "module-boundaries", Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: []domain.ArtifactID{s[0]}}))
			}}},
		{"a report for another candidate", string(runtimev1.CodeConformanceCandidateMismatch), AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(b runtimev1.DesignBaseline, _ string, s []domain.ArtifactID) []byte {
				return good(b, SourceCommit, s)
			}}},
		{"a report for another baseline", string(runtimev1.CodeConformanceBaselineMismatch), AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(_ runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte { return good(other, c, s) }}},
		{"a report that violates the schema", gates.CodeConformanceMalformed, AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(runtimev1.DesignBaseline, string, []domain.ArtifactID) []byte {
				return []byte(`{"disposition":"conforms"}`)
			}}},
		{"a report that is not JSON", gates.CodeConformanceMalformed, AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(runtimev1.DesignBaseline, string, []domain.ArtifactID) []byte {
				return []byte("conforms, trust me")
			}}},
		{"an empty report artifact", gates.CodeConformanceMalformed, AgentSpec{Baseline: &base, Declare: true, Supports: 2, Report: noop}},
		{"a failed check", string(runtimev1.CodeConformanceBlocked), AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
				cs := passed(s)
				cs[1] = runtimev1.ConformanceCheck{ID: cs[1].ID, Outcome: runtimev1.CheckFailed, EvidenceArtifactIDs: []domain.ArtifactID{}}
				return render(report(b, c, cs...))
			}}},
		{"a check that was not run", string(runtimev1.CodeConformanceBlocked), AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
				cs := passed(s)
				cs[0] = runtimev1.ConformanceCheck{ID: cs[0].ID, Outcome: runtimev1.CheckNotRun, EvidenceArtifactIDs: []domain.ArtifactID{}}
				return render(report(b, c, cs...))
			}}},
		{"an unresolved material deviation", string(runtimev1.CodeConformanceBlocked), AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
				r := report(b, c, passed(s)...)
				r.UnresolvedDeviations = []runtimev1.Deviation{{Requirement: "SD-04", Reason: "persistence code lives in the domain package"}}
				r.Disposition = runtimev1.DispBlocked
				return render(r)
			}}},
		{"a forged disposition over failed checks", gates.CodeConformanceMalformed, AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
				cs := passed(s)
				cs[1] = runtimev1.ConformanceCheck{ID: cs[1].ID, Outcome: runtimev1.CheckFailed, EvidenceArtifactIDs: []domain.ArtifactID{}}
				r := report(b, c, cs...)
				r.Disposition = runtimev1.Conforms // the agent claims success its checks contradict
				return render(r)
			}}},
		{"a forged disposition over a hidden deviation", gates.CodeConformanceMalformed, AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
				r := report(b, c, passed(s)...)
				r.UnresolvedDeviations = []runtimev1.Deviation{{Requirement: "SD-04", Reason: "hidden"}}
				return render(r) // still says conforms
			}}},
		{"foreign supporting evidence", gates.CodeConformanceEvidenceInvalid, AgentSpec{Baseline: &base, Declare: true, Supports: 2,
			Report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
				return good(b, c, []domain.ArtifactID{"art_not_in_this_project", s[1]})
			}}},
	}
	for _, tc := range blockedCases {
		t.Run(tc.name+" blocks", func(t *testing.T) {
			h := open(t)
			p := h.Principals()
			c := h.NewAgentCase(t, tc.spec)
			g := requested(t, h, c)
			snap := g.Required.Conformance
			if snap == nil || snap.Status != gates.ConformanceBlocked || snap.Code != tc.code {
				t.Fatalf("snapshot %+v, want blocked %s", snap, tc.code)
			}
			_, err := decide(h, p.Reviewer, g, gates.DecisionApprove)
			if got := conformanceCode(t, err); got != tc.code {
				t.Fatalf("approval denial %s, want %s", got, tc.code)
			}
			cur := reload(t, h, g)
			if cur.Status != gates.StatusPending || cur.Decision != nil || cur.Version != g.Version || cur.Invalidation != nil {
				t.Fatalf("a refused approval changed the gate: %+v", cur)
			}
			if ev := h.Events(t, c, g.ID); ev != (Events{Requested: 1}) {
				t.Fatalf("events %+v", ev)
			}
			// Nothing to consume: the step keeps waiting, with no attempt.
			if rep := process(t, h, g); rep.Completed != 0 || rep.Waiting != 1 {
				t.Fatalf("pass over a blocked gate: %+v", rep)
			}
			if h.StepStatus(t, c) != execution.StepPending {
				t.Fatalf("gate step %s", h.StepStatus(t, c))
			}
			noAttempt(t, h, c)
			// The reviewer can still reject, preserving the evidence.
			rej, err := decide(h, p.Reviewer, g, gates.DecisionReject)
			if err != nil || rej.Status != gates.StatusRejected {
				t.Fatalf("rejection of blocked evidence: %v", err)
			}
			if rep := process(t, h, g); rep.Rejected != 1 {
				t.Fatalf("rejection progression: %+v", rep)
			}
		})
	}

	t.Run("a verified applicable report permits human review only", func(t *testing.T) {
		h := open(t)
		p := h.Principals()
		c := h.NewAgentCase(t, AgentSpec{Baseline: &base, Declare: true, Supports: 2, Report: good})
		g := requested(t, h, c)
		snap := g.Required.Conformance
		if snap == nil || snap.Status != gates.ConformanceVerified || snap.Code != "" || snap.BaselineDigest != string(base.Digest) ||
			len(snap.RequiredChecks) != len(RequiredChecks) || snap.ProducerStep != "implement" {
			t.Fatalf("snapshot %+v", snap)
		}
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
		if snap.ReportID == "" || bound[snap.ReportID] != snap.ReportDigest || g.Subject.CandidateCommit() != AgentCommit {
			t.Fatalf("the report is not bound into the subject of the published candidate: %+v", snap)
		}
		if n := h.Pins(t, c, g.ID); n != 3 {
			t.Fatalf("%d pins for the gate's evidence, want 3", n)
		}
		// Verified evidence is not an approval: the gate waits for a human.
		if g.Status != gates.StatusPending {
			t.Fatalf("status %s", g.Status)
		}
		if rep := process(t, h, g); rep.Completed != 0 || rep.Waiting != 1 {
			t.Fatalf("progression without a human decision: %+v", rep)
		}
		got, err := decide(h, p.Reviewer, g, gates.DecisionApprove)
		if err != nil || got.Status != gates.StatusApproved || got.Decision.By.Type != gates.ActorUser {
			t.Fatalf("the human approval: %v", err)
		}
		if rep := process(t, h, g); rep.Completed != 1 {
			t.Fatalf("after the human approval: %+v", rep)
		}
		if h.StepStatus(t, c) != execution.StepSucceeded {
			t.Fatalf("gate step %s", h.StepStatus(t, c))
		}
		noAttempt(t, h, c)
	})
}
