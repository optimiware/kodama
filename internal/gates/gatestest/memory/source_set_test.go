package memory_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest/gatescontract"
	"github.com/optimiware/kodama/internal/gates/gatestest/memory"
)

func setHarness(t *testing.T) *memory.Harness { return memory.NewHarness(t) }

func requestSet(t *testing.T, h *memory.Harness, spec memory.SourceSetSpec) (memory.SourceSetCase, *gates.Gate) {
	t.Helper()
	c := h.NewSourceSetCase(t, spec)
	g, created, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step)
	if err != nil || !created {
		t.Fatalf("request the source-set gate: %v created=%v", err, created)
	}
	return c, g
}

func subjectSet(t *testing.T, g *gates.Gate) gates.SourceSetSubject {
	t.Helper()
	set, ok := g.Subject.SourceSet()
	if !ok {
		t.Fatalf("the subject names no source set: %s", g.Subject.Canonical())
	}
	return set
}

// approve decides the gate as the fixture reviewer and returns the decided gate.
func approve(t *testing.T, h *memory.Harness, g *gates.Gate) *gates.Gate {
	t.Helper()
	out, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return out
}

// Test_unit_gate_subject_source_set_and_base (DQ-17, GR-12, DQ-A13): the subject
// of a gate over several repositories names the run's source set digest, the
// commit under review of every repository and, for the repository the run may
// write, the integration base. It is canonical and immutable, evidence must be
// produced from exactly that set, and a source set with a writable repository
// is never reviewed without an observed base.
func Test_unit_gate_subject_source_set_and_base(t *testing.T) {
	h := setHarness(t)
	c, g := requestSet(t, h, memory.SourceSetSpec{})

	t.Run("the subject names the set, every repository's commit and the writable base", func(t *testing.T) {
		set := subjectSet(t, g)
		if set.Digest != c.Digest || len(set.Sources) != 2 {
			t.Fatalf("subject set: %+v", set)
		}
		api, web := set.Sources[0], set.Sources[1]
		if api.RepositoryID != memory.RepoAPI || web.RepositoryID != memory.RepoWeb {
			t.Fatalf("sources are ordered by repository: %+v", set.Sources)
		}
		if api.Commit != memory.APINew || web.Commit != memory.WebPinned {
			t.Fatalf("the candidate is the writer's commit and the pinned commit: %+v", set.Sources)
		}
		if api.Base == nil || api.Base.Ref != memory.DefaultRef || api.Base.Commit != memory.APIBase || web.Base != nil {
			t.Fatalf("only the writable repository has an integration base: %+v %+v", api.Base, web.Base)
		}
		if g.Subject.CandidateCommit() != "" {
			t.Fatalf("a source-set subject has no single candidate commit: %q", g.Subject.CandidateCommit())
		}
	})

	t.Run("the canonical subject is closed, restorable and has no candidateCommit", func(t *testing.T) {
		raw := g.Subject.Canonical()
		if bytes.Contains(raw, []byte("candidateCommit")) || !bytes.Contains(raw, []byte(`"sourceSet":{"digest":"`+c.Digest+`"`)) {
			t.Fatalf("canonical subject: %s", raw)
		}
		back, err := gates.ParseSubject(raw)
		if err != nil || back.Digest() != g.Subject.Digest() {
			t.Fatalf("restore: %v", err)
		}
		// The bytes are the only form: reordering a member or adding another fails.
		var doc map[string]json.RawMessage
		_ = json.Unmarshal(raw, &doc)
		doc["candidateCommit"] = json.RawMessage(`"` + memory.APINew + `"`)
		both, _ := json.Marshal(doc)
		if _, err := gates.ParseSubject(both); err == nil {
			t.Fatal("a subject with a commit and a set was restored")
		}
		if _, err := gates.ParseSubject(bytes.Replace(raw, []byte(`"sourceSet"`), []byte(`"sourceSets"`), 1)); err == nil {
			t.Fatal("an unknown member was restored")
		}
	})

	t.Run("every part of the candidate and the base changes the digest", func(t *testing.T) {
		set := subjectSet(t, g)
		scope := g.Subject.Scope()
		build := func(edit func(*gates.SourceSetSubject)) string {
			s := gates.SourceSetSubject{Digest: set.Digest, Sources: append([]gates.SubjectSource(nil), set.Sources...)}
			edit(&s)
			sub, err := gates.NewSubject(gates.SubjectInput{Scope: scope, WorkItemID: g.Subject.WorkItemID(), ContentRevision: g.Subject.ContentRevision(),
				SourceSet: &s, DefinitionID: g.Subject.DefinitionID(), Evidence: g.Subject.Evidence()})
			if err != nil {
				t.Fatal(err)
			}
			return sub.Digest()
		}
		same := build(func(*gates.SourceSetSubject) {})
		if same != g.Subject.Digest() {
			t.Fatalf("rebuilding the subject changed its digest: %s != %s", same, g.Subject.Digest())
		}
		seen := map[string]string{same: "same"}
		for name, edit := range map[string]func(*gates.SourceSetSubject){
			"the api commit": func(s *gates.SourceSetSubject) { s.Sources[0].Commit = memory.MovedTip },
			"the web commit": func(s *gates.SourceSetSubject) { s.Sources[1].Commit = memory.MovedTip },
			"the base": func(s *gates.SourceSetSubject) {
				s.Sources[0].Base = &gates.Base{Ref: memory.DefaultRef, Commit: memory.MovedTip}
			},
			"the base ref": func(s *gates.SourceSetSubject) {
				s.Sources[0].Base = &gates.Base{Ref: "refs/heads/release", Commit: memory.APIBase}
			},
			"a base on the read repository": func(s *gates.SourceSetSubject) {
				s.Sources[1].Base = &gates.Base{Ref: memory.DefaultRef, Commit: memory.WebBase}
			},
			"the set digest": func(s *gates.SourceSetSubject) { s.Digest = "sha256:" + strings.Repeat("0", 64) },
		} {
			d := build(edit)
			if other, dup := seen[d]; dup {
				t.Errorf("%s gives the same digest as %s", name, other)
			}
			seen[d] = name
		}
	})

	t.Run("the input order is not part of the identity and malformed sets are refused", func(t *testing.T) {
		set := subjectSet(t, g)
		rev := []gates.SubjectSource{set.Sources[1], set.Sources[0]}
		again, err := gates.NewSourceSetSubject(set.Digest, rev)
		if err != nil || again.Sources[0].RepositoryID != memory.RepoAPI {
			t.Fatalf("ordering: %v %+v", err, again)
		}
		bad := map[string]func() error{
			"no sources":   func() error { _, err := gates.NewSourceSetSubject(set.Digest, nil); return err },
			"a bad digest": func() error { _, err := gates.NewSourceSetSubject("sha256:x", set.Sources); return err },
			"a repeated repo": func() error {
				_, err := gates.NewSourceSetSubject(set.Digest, []gates.SubjectSource{set.Sources[0], set.Sources[0]})
				return err
			},
			"a branch as commit": func() error {
				_, err := gates.NewSourceSetSubject(set.Digest, []gates.SubjectSource{{RepositoryID: "r", Commit: "main"}})
				return err
			},
			"a tag as base": func() error {
				_, err := gates.NewSourceSetSubject(set.Digest, []gates.SubjectSource{{RepositoryID: "r", Commit: memory.APINew, Base: &gates.Base{Ref: "refs/tags/v1", Commit: memory.APIBase}}})
				return err
			},
			"a hidden ref as base": func() error {
				_, err := gates.NewSourceSetSubject(set.Digest, []gates.SubjectSource{{RepositoryID: "r", Commit: memory.APINew, Base: &gates.Base{Ref: "refs/kodama/runs/x", Commit: memory.APIBase}}})
				return err
			},
		}
		for name, f := range bad {
			if err := f(); !errors.Is(err, gates.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := gates.NewSubject(gates.SubjectInput{Scope: g.Subject.Scope(), WorkItemID: g.Subject.WorkItemID(), ContentRevision: 1, DefinitionID: g.Subject.DefinitionID(),
			SourceSet: &set, CandidateCommit: memory.APINew}); !errors.Is(err, gates.ErrInvalid) {
			t.Errorf("a commit beside a set: %v", err)
		}
		if _, err := gates.NewSubject(gates.SubjectInput{Scope: g.Subject.Scope(), WorkItemID: g.Subject.WorkItemID(), ContentRevision: 1, DefinitionID: g.Subject.DefinitionID()}); !errors.Is(err, gates.ErrInvalid) {
			t.Errorf("neither a commit nor a set: %v", err)
		}
	})

	t.Run("the request is idempotent, pins the evidence and observes the base once per request", func(t *testing.T) {
		again, created, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step)
		if err != nil || created || again.ID != g.ID || again.SubjectDigest() != g.SubjectDigest() {
			t.Fatalf("repeat: %v %v", err, created)
		}
		if n := h.Pins(t, c.Case, g.ID); n != 1 {
			t.Fatalf("pins: %d", n)
		}
		calls := h.E.Bases.Calls()
		if len(calls) == 0 || calls[0] != memory.RepoAPI {
			t.Fatalf("only the writable repository's tip is read: %v", calls)
		}
		for _, r := range calls {
			if r == memory.RepoWeb {
				t.Fatalf("a read repository has no integration base: %v", calls)
			}
		}
	})

	t.Run("evidence must be produced from exactly the candidate's source set", func(t *testing.T) {
		for name, spec := range map[string]memory.SourceSetSpec{
			"another api commit": {EvidenceFrom: map[string]string{memory.RepoAPI: memory.MovedTip, memory.RepoWeb: memory.WebPinned}},
			"another web commit": {EvidenceFrom: map[string]string{memory.RepoAPI: memory.APINew, memory.RepoWeb: memory.MovedTip}},
			"one repository":     {EvidenceFrom: map[string]string{memory.RepoAPI: memory.APINew}},
			"an extra repo":      {EvidenceFrom: map[string]string{memory.RepoAPI: memory.APINew, memory.RepoWeb: memory.WebPinned, "repo_other": memory.APINew}},
			"a single source":    {EvidenceSingle: true},
			"no provenance":      {NoEvidenceProvenance: true},
		} {
			c := h.NewSourceSetCase(t, spec)
			if _, _, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrEvidence) {
				t.Errorf("%s: %v", name, err)
			}
			if _, err := h.E.Store.ByStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotFound) {
				t.Errorf("%s: a gate exists for unverifiable evidence: %v", name, err)
			}
		}
	})

	t.Run("evidence of the pinned set before the writer published is the writer exception", func(t *testing.T) {
		c := h.NewSourceSetCase(t, memory.SourceSetSpec{EvidenceFrom: map[string]string{memory.RepoAPI: memory.APIPinned, memory.RepoWeb: memory.WebPinned}})
		// The artifact was produced by the "test" step, not by the candidate's
		// producer: it is a different commit than the candidate, and the producer
		// exception does not apply to it.
		if _, _, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrEvidence) {
			t.Fatalf("evidence from before the candidate's own writer: %v", err)
		}
	})

	t.Run("a writable set needs an observed base: no port, an outage or an unknown repository create no gate", func(t *testing.T) {
		for name, mut := range map[string]struct {
			arrange func(*memory.Harness)
			want    error
		}{
			"an outage":  {func(h *memory.Harness) { h.E.Bases.Fail(errors.New("git down")) }, gates.ErrUnavailable},
			"an unknown": {func(h *memory.Harness) { h.E.Bases.Remove(memory.RepoAPI) }, gates.ErrEvidence},
		} {
			h2 := setHarness(t)
			c := h2.NewSourceSetCase(t, memory.SourceSetSpec{})
			mut.arrange(h2)
			if _, _, err := h2.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, mut.want) {
				t.Errorf("%s: %v", name, err)
			}
			if _, err := h2.E.Store.ByStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotFound) {
				t.Errorf("%s: a gate was created without a base: %v", name, err)
			}
		}
		h3 := setHarness(t)
		deps := h3.E.Dependencies()
		deps.Bases = nil
		svc, err := gates.NewService(deps, h3.E.ServiceOptions())
		if err != nil {
			t.Fatal(err)
		}
		c := h3.NewSourceSetCase(t, memory.SourceSetSpec{})
		if _, _, err := svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotConfigured) {
			t.Fatalf("no base reader: %v", err)
		}
	})

	t.Run("a read-only source set has no base and needs no base reader", func(t *testing.T) {
		h4 := setHarness(t)
		deps := h4.E.Dependencies()
		deps.Bases = nil
		svc, err := gates.NewService(deps, h4.E.ServiceOptions())
		if err != nil {
			t.Fatal(err)
		}
		c := h4.NewSourceSetCase(t, memory.SourceSetSpec{ReadOnly: true})
		g, _, err := svc.RequestForStep(bg, c.Scope, c.Run, c.Step)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		set := subjectSet(t, g)
		if set.Sources[0].Commit != memory.APIPinned || set.Sources[0].Base != nil || set.Sources[1].Base != nil {
			t.Fatalf("a reader's candidate is the pinned set, without bases: %+v", set.Sources)
		}
	})

	t.Run("a deployment must record the same source set", func(t *testing.T) {
		c := h.NewSourceSetCase(t, memory.SourceSetSpec{Deployment: true})
		dg, _, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step)
		if err != nil {
			t.Fatalf("a deployment of the same set: %v", err)
		}
		if id, _ := dg.Subject.Deployment(); id != c.Deployment {
			t.Fatalf("the deployment is part of the subject: %q", id)
		}
		// Another candidate commit in the deployment record is refused.
		moved := h.NewSourceSetCase(t, memory.SourceSetSpec{Deployment: true})
		wrong, err := gates.NewSourceSetRef(moved.Digest, []gates.SourceCommit{{RepositoryID: memory.RepoAPI, Commit: memory.MovedTip}, {RepositoryID: memory.RepoWeb, Commit: memory.WebPinned}})
		must(t, err)
		must(t, h.E.Deployments.SetSourceSet(bg, moved.Scope, moved.Deployment, wrong, digest("config-1")))
		if _, _, err := h.E.Svc.RequestForStep(bg, moved.Scope, moved.Run, moved.Step); !errors.Is(err, gates.ErrEvidence) {
			t.Fatalf("a deployment of another commit: %v", err)
		}
		// A single-commit deployment cannot stand for a set.
		single := h.NewSourceSetCase(t, memory.SourceSetSpec{Deployment: true})
		must(t, h.E.Deployments.Set(bg, single.Scope, single.Deployment, memory.APINew, digest("config-1")))
		if _, _, err := h.E.Svc.RequestForStep(bg, single.Scope, single.Run, single.Step); !errors.Is(err, gates.ErrEvidence) {
			t.Fatalf("a single-commit deployment for a set: %v", err)
		}
		// And a set cannot stand for a single-commit gate.
		sg := h.NewCase(t, true)
		must(t, h.E.Deployments.SetSourceSet(bg, sg.Scope, sg.Deployment, c.Set, digest("config-1")))
		if _, _, err := h.E.Svc.RequestForStep(bg, sg.Scope, sg.Run, sg.Step); !errors.Is(err, gates.ErrEvidence) {
			t.Fatalf("a set deployment for a single-commit gate: %v", err)
		}
	})

	t.Run("a changed candidate after the request is stale and the approval is never consumed", func(t *testing.T) {
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		h.PublishToTask(t, c, memory.RepoAPI, memory.MovedTip)
		rep := process(t, h, g)
		cur := current(t, h, g)
		if rep.Completed != 0 || cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonCandidateChanged || h.StepStatus(t, c.Case) != execution.StepPending {
			t.Fatalf("a changed set progressed: %+v %+v step %s", rep, cur.Invalidation, h.StepStatus(t, c.Case))
		}
	})
}

