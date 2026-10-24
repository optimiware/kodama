package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/qa/qatest"
	"github.com/optimiware/kodama/internal/work"
)

// Test_qa_evidence_links proves DQ-10 and DQ-01 for the evidence: a finding is
// an ordinary defect work item whose evidence links are artifacts verified to be
// usable outputs of the QA attempt itself, in the defect's tenant and project,
// with the stated digests, and protected from retention for as long as the
// defect refers to them. Anything else is refused before any write.
func Test_qa_evidence_links(t *testing.T) {
	t.Run("every cited artifact and the report are linked, pinned and kept from retention", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		logID, logDigest := s.Run.Diagnostic("console", "text/plain", []byte("TypeError: x is undefined"))
		scratch, _ := s.Run.Diagnostic("scratch", "text/plain", []byte("never cited"))
		obs := qatest.Observation("Order form shows a blank page", "/orders/new",
			qatest.Evidence(s.Shot[0], s.Dig[0], qa.EvidenceScreenshot, "after Save"),
			qatest.Evidence(s.Shot[1], s.Dig[1], qa.EvidenceScreenshot, ""),
			qatest.Evidence(logID, logDigest, qa.EvidenceLog, "console"))
		s.complete(obs)
		res := s.mustRecord(t)
		id := res.Defects[0].ID

		it, err := s.Work(scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		if it.Kind != work.KindDefect || it.State != work.StateBacklog || !strings.HasPrefix(it.Title, "[QA] ") || len(it.Criteria) != 1 {
			t.Fatalf("not an ordinary defect: %+v", it)
		}
		want := []work.EvidenceRef{
			{Type: work.EvidenceArtifact, ID: string(res.Report.ArtifactID), Note: "QA report"},
			{Type: work.EvidenceArtifact, ID: string(s.Shot[0]), Note: "screenshot: after Save"},
			{Type: work.EvidenceArtifact, ID: string(s.Shot[1]), Note: "screenshot"},
			{Type: work.EvidenceArtifact, ID: string(logID), Note: "log: console"},
		}
		if len(it.EvidenceLinks) != len(want) {
			t.Fatalf("links: %+v", it.EvidenceLinks)
		}
		for i := range want {
			if it.EvidenceLinks[i] != want[i] {
				t.Fatalf("link %d: %+v, want %+v", i, it.EvidenceLinks[i], want[i])
			}
		}
		for _, l := range it.EvidenceLinks {
			if !strings.Contains(it.Spec, "`"+l.ID+"`") && l.ID == string(res.Report.ArtifactID) {
				t.Errorf("the provenance does not name the report artifact")
			}
		}
		if !strings.Contains(it.Spec, string(res.Report.ArtifactDigest)) || !strings.Contains(it.Spec, string(s.Run.ID())) {
			t.Error("the provenance does not identify the report content and the QA attempt")
		}

		// Pins: the defect protects exactly what it links.
		if n := s.Pins(scopeA, id); n != len(want) {
			t.Fatalf("pins: %d, want %d", n, len(want))
		}
		ctx := context.Background()
		actor := artifacts.Actor{Type: "system", ID: "retention"}
		for _, art := range []domain.ArtifactID{s.Shot[0], s.Shot[1], logID, res.Report.ArtifactID} {
			if err := s.ArtStore.BeginRetentionDeletion(ctx, scopeA, art, s.X.Clock.Now(), actor); !errors.Is(err, artifacts.ErrPinned) {
				t.Fatalf("retention may not remove linked artifact %s: %v", art, err)
			}
		}
		// The uncited diagnostic is not protected by this defect (control: the
		// protection is the defect's, not a blanket one).
		if err := s.ArtStore.BeginRetentionDeletion(ctx, scopeA, scratch, s.X.Clock.Now(), actor); err != nil {
			t.Fatalf("an unlinked artifact stays removable: %v", err)
		}

		// The linked content is the verified bytes the attempt produced.
		meta, err := s.Evidence.Describe(ctx, scopeA, res.Report.ArtifactID)
		if err != nil || meta.Digest != res.Report.ArtifactDigest || meta.Attempt != s.Run.ID() {
			t.Fatalf("report artifact: %+v %v", meta, err)
		}
	})

	// refused completes the attempt with the report and proves nothing was filed.
	refused := func(t *testing.T, s *stack, want error, obs ...qa.Observation) error {
		t.Helper()
		s.complete(obs...)
		_, err := s.record()
		is(t, err, want)
		s.nothingFiled(t, scopeA)
		return err
	}

	t.Run("an artifact of another tenant is refused exactly like an absent one", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		depB := s.Deploy(qatest.ScopeB, "preview")
		runB := s.StartQA(qatest.ScopeB, depB, qatest.QAOptions{Screenshots: 1})
		foreignID, foreignDigest := runB.Screenshot(1)
		err := refused(t, s, qa.ErrEvidence, qatest.Observation("Order form shows a blank page", "/orders/new",
			qatest.Evidence(foreignID, foreignDigest, qa.EvidenceScreenshot, "")))

		s2 := newStack(t, qatest.Options{})
		err2 := refused(t, s2, qa.ErrEvidence, qatest.Observation("Order form shows a blank page", "/orders/new",
			qatest.Evidence("art_00000000000000000000000000000000", foreignDigest, qa.EvidenceScreenshot, "")))
		if err.Error() != err2.Error() {
			t.Fatalf("a foreign artifact is told apart from an absent one:\n%v\n%v", err, err2)
		}
	})

	t.Run("an artifact of another attempt of the same project is refused", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		other := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1})
		id, d := other.Screenshot(1)
		refused(t, s, qa.ErrEvidence, qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(id, d, qa.EvidenceScreenshot, "")))
	})

	t.Run("the deployed image is not evidence of the attempt", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		refused(t, s, qa.ErrEvidence, qatest.Observation("Order form shows a blank page", "/orders/new",
			qatest.Evidence(s.Dep.Flow.Image, s.Dep.Flow.ImageDigest, qa.EvidenceScreenshot, "")))
	})

	t.Run("a stated digest that is not the artifact's is refused", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		refused(t, s, qa.ErrEvidence, qatest.Observation("Order form shows a blank page", "/orders/new",
			qatest.Evidence(s.Shot[0], runtimev1.Digest("sha256:"+strings.Repeat("0", 64)), qa.EvidenceScreenshot, "")))
	})

	t.Run("an artifact the accepted result did not finalize is refused", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		id, d := s.Run.Upload("scratch", artifacts.RoleDiagnostic, "image/png", qatest.PNG("unlisted"))
		refused(t, s, qa.ErrEvidence, qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(id, d, qa.EvidenceScreenshot, "")))
	})

	t.Run("a screenshot that is not an image is refused", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		id, d := s.Run.Diagnostic("notes", "text/plain", []byte("not an image"))
		refused(t, s, qa.ErrEvidence, qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(id, d, qa.EvidenceScreenshot, "")))
	})

	t.Run("an attempt whose accepted result names no declared report maps nothing", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		// The result is committed without the declared report output (the
		// results service would have refused it; the control plane must not
		// depend on that and refuses it again).
		if err := s.Run.Finish(); err != nil {
			t.Fatal(err)
		}
		_, err := s.record()
		is(t, err, qa.ErrNotReady)
		s.nothingFiled(t, scopeA)
	})

	t.Run("the work store's own guard refuses a link to a foreign artifact, and commits", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		res := s.mustRecord(t)
		id := res.Defects[0].ID
		before, _ := s.Work(scopeA, id)

		depB := s.Deploy(qatest.ScopeB, "preview")
		runB := s.StartQA(qatest.ScopeB, depB, qatest.QAOptions{Screenshots: 1})
		foreign, _ := runB.Screenshot(1)
		ctx := context.Background()
		_, added, err := s.Defects.Attach(ctx, scopeA, s.Run.ID(), "req", id, []qa.EvidenceLink{{ArtifactID: foreign, Note: "foreign"}})
		is(t, err, qa.ErrEvidence)
		if added != 0 {
			t.Fatal("a foreign artifact was linked")
		}
		after, _ := s.Work(scopeA, id)
		if after.Version != before.Version || len(after.EvidenceLinks) != len(before.EvidenceLinks) {
			t.Fatal("a refused link changed the defect")
		}

		// A commit link is not QA's to make, even for the bound mapping principal
		// under an upstream authority that would permit the edit: the store's
		// guard fails closed.
		svc, err := work.NewService(work.Config{Repository: s.WorkStore, Authority: grantedUser{}})
		if err != nil {
			t.Fatal(err)
		}
		links := append(append([]work.EvidenceRef{}, after.EvidenceLinks...), work.EvidenceRef{Type: work.EvidenceCommit, ID: strings.Repeat("a", 40)})
		_, err = svc.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: work.Caller{PrincipalID: qatest.Mapper}, ID: id, Expected: after.Version, Patch: work.Patch{EvidenceLinks: &links}})
		is(t, err, work.ErrInvalid)
	})
}
