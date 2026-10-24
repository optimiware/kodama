package gates

import (
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

const (
	baselineDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

// verifiedSnapshot is a valid verified conformance snapshot bound to the
// report art_report.
func verifiedSnapshot() *ConformanceSnapshot {
	return &ConformanceSnapshot{
		Status: ConformanceVerified, ProducerStep: "implement", BaselineDigest: baselineDigest,
		RequiredChecks: []string{"module-boundaries", "tenant-authority"}, ReportID: "art_report", ReportDigest: digB,
	}
}

func conformingRequired(c *ConformanceSnapshot) (Subject, Required, error) {
	in := baseInput()
	in.Evidence = append(in.Evidence, EvidenceRef{ID: "art_report", Digest: digB})
	s, err := NewSubject(in)
	if err != nil {
		return Subject{}, Required{}, err
	}
	return s, Required{Evidence: []RequiredEvidence{
		{Role: RoleEvidence, ID: "art_tests", Digest: digA},
		{Role: RoleReport, ID: "art_report", Digest: digB},
	}, Conformance: c}, nil
}

// newConformingGate requests a gate over a verified snapshot altered by mutate.
func newConformingGate(mutate func(c *ConformanceSnapshot)) (*Gate, error) {
	c := verifiedSnapshot()
	if mutate != nil {
		mutate(c)
	}
	s, r, err := conformingRequired(c)
	if err != nil {
		return nil, err
	}
	g, err := Request(RequestInput{ID: "gate_1", Scope: scopeA, RunID: "run_1", StepID: "review", Subject: s, Required: r, Now: t0, Deadline: t0.Add(time.Hour)})
	if err != nil {
		return nil, err
	}
	g.MarkClean()
	return g, nil
}

// TestRequiredSnapshotInvariants (DQ-14, SD-08): a verified conformance
// snapshot identifies its bound report and the expected baseline and checks;
// malformed snapshots can be neither requested nor restored as usable approval
// evidence, while valid verified and blocked records remain supported.
func TestRequiredSnapshotInvariants(t *testing.T) {
	g, err := newConformingGate(nil)
	if err != nil {
		t.Fatalf("a valid verified snapshot: %v", err)
	}
	if err := g.Decide(decide(g, DecisionApprove, 1, t0), factsFor(g), t0); err != nil {
		t.Fatalf("approving the valid snapshot: %v", err)
	}

	// Valid historical shapes stay supported: an empty required-check list and
	// a blocked snapshot that never reached a report.
	if _, err := newConformingGate(func(c *ConformanceSnapshot) { c.RequiredChecks = []string{} }); err != nil {
		t.Errorf("verified without required checks: %v", err)
	}
	s, _ := NewSubject(baseInput())
	blocked := &ConformanceSnapshot{Status: ConformanceBlocked, Code: CodeConformanceMissing, ProducerStep: "implement", RequiredChecks: []string{}}
	if _, err := Request(RequestInput{ID: "gate_1", Scope: scopeA, RunID: "run_1", StepID: "review", Subject: s,
		Required: Required{Evidence: []RequiredEvidence{{Role: RoleEvidence, ID: "art_tests", Digest: digA}}, Conformance: blocked}, Now: t0, Deadline: t0.Add(time.Hour)}); err != nil {
		t.Errorf("blocked snapshot without a report: %v", err)
	}

	for name, mutate := range map[string]func(c *ConformanceSnapshot){
		"no report":                 func(c *ConformanceSnapshot) { c.ReportID, c.ReportDigest = "", "" },
		"report id without digest":  func(c *ConformanceSnapshot) { c.ReportDigest = "" },
		"malformed report digest":   func(c *ConformanceSnapshot) { c.ReportDigest = "sha256:ab" },
		"report not bound":          func(c *ConformanceSnapshot) { c.ReportID = "art_other" },
		"report digest differs":     func(c *ConformanceSnapshot) { c.ReportDigest = digC },
		"no baseline digest":        func(c *ConformanceSnapshot) { c.BaselineDigest = "" },
		"malformed baseline digest": func(c *ConformanceSnapshot) { c.BaselineDigest = "baseline" },
		"missing required checks":   func(c *ConformanceSnapshot) { c.RequiredChecks = nil },
		"unsorted required checks":  func(c *ConformanceSnapshot) { c.RequiredChecks = []string{"b", "a"} },
		"duplicate required checks": func(c *ConformanceSnapshot) { c.RequiredChecks = []string{"a", "a"} },
		"empty required check":      func(c *ConformanceSnapshot) { c.RequiredChecks = []string{""} },
		"no producer step":          func(c *ConformanceSnapshot) { c.ProducerStep = "" },
		"verified with a denial":    func(c *ConformanceSnapshot) { c.Code = string(runtimev1.CodeConformanceBlocked) },
		"unknown status":            func(c *ConformanceSnapshot) { c.Status = "ok" },
		"blocked without a code":    func(c *ConformanceSnapshot) { c.Status, c.Code = ConformanceBlocked, "" },
		"blocked naming unbound doc": func(c *ConformanceSnapshot) {
			c.Status, c.Code, c.ReportID = ConformanceBlocked, string(runtimev1.CodeConformanceBlocked), "art_other"
		},
	} {
		if _, err := newConformingGate(mutate); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: requested (%v)", name, err)
		}
	}

	// A stored row that violates the same invariants is not restored.
	good, _ := newConformingGate(nil)
	rec := Record{ID: good.ID, Scope: good.Scope, RunID: good.RunID, StepID: good.StepID, Subject: good.Subject.Canonical(), Required: good.Required, Status: StatusPending,
		Version: 1, RequestedAt: t0, Deadline: t0.Add(time.Hour), CreatedAt: t0, UpdatedAt: t0}
	if _, err := Restore(rec); err != nil {
		t.Fatalf("restoring the valid record: %v", err)
	}
	for name, mutate := range map[string]func(c *ConformanceSnapshot){
		"report digest cleared": func(c *ConformanceSnapshot) { c.ReportID, c.ReportDigest = "", "" },
		"baseline cleared":      func(c *ConformanceSnapshot) { c.BaselineDigest = "" },
		"checks cleared":        func(c *ConformanceSnapshot) { c.RequiredChecks = nil },
	} {
		c := *good.Required.Conformance
		mutate(&c)
		r := rec
		r.Required.Conformance = &c
		if _, err := Restore(r); err == nil {
			t.Errorf("%s: restored", name)
		}
	}

	// Duplicate (artifact, role) references cannot be requested; distinct roles
	// of one artifact can.
	in := baseInput()
	sub, _ := NewSubject(in)
	dup := Required{Evidence: []RequiredEvidence{{Role: RoleEvidence, ID: "art_tests", Digest: digA, Expression: "a"}, {Role: RoleEvidence, ID: "art_tests", Digest: digA, Expression: "b"}}}
	if _, err := Request(RequestInput{ID: "gate_1", Scope: scopeA, RunID: "run_1", StepID: "review", Subject: sub, Required: dup, Now: t0, Deadline: t0.Add(time.Hour)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicate reference: %v", err)
	}
	twoRoles := Required{Evidence: []RequiredEvidence{{Role: RoleEvidence, ID: "art_tests", Digest: digA}, {Role: RoleSupport, ID: "art_tests", Digest: digA}}}
	if _, err := Request(RequestInput{ID: "gate_1", Scope: scopeA, RunID: "run_1", StepID: "review", Subject: sub, Required: twoRoles, Now: t0, Deadline: t0.Add(time.Hour)}); err != nil {
		t.Errorf("one artifact in two roles: %v", err)
	}
}