// Test_unit_gate_moved_base_not_applicable (GR-12, DQ-04, DQ-05, DQ-A13): an
// approval is given over a base. If a repository's default branch moved, the
// subject is no longer applicable: it is invalidated once, takes no decision,
// never progresses its step, and yields no integration approval. A base that
// moves back never resurrects it.
func Test_unit_gate_moved_base_not_applicable(t *testing.T) {
	t.Run("a pending gate whose base moved takes no decision and is invalidated once", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.MovedTip)
		_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
		se := wantStale(t, err, gates.ReasonBaseMoved, true)
		if se.Reason != "base_moved" {
			t.Fatalf("reason: %v", se)
		}
		cur := current(t, h, g)
		if cur.Status != gates.StatusPending || cur.Decision != nil || cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonBaseMoved {
			t.Fatalf("the refused decision left a decision or no invalidation: %+v", cur)
		}
		if ev := h.Events(t, c.Case, g.ID); ev.Invalidated != 1 || ev.Decided != 0 {
			t.Fatalf("events: %+v", ev)
		}
		// A repeat of the request, or the base moving back, records nothing more.
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.APIBase)
		if _, err := decide(h, memory.Reviewer, current(t, h, g), gates.DecisionApprove); !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("an invalidated gate decided: %v", err)
		}
		if ev := h.Events(t, c.Case, g.ID); ev.Invalidated != 1 || ev.Decided != 0 {
			t.Fatalf("events after the base moved back: %+v", ev)
		}
		if h.StepStatus(t, c.Case) != execution.StepPending {
			t.Fatal("the step progressed")
		}
	})

	t.Run("an approval whose base moved never progresses its step", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.MovedTip)
		rep := process(t, h, g)
		cur := current(t, h, g)
		if rep.Completed != 0 || rep.Invalidated != 1 || cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonBaseMoved {
			t.Fatalf("process: %+v invalidation %+v", rep, cur.Invalidation)
		}
		if cur.Status != gates.StatusApproved || cur.Decision == nil || cur.Decision.Decision != gates.DecisionApprove {
			t.Fatalf("the historical decision changed: %+v", cur.Decision)
		}
		if h.StepStatus(t, c.Case) != execution.StepPending {
			t.Fatal("the step progressed over a moved base")
		}
		// Moving the base back does not revive the approval.
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.APIBase)
		if rep := process(t, h, g); rep.Completed != 0 || h.StepStatus(t, c.Case) != execution.StepPending {
			t.Fatalf("a revived approval: %+v", rep)
		}
		if err := cur.Applicable(h.E.Clock.Now()); !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("applicable: %v", err)
		}
	})

	t.Run("another branch as the default is a moved base", func(t *testing.T) {
		h := setHarness(t)
		_, g := requestSet(t, h, memory.SourceSetSpec{})
		h.E.Bases.Set(memory.RepoAPI, "refs/heads/trunk", memory.APIBase)
		_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
		wantStale(t, err, gates.ReasonBaseMoved, true)
	})

	t.Run("a base outage denies without invalidating", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		h.E.Bases.Fail(errors.New("git down"))
		if _, err := decide(h, memory.Reviewer, g, gates.DecisionApprove); !errors.Is(err, gates.ErrUnavailable) {
			t.Fatalf("decide during an outage: %v", err)
		}
		if cur := current(t, h, g); cur.Invalidation != nil || cur.Status != gates.StatusPending {
			t.Fatalf("an outage changed the gate: %+v", cur)
		}
		if ev := h.Events(t, c.Case, g.ID); ev.Invalidated != 0 {
			t.Fatalf("an invalidation was fabricated: %+v", ev)
		}
		h.E.Bases.Fail(nil)
		if _, err := decide(h, memory.Reviewer, current(t, h, g), gates.DecisionApprove); err != nil {
			t.Fatalf("decide after the outage: %v", err)
		}
	})

	t.Run("a base that did not move leaves the approval applicable and progresses once", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		rep := process(t, h, g)
		if rep.Completed != 1 || h.StepStatus(t, c.Case) != execution.StepSucceeded {
			t.Fatalf("progress: %+v", rep)
		}
		if again := process(t, h, g); again.Completed != 0 {
			t.Fatalf("a second progression: %+v", again)
		}
	})

	t.Run("a single-source gate has no base and is unaffected by tips", func(t *testing.T) {
		h := setHarness(t)
		c := h.NewCase(t, false)
		g := requested(t, h, c)
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.MovedTip)
		h.E.Bases.Fail(errors.New("git down"))
		g, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
		if err != nil {
			t.Fatalf("a single-source decision read the bases: %v", err)
		}
		if calls := h.E.Bases.Calls(); len(calls) != 0 {
			t.Fatalf("bases were consulted for a single-source gate: %v", calls)
		}
		if rep := process(t, h, g); rep.Completed != 1 {
			t.Fatalf("progress: %+v", rep)
		}
	})
}

