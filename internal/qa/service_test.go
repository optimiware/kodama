package qa_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
)

// These tests run the use case against in-memory ports: authorization decisions,
// check order, the no-write-before-verification rule and retry semantics. The
// real adapters over PostgreSQL and S3 are covered in ./integration.

var (
	uScope   = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	uAttempt = domain.AttemptID("att_qa")
)

const (
	cfgDigest runtimev1.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imgDigest runtimev1.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type world struct {
	mu          sync.Mutex
	facts       qa.AttemptFacts
	meta        map[domain.ArtifactID]qa.EvidenceMeta
	content     map[domain.ArtifactID][]byte
	deployments map[string]qa.DeploymentFacts
	subject     qa.SubjectFacts
	defects     map[domain.WorkItemID]*qa.Defect
	pins        []string
	payloads    map[domain.WorkItemID]string
	calls       []string
	failOpen    map[int]error // by call number
	opens       int
	report      qa.Report
}

func shot(id string, d runtimev1.Digest) (domain.ArtifactID, qa.EvidenceMeta) {
	a := domain.ArtifactID(id)
	return a, qa.EvidenceMeta{ID: a, Scope: uScope, Run: "run_qa", Attempt: uAttempt, Step: "qa", Digest: d, MediaType: "image/png", File: true, Usable: true}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		meta: map[domain.ArtifactID]qa.EvidenceMeta{}, content: map[domain.ArtifactID][]byte{},
		deployments: map[string]qa.DeploymentFacts{}, defects: map[domain.WorkItemID]*qa.Defect{}, failOpen: map[int]error{},
		payloads: map[domain.WorkItemID]string{},
	}
	s1, m1 := shot("art_s1", "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	s2, m2 := shot("art_s2", "sha256:2222222222222222222222222222222222222222222222222222222222222222")
	w.meta[s1], w.meta[s2] = m1, m2
	crit := "AC-1"
	w.report = qa.Report{
		Format:     qa.ReportFormat,
		Subject:    qa.Subject{WorkItemID: "wi_1", ContentRevision: 2},
		Deployment: qa.DeploymentRef{ID: "dep_1", EnvironmentID: "env_1", ConfigDigest: cfgDigest, CandidateCommit: commitA, ImageDigest: imgDigest},
		Summary:    "explored",
		Observations: []qa.Observation{
			{Title: "First", Severity: qa.SeverityMajor, CriterionID: &crit, Location: "/a", Steps: []string{"go"}, Expected: "e", Actual: "a",
				Evidence: []qa.EvidenceRef{{ArtifactID: s1, Digest: m1.Digest, Kind: qa.EvidenceScreenshot}}},
			{Title: "Second", Severity: qa.SeverityMinor, Location: "/b", Steps: []string{"go"}, Expected: "e", Actual: "a",
				Evidence: []qa.EvidenceRef{{ArtifactID: s2, Digest: m2.Digest, Kind: qa.EvidenceScreenshot}}},
		},
	}
	rep := domain.ArtifactID("art_report")
	w.facts = qa.AttemptFacts{
		Scope: uScope, Run: "run_qa", Step: "qa", Attempt: uAttempt, Completed: true,
		Outputs: map[string]string{"qa-report": string(rep), "shot-1": string(s1), "shot-2": string(s2)},
		Context: runtimev1.Context{
			TenantID: uScope.TenantID, ProjectID: uScope.ProjectID, RunID: "run_qa", StepID: "qa", AttemptID: uAttempt,
			WorkItem:    &runtimev1.WorkItem{ID: "wi_1", ContentRevision: 2, AcceptanceCriteria: []runtimev1.AcceptanceCriterion{{ID: "AC-1", Text: "works"}}},
			Environment: &runtimev1.EnvironmentPin{ID: "env_1", ConfigDigest: cfgDigest},
			Inputs:      map[string]runtimev1.Value{"deployment": runtimev1.StringValue("dep_1")},
			Outputs: []runtimev1.OutputDecl{
				{Name: "qa-report", Type: runtimev1.OutputArtifact}, {Name: "shot-1", Type: runtimev1.OutputArtifact}, {Name: "shot-2", Type: runtimev1.OutputArtifact},
			},
		},
	}
	w.meta[rep] = qa.EvidenceMeta{ID: rep, Scope: uScope, Run: "run_qa", Attempt: uAttempt, Step: "qa", Digest: "sha256:3333333333333333333333333333333333333333333333333333333333333333", MediaType: "application/json", File: true, Usable: true}
	w.deployments["dep_1"] = qa.DeploymentFacts{ID: "dep_1", Scope: uScope, Environment: "env_1", ConfigDigest: cfgDigest, Candidate: commitA, Image: imgDigest, Succeeded: true}
	w.subject = qa.SubjectFacts{ID: "wi_1", ContentRevision: 2}
	w.publish()
	return w
}

// publish writes the current report as the report artifact's content.
func (w *world) publish() {
	raw, err := w.report.Canonical()
	if err != nil {
		panic(err)
	}
	w.content["art_report"] = raw
}

func (w *world) service(t *testing.T) *qa.Service {
	t.Helper()
	s, err := qa.NewService(qa.Config{Attempts: (*uAttempts)(w), Deployments: (*uDeployments)(w), Artifacts: (*uArtifacts)(w), Defects: (*uDefects)(w), ReportOutput: "qa-report", DeploymentInput: "deployment"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (w *world) note(s string) { w.mu.Lock(); w.calls = append(w.calls, s); w.mu.Unlock() }

type (
	uAttempts    world
	uDeployments world
	uArtifacts   world
	uDefects     world
)

func (a *uAttempts) Facts(_ context.Context, scope domain.Scope, id domain.AttemptID) (qa.AttemptFacts, error) {
	(*world)(a).note("facts")
	if scope != a.facts.Scope || id != a.facts.Attempt {
		return qa.AttemptFacts{}, qa.Failf(qa.ErrNotFound, "attempt not found")
	}
	return a.facts, nil
}

func (d *uDeployments) Deployment(_ context.Context, scope domain.Scope, id string) (qa.DeploymentFacts, error) {
	(*world)(d).note("deployment")
	f, ok := d.deployments[id]
	if !ok || f.Scope != scope {
		return qa.DeploymentFacts{}, qa.Failf(qa.ErrNotFound, "deployment not found")
	}
	return f, nil
}

func (a *uArtifacts) Describe(_ context.Context, scope domain.Scope, id domain.ArtifactID) (qa.EvidenceMeta, error) {
	(*world)(a).note("describe")
	m, ok := a.meta[id]
	if !ok || m.Scope != scope {
		return qa.EvidenceMeta{}, qa.Failf(qa.ErrNotFound, "artifact not found")
	}
	return m, nil
}

func (a *uArtifacts) Read(_ context.Context, _ domain.Scope, meta qa.EvidenceMeta, max int64) ([]byte, error) {
	(*world)(a).note("read")
	b := a.content[meta.ID]
	if int64(len(b)) > max {
		return nil, qa.Failf(qa.ErrEvidence, "too large")
	}
	return b, nil
}

func (a *uArtifacts) Pin(_ context.Context, _ domain.Scope, id domain.ArtifactID, owner string) error {
	(*world)(a).note("pin")
	a.mu.Lock()
	a.pins = append(a.pins, owner+"|"+string(id))
	a.mu.Unlock()
	return nil
}

func (d *uDefects) Subject(_ context.Context, _ domain.Scope, id domain.WorkItemID) (qa.SubjectFacts, error) {
	(*world)(d).note("subject")
	if id != d.subject.ID {
		return qa.SubjectFacts{}, qa.Failf(qa.ErrNotFound, "not found")
	}
	return d.subject, nil
}

func (d *uDefects) Verify(_ context.Context, draft qa.DefectDraft) error {
	w := (*world)(d)
	w.note("verify")
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.defects[draft.ID]; ok && w.payloads[draft.ID] != draft.Payload {
		return qa.Failf(qa.ErrConflict, "payload differs")
	}
	return nil
}

func (d *uDefects) Open(_ context.Context, draft qa.DefectDraft) (qa.Defect, bool, error) {
	w := (*world)(d)
	w.note("open")
	w.mu.Lock()
	defer w.mu.Unlock()
	w.opens++
	if err := w.failOpen[w.opens]; err != nil {
		return qa.Defect{}, false, err
	}
	if cur, ok := w.defects[draft.ID]; ok {
		return *cur, false, nil
	}
	nd := &qa.Defect{ID: draft.ID, Version: 1, State: "backlog"}
	for _, l := range draft.Evidence {
		nd.Links = append(nd.Links, l.ArtifactID)
	}
	w.defects[draft.ID] = nd
	w.payloads[draft.ID] = draft.Payload
	return *nd, true, nil
}

func (d *uDefects) Attach(_ context.Context, _ domain.Scope, _ domain.AttemptID, _ string, id domain.WorkItemID, links []qa.EvidenceLink) (qa.Defect, int, error) {
	w := (*world)(d)
	w.note("attach")
	w.mu.Lock()
	defer w.mu.Unlock()
	cur := w.defects[id]
	have := map[domain.ArtifactID]bool{}
	for _, l := range cur.Links {
		have[l] = true
	}
	added := 0
	for _, l := range links {
		if !have[l.ArtifactID] {
			cur.Links = append(cur.Links, l.ArtifactID)
			added++
		}
	}
	return *cur, added, nil
}

var system = qa.Caller{Actor: qa.Actor{Type: qa.ActorSystem, ID: "reconciler"}, RequestID: "req_1"}

func cmd(c qa.Caller) qa.RecordCommand {
	return qa.RecordCommand{Scope: uScope, Attempt: uAttempt, Caller: c}
}

func TestRecordAuthorizesCallerBeforeAnyRead(t *testing.T) {
	for name, c := range map[string]qa.Caller{
		"a person":                  {Actor: qa.Actor{Type: qa.ActorUser, ID: "u_reviewer"}},
		"an operator":               {Actor: qa.Actor{Type: qa.ActorOperator, ID: "op"}},
		"another attempt's QA":      {Actor: qa.Actor{Type: qa.ActorWorkload, ID: "att_other"}},
		"an unattributed caller":    {Actor: qa.Actor{Type: qa.ActorSystem}},
		"an unknown actor type":     {Actor: qa.Actor{Type: "root", ID: "x"}},
		"an oversized request id":   {Actor: qa.Actor{Type: qa.ActorSystem, ID: "r"}, RequestID: string(make([]byte, 300))},
		"a workload under a prefix": {Actor: qa.Actor{Type: qa.ActorWorkload, ID: string(uAttempt) + "_x"}},
		// Mapping is a control-plane operation: not even the attempt's own
		// workload identity may ask for it.
		"the attempt's own QA": {Actor: qa.Actor{Type: qa.ActorWorkload, ID: string(uAttempt)}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			_, err := w.service(t).Record(context.Background(), cmd(c))
			if !errors.Is(err, qa.ErrDenied) {
				t.Fatalf("got %v, want denied", err)
			}
			if len(w.calls) != 0 || len(w.defects) != 0 {
				t.Fatalf("a refused caller reached a port: %v", w.calls)
			}
		})
	}
	t.Run("the control plane", func(t *testing.T) {
		w := newWorld(t)
		if _, err := w.service(t).Record(context.Background(), cmd(system)); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRecordRefusalsWriteNothing: every verification failure happens before the
// first pin or defect, with its own class.
func TestRecordRefusalsWriteNothing(t *testing.T) {
	cases := []struct {
		name string
		want error
		mut  func(w *world)
	}{
		{"attempt not completed", qa.ErrNotReady, func(w *world) { w.facts.Completed = false }},
		{"no declared report output", qa.ErrNotReady, func(w *world) { delete(w.facts.Outputs, "qa-report") }},
		{"report output not declared as an artifact", qa.ErrNotReady, func(w *world) { w.facts.Context.Outputs[0].Type = runtimev1.OutputString }},
		{"attempt without a pinned environment", qa.ErrContext, func(w *world) { w.facts.Context.Environment = nil }},
		{"attempt without a pinned work item", qa.ErrContext, func(w *world) { w.facts.Context.WorkItem = nil }},
		{"report judged another work item", qa.ErrContext, func(w *world) { w.report.Subject.WorkItemID = "wi_9"; w.publish() }},
		{"report judged another content revision", qa.ErrContext, func(w *world) { w.report.Subject.ContentRevision = 1; w.publish() }},
		{"report cites a criterion the context lacks", qa.ErrContext, func(w *world) { c := "AC-9"; w.report.Observations[0].CriterionID = &c; w.publish() }},
		{"subject cancelled", qa.ErrContext, func(w *world) { w.subject.Cancelled = true }},
		{"unknown deployment", qa.ErrWrongDeployment, func(w *world) {
			w.facts.Context.Inputs["deployment"] = runtimev1.StringValue("dep_nope")
			w.report.Deployment.ID = "dep_nope"
			w.publish()
		}},
		{"report names another succeeded deployment than the pinned input", qa.ErrWrongDeployment, func(w *world) {
			// Same environment and configuration, another candidate; the report
			// states that deployment's facts exactly, so only the trusted pinned
			// input can tell it apart.
			w.deployments["dep_2"] = qa.DeploymentFacts{ID: "dep_2", Scope: uScope, Environment: "env_1", ConfigDigest: cfgDigest, Candidate: commitB, Image: imgDigest, Succeeded: true}
			w.report.Deployment.ID, w.report.Deployment.CandidateCommit = "dep_2", commitB
			w.publish()
		}},
		{"attempt without a pinned deployment input", qa.ErrContext, func(w *world) { delete(w.facts.Context.Inputs, "deployment") }},
		{"pinned deployment input that is not a string", qa.ErrContext, func(w *world) { w.facts.Context.Inputs["deployment"] = runtimev1.IntValue(7) }},
		{"pinned deployment input that is empty", qa.ErrContext, func(w *world) { w.facts.Context.Inputs["deployment"] = runtimev1.StringValue("") }},
		{"foreign deployment", qa.ErrWrongDeployment, func(w *world) {
			d := w.deployments["dep_1"]
			d.Scope = domain.Scope{TenantID: "t_other", ProjectID: "p_inventory"}
			w.deployments["dep_1"] = d
		}},
		{"reported environment differs from the pin", qa.ErrWrongDeployment, func(w *world) { w.report.Deployment.EnvironmentID = "env_2"; w.publish() }},
		{"reported configuration differs from the pin", qa.ErrWrongDeployment, func(w *world) {
			w.report.Deployment.ConfigDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
			w.publish()
		}},
		{"deployment recorded under another configuration", qa.ErrWrongDeployment, func(w *world) {
			d := w.deployments["dep_1"]
			d.ConfigDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
			w.deployments["dep_1"] = d
		}},
		{"deployment of another environment", qa.ErrWrongDeployment, func(w *world) {
			d := w.deployments["dep_1"]
			d.Environment = "env_2"
			w.deployments["dep_1"] = d
		}},
		{"reported candidate differs from the deployment", qa.ErrWrongDeployment, func(w *world) { w.report.Deployment.CandidateCommit = commitB; w.publish() }},
		{"reported image differs from the deployment", qa.ErrWrongDeployment, func(w *world) {
			w.report.Deployment.ImageDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
			w.publish()
		}},
		{"deployment did not succeed", qa.ErrWrongDeployment, func(w *world) {
			d := w.deployments["dep_1"]
			d.Succeeded = false
			w.deployments["dep_1"] = d
		}},
		{"evidence from another attempt", qa.ErrEvidence, func(w *world) {
			m := w.meta["art_s1"]
			m.Attempt = "att_other"
			w.meta["art_s1"] = m
		}},
		{"evidence of another project", qa.ErrEvidence, func(w *world) {
			m := w.meta["art_s1"]
			m.Scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_billing"}
			w.meta["art_s1"] = m
		}},
		{"evidence not usable", qa.ErrEvidence, func(w *world) { m := w.meta["art_s1"]; m.Usable = false; w.meta["art_s1"] = m }},
		{"evidence not a file", qa.ErrEvidence, func(w *world) { m := w.meta["art_s1"]; m.File = false; w.meta["art_s1"] = m }},
		{"evidence with another digest", qa.ErrEvidence, func(w *world) {
			m := w.meta["art_s1"]
			m.Digest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
			w.meta["art_s1"] = m
		}},
		{"evidence not finalized by the accepted result", qa.ErrEvidence, func(w *world) { delete(w.facts.Outputs, "shot-1") }},
		{"screenshot that is not an image", qa.ErrEvidence, func(w *world) { m := w.meta["art_s1"]; m.MediaType = "text/plain"; w.meta["art_s1"] = m }},
		{"report artifact of another attempt", qa.ErrEvidence, func(w *world) {
			m := w.meta["art_report"]
			m.Attempt = "att_other"
			w.meta["art_report"] = m
		}},
		{"garbage report", qa.ErrInvalid, func(w *world) { w.content["art_report"] = []byte(`{"approved": true}`) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			c.mut(w)
			_, err := w.service(t).Record(context.Background(), cmd(system))
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if len(w.pins) != 0 || len(w.defects) != 0 || w.opens != 0 {
				t.Fatalf("a refused report wrote: pins=%v defects=%d", w.pins, len(w.defects))
			}
		})
	}
}

func TestRecordFilesOneDeterministicDefectPerFinding(t *testing.T) {
	w := newWorld(t)
	s := w.service(t)
	first, err := s.Record(context.Background(), cmd(system))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Defects) != 2 || len(w.defects) != 2 {
		t.Fatalf("defects: %+v", first.Defects)
	}
	for i, d := range first.Defects {
		if !d.Created || d.ID != qa.DefectIDFor(d.Key) || d.Index != i || d.EvidenceAdded != 2 {
			t.Fatalf("defect %d: %+v", i, d)
		}
	}
	// Every defect's evidence (and the report) is pinned to that defect first.
	if len(w.pins) != 4 {
		t.Fatalf("pins: %v", w.pins)
	}
	again, err := s.Record(context.Background(), cmd(system))
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range again.Defects {
		if d.Created || d.ID != first.Defects[i].ID || d.Key != first.Defects[i].Key || d.EvidenceAdded != 0 {
			t.Fatalf("replay %d: %+v", i, d)
		}
	}
	if len(w.defects) != 2 {
		t.Fatalf("a replay created defects: %d", len(w.defects))
	}
	if first.Report != again.Report || first.Report.Digest == "" {
		t.Fatalf("report refs: %+v %+v", first.Report, again.Report)
	}
}

func TestRecordPartialFailureResumesWithoutDuplicates(t *testing.T) {
	w := newWorld(t)
	w.failOpen[2] = qa.Unavailablef(nil, "store down")
	s := w.service(t)
	res, err := s.Record(context.Background(), cmd(system))
	if !errors.Is(err, qa.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if len(res.Defects) != 2 || !res.Defects[0].Created || len(w.defects) != 1 {
		t.Fatalf("partial result: %+v defects=%d", res.Defects, len(w.defects))
	}
	res2, err := s.Record(context.Background(), cmd(system))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.defects) != 2 || res2.Defects[0].Created || !res2.Defects[1].Created {
		t.Fatalf("resume: %+v defects=%d", res2.Defects, len(w.defects))
	}
	ids := []string{}
	for id := range w.defects {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	if ids[0] == ids[1] {
		t.Fatal("identical identities")
	}
}

func TestRecordClassifiesUnknownFailuresAsUnavailable(t *testing.T) {
	w := newWorld(t)
	w.failOpen[1] = errors.New("pq: connection reset")
	_, err := w.service(t).Record(context.Background(), cmd(system))
	if !errors.Is(err, qa.ErrUnavailable) || !qa.Classified(err) {
		t.Fatalf("got %v", err)
	}
}

// TestChangedPayloadIsRefusedBeforeAnyPin: a second report whose finding has the
// same key but different substantive content is a conflict that pins and writes
// nothing, while a report that differs only in evidence is still accepted.
func TestChangedPayloadIsRefusedBeforeAnyPin(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	s := w.service(t)
	if _, err := s.Record(ctx, cmd(system)); err != nil {
		t.Fatal(err)
	}
	pins := len(w.pins)

	for name, edit := range map[string]func(o *qa.Observation){
		"severity": func(o *qa.Observation) { o.Severity = qa.SeverityCritical },
		"steps":    func(o *qa.Observation) { o.Steps = []string{"go", "click twice"} },
		"expected": func(o *qa.Observation) { o.Expected = "something else" },
		"actual":   func(o *qa.Observation) { o.Actual = "something worse" },
	} {
		t.Run(name, func(t *testing.T) {
			w.report.Observations[0].Evidence = append([]qa.EvidenceRef(nil), w.report.Observations[0].Evidence...)
			saved := w.report
			saved.Observations = append([]qa.Observation(nil), w.report.Observations...)
			edit(&w.report.Observations[0])
			w.publish()
			defer func() { w.report = saved; w.publish() }()
			if _, err := s.Record(ctx, cmd(system)); !errors.Is(err, qa.ErrConflict) {
				t.Fatalf("got %v, want conflict", err)
			}
			if len(w.pins) != pins || len(w.defects) != 2 {
				t.Fatalf("a conflicting payload pinned or filed: pins=%d defects=%d", len(w.pins)-pins, len(w.defects))
			}
		})
	}

	// Evidence-only change: the same payload stays accepted.
	w.report.Observations[0].Evidence = append(w.report.Observations[0].Evidence, qa.EvidenceRef{ArtifactID: "art_s2", Digest: w.meta["art_s2"].Digest, Kind: qa.EvidenceScreenshot})
	w.publish()
	if _, err := s.Record(ctx, cmd(system)); err != nil {
		t.Fatalf("an evidence-only addition was refused: %v", err)
	}
}

func TestDefectIdentityConflictIsRefused(t *testing.T) {
	w := newWorld(t)
	w.failOpen = nil
	s := w.service(t)
	res, err := s.Record(context.Background(), cmd(system))
	if err != nil {
		t.Fatal(err)
	}
	// Another record answers under the first defect's identity.
	w.defects[res.Defects[0].ID].ID = "wi_other"
	if _, err := s.Record(context.Background(), cmd(system)); !errors.Is(err, qa.ErrConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestNewServiceRequiresEveryCollaborator(t *testing.T) {
	w := newWorld(t)
	full := qa.Config{Attempts: (*uAttempts)(w), Deployments: (*uDeployments)(w), Artifacts: (*uArtifacts)(w), Defects: (*uDefects)(w), ReportOutput: "qa-report", DeploymentInput: "deployment"}
	for name, mut := range map[string]func(*qa.Config){
		"attempts":    func(c *qa.Config) { c.Attempts = nil },
		"deployments": func(c *qa.Config) { c.Deployments = nil },
		"artifacts":   func(c *qa.Config) { c.Artifacts = nil },
		"defects":     func(c *qa.Config) { c.Defects = nil },
		"output":      func(c *qa.Config) { c.ReportOutput = "" },
		"deployment":  func(c *qa.Config) { c.DeploymentInput = "" },
		"bound":       func(c *qa.Config) { c.MaxReportBytes = qa.MaxReportBytes + 1 },
	} {
		c := full
		mut(&c)
		if _, err := qa.NewService(c); err == nil {
			t.Errorf("a service without %s was constructed", name)
		}
	}
}

// TestSurfaceHasNoDecisionOperation (DQ-06, AR-12): the use case exposes only
// Record, and the work port it reaches offers only read, create-or-observe and
// evidence attachment, so no approval, completion or transition can be added by
// a QA report or reached through the module.
func TestSurfaceHasNoDecisionOperation(t *testing.T) {
	methods := func(t reflect.Type) []string {
		var out []string
		for i := 0; i < t.NumMethod(); i++ {
			out = append(out, t.Method(i).Name)
		}
		sort.Strings(out)
		return out
	}
	if got := methods(reflect.TypeOf(&qa.Service{})); !reflect.DeepEqual(got, []string{"Record"}) {
		t.Fatalf("service methods: %v", got)
	}
	if got := methods(reflect.TypeOf((*qa.Defects)(nil)).Elem()); !reflect.DeepEqual(got, []string{"Attach", "Open", "Subject", "Verify"}) {
		t.Fatalf("defects port: %v", got)
	}
}
