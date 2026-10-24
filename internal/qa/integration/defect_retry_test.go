package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/qa/qatest"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// historyRows counts the work_item_history rows of a defect, and eventsOf its
// created/updated outbox events.
func (s *stack) historyRows(id domain.WorkItemID) int {
	return s.Count(`SELECT count(*) FROM work_item_history WHERE tenant_id = $1 AND work_item_id = $2`, string(scopeA.TenantID), string(id))
}

func (s *stack) workEvents(typ string) int {
	return s.X.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = $1`, typ)
}

func (s *stack) workAudits(action string) int {
	return s.Count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = $2 AND resource_type = 'work_item'`, string(scopeA.TenantID), action)
}

// Test_qa_defect_retry proves that mapping findings to defects is repeatable:
// a replay, a retry after a lost acknowledgment, a retry after a rolled-back
// write, a retry after a partial failure and concurrent callers all converge
// on exactly one defect per finding (DQ-10, WM-01), each created atomically
// with its history, audit record and event (WM-08).
func Test_qa_defect_retry(t *testing.T) {
	three := func(s *stack) []qa.Observation {
		return []qa.Observation{
			s.finding("Order form shows a blank page", "/orders/new", 0),
			s.finding("Totals are wrong", "/orders", 1),
			s.finding("Quantity accepts negative numbers", "/orders/new", 0, 1),
		}
	}

	t.Run("a replay returns the same defects and writes nothing", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(three(s)...)
		first := s.mustRecord(t)
		if len(first.Defects) != 3 || s.DefectRows(scopeA) != 3 {
			t.Fatalf("first: %+v", first.Defects)
		}
		ids := map[domain.WorkItemID]bool{}
		for i, d := range first.Defects {
			if !d.Created || d.ID != qa.DefectIDFor(d.Key) || d.State != "backlog" {
				t.Fatalf("defect %d: %+v", i, d)
			}
			ids[d.ID] = true
		}
		if len(ids) != 3 {
			t.Fatal("two findings share an identity")
		}
		history, events, audits := s.Count(`SELECT count(*) FROM work_item_history WHERE tenant_id = $1`, string(scopeA.TenantID)), s.workEvents("work_item.created"), s.workAudits("work_item.created")

		again := s.mustRecord(t)
		for i, d := range again.Defects {
			if d.Created || d.ID != first.Defects[i].ID || d.EvidenceAdded != 0 {
				t.Fatalf("replay %d: %+v", i, d)
			}
		}
		if s.DefectRows(scopeA) != 3 ||
			s.Count(`SELECT count(*) FROM work_item_history WHERE tenant_id = $1`, string(scopeA.TenantID)) != history ||
			s.workEvents("work_item.created") != events || s.workAudits("work_item.created") != audits {
			t.Fatal("a replay wrote history, audit or events")
		}
		for _, d := range first.Defects {
			if got := s.historyRows(d.ID); got != 1 {
				t.Fatalf("defect %s has %d history rows, want the single creation entry", d.ID, got)
			}
		}
	})

	t.Run("a lost commit acknowledgment is not a failure and not a duplicate", func(t *testing.T) {
		var db *qatest.CommitFault
		s := newStack(t, qatest.Options{WrapWorkDB: func(in storage.TxBeginner) storage.TxBeginner { db = &qatest.CommitFault{Inner: in}; return db }})
		s.complete(three(s)...)
		db.Lose() // every defect commit applies, none is acknowledged
		res, err := s.record()
		db.Off()
		if err != nil {
			t.Fatalf("the committed defects were not observed after the lost acknowledgments: %v", err)
		}
		if len(res.Defects) != 3 || s.DefectRows(scopeA) != 3 {
			t.Fatalf("defects: %+v rows=%d", res.Defects, s.DefectRows(scopeA))
		}
		for _, d := range res.Defects {
			if d.Created {
				t.Fatalf("defect %s: the lost acknowledgment path must observe the commit by rereading, not report a creation", d.ID)
			}
			if s.historyRows(d.ID) != 1 {
				t.Fatalf("defect %s history: %d", d.ID, s.historyRows(d.ID))
			}
		}
		again := s.mustRecord(t)
		for i, d := range again.Defects {
			if d.ID != res.Defects[i].ID || d.Created {
				t.Fatalf("retry %d: %+v", i, d)
			}
		}
		if s.DefectRows(scopeA) != 3 || s.workEvents("work_item.created") != 3 {
			t.Fatal("the retry created a duplicate")
		}
	})

	t.Run("a rolled-back write leaves nothing and the retry creates exactly one defect each", func(t *testing.T) {
		var db *qatest.CommitFault
		s := newStack(t, qatest.Options{WrapWorkDB: func(in storage.TxBeginner) storage.TxBeginner { db = &qatest.CommitFault{Inner: in}; return db }})
		s.complete(three(s)...)
		db.Fail()
		res, err := s.record()
		db.Off()
		is(t, err, qa.ErrUnavailable)
		if len(res.Defects) != 1 || res.Defects[0].Created {
			t.Fatalf("the first failure stops the pass: %+v", res.Defects)
		}
		if s.DefectRows(scopeA) != 0 || s.workEvents("work_item.created") != 0 || s.workAudits("work_item.created") != 0 {
			t.Fatal("a rolled-back commit left state behind")
		}
		retry := s.mustRecord(t)
		if s.DefectRows(scopeA) != 3 || len(retry.Defects) != 3 {
			t.Fatalf("retry: %+v", retry.Defects)
		}
		for _, d := range retry.Defects {
			if !d.Created {
				t.Fatalf("the retry should have created %s", d.ID)
			}
		}
	})

	t.Run("an event append failing after the rows were written rolls the whole defect back", func(t *testing.T) {
		var fa *qatest.FaultAppender
		s := newStack(t, qatest.Options{WrapWorkAppender: func(in workpg.EventAppender) workpg.EventAppender { fa = &qatest.FaultAppender{Inner: in}; return fa }})
		s.complete(three(s)...)
		fa.FailNext(1)
		_, err := s.record()
		is(t, err, qa.ErrUnavailable)
		if s.DefectRows(scopeA) != 0 ||
			s.Count(`SELECT count(*) FROM work_item_history h JOIN work_items w ON w.tenant_id = h.tenant_id AND w.id = h.work_item_id WHERE h.tenant_id = $1 AND w.kind = 'defect'`, string(scopeA.TenantID)) != 0 ||
			s.Count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = 'work_item.created' AND resource_type = 'work_item' AND actor_type = 'workload'`, string(scopeA.TenantID)) != 0 {
			t.Fatal("a defect's rows survived the failed event append: creation is not atomic")
		}
		s.mustRecord(t)
		if s.DefectRows(scopeA) != 3 {
			t.Fatalf("defects after the retry: %d", s.DefectRows(scopeA))
		}
	})

	t.Run("a partial failure keeps the filed defects and the retry files the rest", func(t *testing.T) {
		var fa *qatest.FaultAppender
		s := newStack(t, qatest.Options{WrapWorkAppender: func(in workpg.EventAppender) workpg.EventAppender { fa = &qatest.FaultAppender{Inner: in}; return fa }})
		s.complete(three(s)...)
		fa.SkipFail(1, 1) // the first defect commits; the second one's event append fails
		res, err := s.record()
		is(t, err, qa.ErrUnavailable)
		if len(res.Defects) != 2 || !res.Defects[0].Created || s.DefectRows(scopeA) != 1 {
			t.Fatalf("partial: %+v rows=%d", res.Defects, s.DefectRows(scopeA))
		}
		first := res.Defects[0].ID
		retry := s.mustRecord(t)
		if s.DefectRows(scopeA) != 3 || retry.Defects[0].Created || retry.Defects[0].ID != first || !retry.Defects[1].Created || !retry.Defects[2].Created {
			t.Fatalf("retry: %+v rows=%d", retry.Defects, s.DefectRows(scopeA))
		}
		if s.historyRows(first) != 1 {
			t.Fatal("the first defect was written twice")
		}
	})

	t.Run("concurrent callers converge on one defect per finding", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(three(s)...)
		const callers = 8
		var wg sync.WaitGroup
		results := make([]qa.Result, callers)
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = s.Service.Record(context.Background(), s.Run.Command())
			}()
		}
		wg.Wait()
		created := map[domain.WorkItemID]int{}
		for i, err := range errs {
			if err != nil {
				t.Fatalf("caller %d: %v", i, err)
			}
			for j, d := range results[i].Defects {
				if d.ID != results[0].Defects[j].ID {
					t.Fatalf("callers disagree on defect %d", j)
				}
				if d.Created {
					created[d.ID]++
				}
			}
		}
		if s.DefectRows(scopeA) != 3 {
			t.Fatalf("rows: %d", s.DefectRows(scopeA))
		}
		for id, n := range created {
			if n != 1 {
				t.Fatalf("defect %s was created %d times", id, n)
			}
			if s.historyRows(id) != 1 {
				t.Fatalf("defect %s has %d history rows", id, s.historyRows(id))
			}
		}
		if s.workEvents("work_item.created") != 3 {
			t.Fatalf("created events: %d", s.workEvents("work_item.created"))
		}
	})

	t.Run("a second pass over the same finding adds its evidence to the same defect", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		first := s.mustRecord(t)
		id := first.Defects[0].ID
		before, err := s.Work(scopeA, id)
		if err != nil {
			t.Fatal(err)
		}

		// A second QA attempt of the same work item observes the same finding on
		// the same deployment, with its own screenshot and report.
		second := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1})
		sid, sd := second.Screenshot(1)
		other := qatest.Observation("Another finding", "/orders", qatest.Evidence(sid, sd, qa.EvidenceScreenshot, "second pass"))
		same := qatest.Observation("  order FORM shows a blank page ", "/orders/new/", qatest.Evidence(sid, sd, qa.EvidenceScreenshot, "second pass"))
		// Wording noise in the title and a trailing slash in the route are the
		// same finding; the steps, expectation and severity are the same claim.
		same.Steps = []string{"Open /orders/new", "Submit the form"}
		second.Complete(second.ReportFor(s.Dep, same, other))
		res, err := s.Service.Record(context.Background(), second.Command())
		if err != nil {
			t.Fatal(err)
		}
		if res.Defects[0].ID != id || res.Defects[0].Created || res.Defects[0].EvidenceAdded != 2 || !res.Defects[1].Created {
			t.Fatalf("second pass: %+v", res.Defects)
		}
		if s.DefectRows(scopeA) != 2 {
			t.Fatalf("rows: %d", s.DefectRows(scopeA))
		}
		after, err := s.Work(scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.EvidenceLinks) != len(before.EvidenceLinks)+2 || after.Version != before.Version+1 || after.Spec != before.Spec || after.State != before.State {
			t.Fatalf("links %d -> %d, version %d -> %d", len(before.EvidenceLinks), len(after.EvidenceLinks), before.Version, after.Version)
		}
		hist, err := s.WorkStore.History(context.Background(), scopeA, id, work.HistoryQuery{Limit: 10})
		if err != nil || len(hist) != 2 {
			t.Fatalf("history: %v %d", err, len(hist))
		}
		if h := hist[1]; h.Kind != work.ChangeUpdated || h.Actor.Type != work.ActorUser || h.Actor.ID != qatest.Mapper || len(h.ChangedFields) != 1 || h.ChangedFields[0] != work.FieldEvidenceLinks {
			t.Fatalf("the merge is not attributed to the mapping principal as an evidence-link edit: %+v", h)
		}
		// Repeating the second pass changes nothing.
		if res, err = s.Service.Record(context.Background(), second.Command()); err != nil || res.Defects[0].EvidenceAdded != 0 {
			t.Fatalf("repeat: %+v %v", res, err)
		}
		if again, _ := s.Work(scopeA, id); again.Version != after.Version {
			t.Fatal("a repeated pass edited the defect")
		}
	})

	t.Run("a changed payload under the same finding key conflicts and mutates nothing", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		first := s.mustRecord(t)
		id := first.Defects[0].ID
		before, err := s.Work(scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		hist, pins, created := s.historyRows(id), s.Pins(scopeA, id), s.workEvents("work_item.created")
		updated, audits := s.workEvents("work_item.updated"), s.workAudits("work_item.updated")

		for name, edit := range map[string]func(o *qa.Observation){
			"severity": func(o *qa.Observation) { o.Severity = qa.SeverityTrivial },
			"steps":    func(o *qa.Observation) { o.Steps = []string{"Open /orders/new", "Press enter twice"} },
			"expected": func(o *qa.Observation) { o.Expected = "The order is rejected with a message." },
			"actual":   func(o *qa.Observation) { o.Actual = "The server answers 500." },
		} {
			second := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1})
			sid, sd := second.Screenshot(1)
			// Same finding key (title, route, criterion); one substantive field differs
			// and the report brings a new screenshot that must not be linked or pinned.
			o := qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(sid, sd, qa.EvidenceScreenshot, "changed claim"))
			edit(&o)
			if o.Fingerprint() != s.finding("Order form shows a blank page", "/orders/new", 0).Fingerprint() {
				t.Fatalf("%s: the edit changed the finding key", name)
			}
			second.Complete(second.ReportFor(s.Dep, o))
			res, err := s.Service.Record(context.Background(), second.Command())
			if !errors.Is(err, qa.ErrConflict) {
				t.Fatalf("%s: got %v, want conflict", name, err)
			}
			for _, d := range res.Defects {
				if d.Created || d.EvidenceAdded != 0 {
					t.Fatalf("%s: a conflicting finding reported a mutation: %+v", name, d)
				}
			}
			after, err := s.Work(scopeA, id)
			if err != nil {
				t.Fatal(err)
			}
			if s.DefectRows(scopeA) != 1 || after.Version != before.Version || len(after.EvidenceLinks) != len(before.EvidenceLinks) || after.Spec != before.Spec ||
				s.historyRows(id) != hist || s.Pins(scopeA, id) != pins || s.workEvents("work_item.created") != created ||
				s.workEvents("work_item.updated") != updated || s.workAudits("work_item.updated") != audits {
				t.Fatalf("%s: a conflicting payload changed the first defect or its evidence", name)
			}
		}

		// Control: the same payload with only new evidence still merges, so the
		// conflicts above are the payload comparison and not a general refusal.
		third := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1})
		tid, td := third.Screenshot(1)
		third.Complete(third.ReportFor(s.Dep, qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(tid, td, qa.EvidenceScreenshot, "retry"))))
		res, err := s.Service.Record(context.Background(), third.Command())
		if err != nil || res.Defects[0].ID != id || res.Defects[0].Created || res.Defects[0].EvidenceAdded != 2 {
			t.Fatalf("an evidence-only retry: %+v %v", res, err)
		}
	})

	// The marker is the server's last line of the creation specification. Report
	// text is rendered before it and may span lines, so it can contain a line that
	// looks exactly like a marker; that line is content, never the identity record.
	t.Run("a report whose text contains a fake marker line replays identically", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		fake := qa.PayloadMarker(strings.Repeat("f", 64))
		o := s.finding("Order form shows a blank page", "/orders/new", 0)
		o.Expected = "The order is saved.\n" + fake + "\nand confirmed."
		o.Actual = "A blank page.\n" + fake + "\n"
		s.complete(o)
		first := s.mustRecord(t)
		if len(first.Defects) != 1 || !first.Defects[0].Created {
			t.Fatalf("first: %+v", first.Defects)
		}
		id := first.Defects[0].ID
		if got, _ := s.Work(scopeA, id); !strings.Contains(got.Spec, fake) {
			t.Fatal("the fake line is not part of the rendered specification, so this test proves nothing")
		}
		hist, events, pins := s.historyRows(id), s.workEvents("work_item.created"), s.Pins(scopeA, id)

		again, err := s.record()
		if err != nil {
			t.Fatalf("an identical replay conflicted with its own defect: %v", err)
		}
		if d := again.Defects[0]; d.Created || d.ID != id || d.EvidenceAdded != 0 {
			t.Fatalf("replay: %+v", d)
		}
		// A separate attempt reporting the same payload (new evidence only) merges too.
		second := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1})
		sid, sd := second.Screenshot(1)
		o2 := qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(sid, sd, qa.EvidenceScreenshot, "again"))
		o2.Expected, o2.Actual = o.Expected, o.Actual
		second.Complete(second.ReportFor(s.Dep, o2))
		res, err := s.Service.Record(context.Background(), second.Command())
		if err != nil || res.Defects[0].ID != id || res.Defects[0].Created {
			t.Fatalf("an identical payload from another attempt: %+v %v", res, err)
		}
		if s.DefectRows(scopeA) != 1 || s.historyRows(id) != hist+1 || s.workEvents("work_item.created") != events || s.Pins(scopeA, id) < pins {
			t.Fatal("the replays duplicated the defect or its creation record")
		}
	})

	t.Run("a changed payload that embeds a fake marker still conflicts", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		const title, route = "Order form shows a blank page", "/orders/new"
		// The changed claim (what a later report says) and its true digest.
		changed := s.finding(title, route, 0)
		changed.Actual = "The server answers 500."
		// The first report plants the changed claim's digest as a fake marker line
		// before the server's own marker.
		planted := s.finding(title, route, 0)
		planted.Expected = "The order is saved.\n" + qa.PayloadMarker(changed.PayloadDigest()) + "\n"
		if planted.PayloadDigest() == changed.PayloadDigest() || planted.Fingerprint() != changed.Fingerprint() {
			t.Fatal("setup: the claims must differ in payload and share one finding key")
		}
		s.complete(planted)
		first := s.mustRecord(t)
		id := first.Defects[0].ID
		before, err := s.Work(scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		hist, pins := s.historyRows(id), s.Pins(scopeA, id)

		second := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1})
		sid, sd := second.Screenshot(1)
		changed.Evidence = []qa.EvidenceRef{qatest.Evidence(sid, sd, qa.EvidenceScreenshot, "changed claim")}
		second.Complete(second.ReportFor(s.Dep, changed))
		res, err := s.Service.Record(context.Background(), second.Command())
		if !errors.Is(err, qa.ErrConflict) {
			t.Fatalf("got %v, want conflict: the planted marker was used as the creation payload", err)
		}
		for _, d := range res.Defects {
			if d.Created || d.EvidenceAdded != 0 {
				t.Fatalf("a conflicting finding reported a mutation: %+v", d)
			}
		}
		after, err := s.Work(scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		if s.DefectRows(scopeA) != 1 || after.Version != before.Version || len(after.EvidenceLinks) != len(before.EvidenceLinks) ||
			s.historyRows(id) != hist || s.Pins(scopeA, id) != pins {
			t.Fatal("a conflicting payload changed the first defect or its evidence")
		}
	})

	t.Run("a creation specification without the exact final server marker fails closed", func(t *testing.T) {
		fake := strings.Repeat("b", 64)
		for name, spec := range map[string]func(real string) string{
			"no marker":                   func(string) string { return "## Observation\n\nbody\n" },
			"the real digest mid-spec":    func(real string) string { return qa.PayloadMarker(real) + "\n- Deduplication key: `x`\n" },
			"a marker with a later line":  func(real string) string { return "body\n" + qa.PayloadMarker(real) + "\ntrailing text\n" },
			"another digest as last line": func(string) string { return "body\n" + qa.PayloadMarker(fake) + "\n" },
			"a marker with a suffix":      func(real string) string { return "body\n" + qa.PayloadMarker(real) + " (verified)\n" },
			"a marker inside a long line": func(real string) string { return "body " + qa.PayloadMarker(real) + "\n" },
		} {
			t.Run(name, func(t *testing.T) {
				s := newStack(t, qatest.Options{})
				o := s.finding("Order form shows a blank page", "/orders/new", 0)
				rep := s.Run.ReportFor(s.Dep, o)
				key := qa.DefectKey(scopeA, rep.Subject.WorkItemID, rep.Deployment.CandidateCommit, o.Fingerprint())
				// Seed the identity with a creation record that carries the report's own
				// payload digest somewhere, but not as the exact final server line.
				seed := qa.DefectDraft{
					Scope: scopeA, ID: qa.DefectIDFor(key), Key: key, Payload: o.PayloadDigest(), Attempt: s.Run.ID(),
					Title: "[QA] seeded", Spec: spec(o.PayloadDigest()), RequestID: "req_seed",
					Criteria: []qa.Criterion{{ID: "qa-1", Text: "Expected behavior: x"}},
				}
				if _, created, err := s.Defects.Open(context.Background(), seed); err != nil || !created {
					t.Fatalf("seeding the identity: created=%v err=%v", created, err)
				}
				rows, hist, pins := s.DefectRows(scopeA), s.historyRows(seed.ID), s.Pins(scopeA, seed.ID)

				s.Run.Complete(rep)
				res, err := s.record()
				if !errors.Is(err, qa.ErrConflict) {
					t.Fatalf("got %v, want conflict", err)
				}
				for _, d := range res.Defects {
					if d.Created || d.EvidenceAdded != 0 {
						t.Fatalf("a refused finding reported a mutation: %+v", d)
					}
				}
				if s.DefectRows(scopeA) != rows || s.historyRows(seed.ID) != hist || s.Pins(scopeA, seed.ID) != pins {
					t.Fatal("a refused finding wrote a defect, history or pin")
				}
			})
		}
	})

	t.Run("a closed defect is observed and never edited", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		first := s.mustRecord(t)
		id := first.Defects[0].ID
		s.X.OwnerExec(`UPDATE work_items SET state = 'cancelled', version = version + 1 WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scopeA.TenantID), string(scopeA.ProjectID), string(id))
		before, _ := s.Work(scopeA, id)

		second := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1})
		sid, sd := second.Screenshot(1)
		second.Complete(second.ReportFor(s.Dep, qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(sid, sd, qa.EvidenceScreenshot, ""))))
		res, err := s.Service.Record(context.Background(), second.Command())
		if err != nil {
			t.Fatal(err)
		}
		d := res.Defects[0]
		if d.ID != id || d.Created || d.EvidenceAdded != 0 || d.State != "cancelled" {
			t.Fatalf("closed defect: %+v", d)
		}
		after, _ := s.Work(scopeA, id)
		if after.Version != before.Version || len(after.EvidenceLinks) != len(before.EvidenceLinks) {
			t.Fatal("QA edited a closed defect")
		}
		if s.DefectRows(scopeA) != 1 {
			t.Fatal("a closed defect was duplicated instead of observed")
		}
	})
}