func requested(t *testing.T, h *memory.Harness, c gatescontract.Case) *gates.Gate {
	t.Helper()
	g, created, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step)
	if err != nil || !created {
		t.Fatalf("request: %v %v", err, created)
	}
	return g
}

// Test_unit_integration_grant_requires_approved_subject (DQ-17, GR-12, GR-A08):
// the data an integration grant is issued from exists only for a gate that a
// human approved, that is still applicable and that passed its step; only for a
// repository the run may write; only while the default branch is at the approved
// base; and it names exactly the base and the approved commit. Everything else
// yields no approval, so no grant can be issued.
func Test_unit_integration_grant_requires_approved_subject(t *testing.T) {
	ask := func(h *memory.Harness, c memory.SourceSetCase, g *gates.Gate, repo string) (gates.IntegrationApproval, error) {
		return h.E.Svc.IntegrationApproval(bg, c.Scope, c.Run, g.ID, repo)
	}
	noApproval := func(t *testing.T, name string, err error) {
		t.Helper()
		if !errors.Is(err, gates.ErrNoApproval) {
			t.Errorf("%s: %v", name, err)
		}
	}

	t.Run("an approved, passed and unmoved subject names exactly the update", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		got, err := ask(h, c, g, memory.RepoAPI)
		if err != nil {
			t.Fatalf("approval: %v", err)
		}
		want := gates.IntegrationApproval{GateID: g.ID, RunID: c.Run, RepositoryID: memory.RepoAPI, Ref: memory.DefaultRef,
			Base: memory.APIBase, Candidate: memory.APINew, CurrentTip: memory.APIBase}
		if got != want {
			t.Fatalf("approval %+v, want %+v", got, want)
		}
		// Idempotent: the same answer again.
		if again, err := ask(h, c, g, memory.RepoAPI); err != nil || again != want {
			t.Fatalf("repeat: %+v %v", again, err)
		}
	})

	t.Run("no approval for an unapproved, rejected, undecided or unprocessed gate", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		_, err := ask(h, c, g, memory.RepoAPI)
		noApproval(t, "pending", err)
		approved := approve(t, h, g)
		// Approved, but its step has not passed: no grant until progression.
		_, err = ask(h, c, approved, memory.RepoAPI)
		noApproval(t, "approved but not yet progressed", err)
		process(t, h, approved)
		if _, err := ask(h, c, approved, memory.RepoAPI); err != nil {
			t.Fatalf("approved and progressed: %v", err)
		}

		c2, g2 := requestSet(t, h, memory.SourceSetSpec{})
		rejected, err := decide(h, memory.Reviewer, g2, gates.DecisionReject)
		if err != nil {
			t.Fatal(err)
		}
		process(t, h, rejected)
		_, err = ask(h, c2, rejected, memory.RepoAPI)
		noApproval(t, "rejected", err)
	})

	t.Run("no approval for a read repository, an unknown one, another run or a single-source gate", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		_, err := ask(h, c, g, memory.RepoWeb)
		noApproval(t, "a read repository", err)
		_, err = ask(h, c, g, "repo_unknown")
		noApproval(t, "a repository outside the set", err)
		if _, err := h.E.Svc.IntegrationApproval(bg, c.Scope, "run_other", g.ID, memory.RepoAPI); !errors.Is(err, gates.ErrNoApproval) {
			t.Errorf("another run's gate: %v", err)
		}
		if _, err := h.E.Svc.IntegrationApproval(bg, c.Scope, c.Run, "gate_unknown", memory.RepoAPI); !errors.Is(err, gates.ErrNotFound) {
			t.Errorf("an unknown gate: %v", err)
		}
		foreign := domain.Scope{TenantID: memory.ScopeB.TenantID, ProjectID: c.Scope.ProjectID}
		if _, err := h.E.Svc.IntegrationApproval(bg, foreign, c.Run, g.ID, memory.RepoAPI); !errors.Is(err, gates.ErrNotFound) {
			t.Errorf("a foreign tenant's read: %v", err)
		}
		if _, err := h.E.Svc.IntegrationApproval(bg, c.Scope, c.Run, g.ID, "../etc"); !errors.Is(err, gates.ErrNotFound) {
			t.Errorf("a malformed repository: %v", err)
		}
		single := h.NewCase(t, false)
		sg := requested(t, h, single)
		sg, err2 := decide(h, memory.Reviewer, sg, gates.DecisionApprove)
		if err2 != nil {
			t.Fatal(err2)
		}
		process(t, h, sg)
		if _, err := h.E.Svc.IntegrationApproval(bg, single.Scope, single.Run, sg.ID, memory.RepoAPI); !errors.Is(err, gates.ErrNoApproval) {
			t.Errorf("a single-source gate has no integration: %v", err)
		}
	})

	t.Run("an expired approval yields none", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		h.Advance(t, 2*time.Hour)
		_, err := ask(h, c, g, memory.RepoAPI)
		noApproval(t, "expired", err)
		if cur := current(t, h, g); cur.Invalidation != nil {
			t.Fatalf("expiry fabricated an invalidation: %+v", cur.Invalidation)
		}
	})

	t.Run("a moved base is a conflict, not an approval, and the gate is invalidated", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.MovedTip)
		got, err := ask(h, c, g, memory.RepoAPI)
		var se *gates.StaleError
		if !errors.As(err, &se) || !se.Definitive || se.Reason != gates.ReasonBaseMoved || !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("a moved base: %v", err)
		}
		if got != (gates.IntegrationApproval{}) || se.SupersedingCommit != memory.MovedTip {
			t.Fatalf("an error carries no approval; the stale error names the moved tip: %+v %+v", got, se)
		}
		if cur := current(t, h, g); cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonBaseMoved {
			t.Fatalf("not invalidated: %+v", cur.Invalidation)
		}
		// Moving back does not revive it, and asking again stays a conflict.
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.APIBase)
		if _, err := ask(h, c, g, memory.RepoAPI); !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("an invalidated approval after the base moved back: %v", err)
		}
	})

	t.Run("a push applied after the step passed is not a moved base: no invalidation, and a replay is already integrated", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		if _, err := ask(h, c, g, memory.RepoAPI); err != nil {
			t.Fatalf("before the push: %v", err)
		}
		// The integration push applies: the default branch is now the candidate.
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.APINew)
		rep := process(t, h, g)
		cur := current(t, h, g)
		if rep.Invalidated != 0 || cur.Invalidation != nil {
			t.Fatalf("a successful integration invalidated the approval: %+v %+v", rep, cur.Invalidation)
		}
		if rep, err := h.E.Svc.Reconcile(bg, c.Scope.TenantID); err != nil || rep.Invalidated != 0 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		if ev := h.Events(t, c.Case, g.ID); ev.Invalidated != 0 {
			t.Fatalf("a spurious gate.invalidated event: %+v", ev)
		}
		// A replay after a lost push result is answered "already integrated", with
		// the facts and without an approval value, then and after the gate expired.
		for _, when := range []string{"now", "after the gate expired"} {
			if when != "now" {
				h.Advance(t, 2*time.Hour)
			}
			got, err := ask(h, c, g, memory.RepoAPI)
			var done *gates.AlreadyIntegratedError
			if !errors.Is(err, gates.ErrAlreadyIntegrated) || !errors.As(err, &done) {
				t.Fatalf("%s: %v", when, err)
			}
			if got != (gates.IntegrationApproval{}) || done.Approval.Candidate != memory.APINew || done.Approval.Base != memory.APIBase || done.Approval.CurrentTip != memory.APINew ||
				done.Approval.Ref != memory.DefaultRef || done.Approval.RepositoryID != memory.RepoAPI {
				t.Fatalf("%s: %+v %+v", when, got, done.Approval)
			}
		}
		if cur := current(t, h, g); cur.Invalidation != nil {
			t.Fatalf("replays invalidated: %+v", cur.Invalidation)
		}
	})

	t.Run("a branch that moved to something else after the step passed is a conflict and invalidates once", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.MovedTip)
		// Reconciliation leaves the consumed approval alone; the query is what
		// refuses to issue over a base that is neither the base nor the candidate.
		if rep := process(t, h, g); rep.Invalidated != 0 {
			t.Fatalf("reconcile invalidated a consumed approval: %+v", rep)
		}
		if _, err := ask(h, c, g, memory.RepoAPI); !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("a base moved to another commit: %v", err)
		}
		if ev := h.Events(t, c.Case, g.ID); ev.Invalidated != 1 {
			t.Fatalf("events: %+v", ev)
		}
	})

	t.Run("the deciding reviewer must still hold the reviewer authority", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		if _, err := ask(h, c, g, memory.RepoAPI); err != nil {
			t.Fatal(err)
		}
		h.Revoke(t, memory.Reviewer)
		_, err := ask(h, c, g, memory.RepoAPI)
		noApproval(t, "a revoked reviewer", err)
		// Even after the push, a revoked reviewer's approval is not answered as a fact
		// that would let a grant through; it just never yields an approval.
		h.Grant(t, memory.Reviewer)
		if _, err := ask(h, c, g, memory.RepoAPI); err != nil {
			t.Fatalf("the reviewer holds the authority again: %v", err)
		}
		// An authority outage is no approval and invalidates nothing.
		deps := h.E.Dependencies()
		deps.Authority = downAuthority{deps.Authority}
		down, err := gates.NewService(deps, h.E.ServiceOptions())
		must(t, err)
		if _, err := down.IntegrationApproval(bg, c.Scope, c.Run, g.ID, memory.RepoAPI); !errors.Is(err, gates.ErrUnavailable) {
			t.Fatalf("an authority outage: %v", err)
		}
		if cur := current(t, h, g); cur.Invalidation != nil {
			t.Fatalf("authority checks invalidated: %+v", cur.Invalidation)
		}
	})

	t.Run("a candidate changed after the approval yields none", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		h.PublishToTask(t, c, memory.RepoAPI, memory.MovedTip)
		_, err := ask(h, c, g, memory.RepoAPI)
		noApproval(t, "a changed candidate", err)
		if cur := current(t, h, g); cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonCandidateChanged {
			t.Fatalf("not invalidated: %+v", cur.Invalidation)
		}
	})

	t.Run("a revoked content revision yields none", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		h.ChangeContent(t, c.Case)
		_, err := ask(h, c, g, memory.RepoAPI)
		noApproval(t, "changed content", err)
	})

	t.Run("an outage of the base reader is no approval", func(t *testing.T) {
		h := setHarness(t)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		h.E.Bases.Fail(errors.New("git down"))
		if _, err := ask(h, c, g, memory.RepoAPI); !errors.Is(err, gates.ErrUnavailable) {
			t.Fatalf("an outage: %v", err)
		}
		if cur := current(t, h, g); cur.Invalidation != nil {
			t.Fatalf("an outage invalidated: %+v", cur.Invalidation)
		}
		h.E.Bases.Fail(nil)
		if _, err := ask(h, c, g, memory.RepoAPI); err != nil {
			t.Fatalf("after the outage: %v", err)
		}
	})

	t.Run("nothing to integrate when the candidate is the base", func(t *testing.T) {
		h := setHarness(t)
		h.E.Bases.Set(memory.RepoAPI, memory.DefaultRef, memory.APINew)
		c, g := requestSet(t, h, memory.SourceSetSpec{})
		g = approve(t, h, g)
		process(t, h, g)
		_, err := ask(h, c, g, memory.RepoAPI)
		noApproval(t, "the candidate already is the default branch's tip", err)
	})

	t.Run("the module mints no grant: its API holds no credential", func(t *testing.T) {
		got := gates.IntegrationApproval{}
		raw, _ := json.Marshal(got)
		for _, forbidden := range []string{"credential", "token", "secret", "password", "grant"} {
			if strings.Contains(strings.ToLower(string(raw)), forbidden) {
				t.Fatalf("the approval carries %q: %s", forbidden, raw)
			}
		}
	})
}

// Test_unit_single_source_subjects_unchanged (T-149): a subject of a
// single-repository gate keeps its canonical bytes and digest exactly. The
// fixture file was written before source sets existed; its checksum and the
// bytes and digest of its first case were captured from the commit before this
// task and are pinned here, so regenerating the file cannot hide a change. Every
// case still restores and reproduces byte for byte, and a gate of a
// single-source run has no source set, no integration base and a candidateCommit
// member.
func Test_unit_single_source_subjects_unchanged(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "canonical_subject.json"))
	if err != nil {
		t.Fatal(err)
	}
	const (
		fixtureSum      = "c0fa2aff6d4ac50bcb165c68fa0af1118248c756f6800715c640727a71c96221"
		specCanonical   = `{"candidateCommit":"1111111111111111111111111111111111111111","contentRevision":2,"definitionId":"pd_01","deploymentConfigDigest":null,"deploymentId":null,"evidence":[{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","id":"art_tests"}],"projectId":"p_inventory","tenantId":"t_acme","workItemId":"wi_1042"}`
		specCanonicalID = "sha256:5f6f22a353ca87984abc73ea3a2794e729632ca5e8b931a3bb7ca08832626d71"
	)
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != fixtureSum {
		t.Fatalf("the canonical subject fixture changed (sha256 %x): single-source subjects must not", sum)
	}
	var cases []struct {
		Name      string `json:"name"`
		Canonical string `json:"canonical"`
		Digest    string `json:"digest"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) < 4 {
		t.Fatalf("fixture: %v (%d cases)", err, len(cases))
	}
	if cases[0].Canonical != specCanonical || cases[0].Digest != specCanonicalID {
		t.Fatalf("the specification example changed: %s %s", cases[0].Canonical, cases[0].Digest)
	}
	for _, fc := range cases {
		s, err := gates.ParseSubject([]byte(fc.Canonical))
		if err != nil {
			t.Fatalf("%s: a subject stored before source sets no longer restores: %v", fc.Name, err)
		}
		if string(s.Canonical()) != fc.Canonical || s.Digest() != fc.Digest {
			t.Fatalf("%s: the subject changed its canonical bytes or digest:\n got %s %s\nwant %s %s", fc.Name, s.Canonical(), s.Digest(), fc.Canonical, fc.Digest)
		}
		if _, isSet := s.SourceSet(); isSet || !strings.Contains(fc.Canonical, `"candidateCommit":"`) || strings.Contains(fc.Canonical, "sourceSet") {
			t.Fatalf("%s: a single-source subject has a candidateCommit and no source set: %s", fc.Name, fc.Canonical)
		}
	}

	t.Run("a built single-source subject reproduces the pinned bytes and digest", func(t *testing.T) {
		s, err := gates.NewSubject(gates.SubjectInput{Scope: domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}, WorkItemID: "wi_1042", ContentRevision: 2,
			CandidateCommit: memory.Commit, DefinitionID: "pd_01", Evidence: []gates.EvidenceRef{{ID: "art_tests", Digest: "sha256:" + strings.Repeat("a", 64)}}})
		if err != nil {
			t.Fatal(err)
		}
		if string(s.Canonical()) != specCanonical || s.Digest() != specCanonicalID {
			t.Fatalf("a single-source subject changed:\n got %s %s", s.Canonical(), s.Digest())
		}
	})

	t.Run("a gate of a single-source run is built and compared as before", func(t *testing.T) {
		h := setHarness(t)
		c := h.NewCase(t, true)
		g := requested(t, h, c)
		if _, isSet := g.Subject.SourceSet(); isSet || g.Subject.CandidateCommit() != memory.Commit || bytes.Contains(g.Subject.Canonical(), []byte("sourceSet")) ||
			!bytes.Contains(g.Subject.Canonical(), []byte(`"candidateCommit":"`+memory.Commit+`"`)) {
			t.Fatalf("a single-source subject: %s", g.Subject.Canonical())
		}
		if got := g.BaseRepositories(); len(got) != 0 {
			t.Fatalf("a single-source subject has bases: %v", got)
		}
		if calls := h.E.Bases.Calls(); len(calls) != 0 {
			t.Fatalf("bases were read for a single-source gate: %v", calls)
		}
		g2, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		if rep := process(t, h, g2); rep.Completed != 1 {
			t.Fatalf("progress: %+v", rep)
		}
	})
}

// downAuthority answers every permission question as an outage.
type downAuthority struct{ gates.Authority }

func (downAuthority) Authorize(context.Context, gates.Actor, domain.Scope, gates.Permission) error {
	return errors.New("identity down")
}
