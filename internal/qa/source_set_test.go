package qa_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/qa"
)

const (
	srcBack  runtimev1.GitCommit = "5555555555555555555555555555555555555555"
	srcFront runtimev1.GitCommit = "6666666666666666666666666666666666666666"
	srcHead  runtimev1.GitCommit = "7777777777777777777777777777777777777777"
	srcDig   runtimev1.Digest    = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// specSpy records the specification of every defect the mapping files.
type specSpy struct {
	*uDefects
	specs []string
}

func (s *specSpy) Open(ctx context.Context, d qa.DefectDraft) (qa.Defect, bool, error) {
	s.specs = append(s.specs, d.Spec)
	return s.uDefects.Open(ctx, d)
}

func (w *world) spyService(t *testing.T) (*qa.Service, *specSpy) {
	t.Helper()
	spy := &specSpy{uDefects: (*uDefects)(w)}
	s, err := qa.NewService(qa.Config{Attempts: (*uAttempts)(w), Deployments: (*uDeployments)(w), Artifacts: (*uArtifacts)(w), Defects: spy, ReportOutput: "qa-report", DeploymentInput: "deployment"})
	if err != nil {
		t.Fatal(err)
	}
	return s, spy
}

// sourceSetOf is the two-checkout set of a context v3 QA attempt: the backend
// pinned at srcBack and advanced to commitA by the run's writer (the commit the
// deployment was built from), and a read-only frontend.
func sourceSetOf() qa.SourceSet {
	return qa.SourceSet{Digest: srcDig, Sources: []qa.Source{
		{RepositoryID: "repo_01", Path: "backend", Commit: srcBack, HeadCommit: commitA, Access: "write"},
		{RepositoryID: "repo_02", Path: "frontend", Commit: srcFront, HeadCommit: srcFront, Access: "read"},
	}}
}

// Test_unit_qa_context_source_set: the QA mapping reads the source set from the
// attempt's frozen context and from nowhere else (DQ-10, AR-05, ADR 0011). A
// context without one maps exactly as before; with one, the defect names every
// checkout that was inspected, the reported candidate must be a commit of that
// set, and a malformed or misplaced set is a context refusal that writes nothing.
func Test_unit_qa_context_source_set(t *testing.T) {
	t.Run("a context without a source set maps exactly as before", func(t *testing.T) {
		w := newWorld(t)
		s, spy := w.spyService(t)
		if _, err := s.Record(context.Background(), cmd(system)); err != nil {
			t.Fatal(err)
		}
		if len(spy.specs) != 2 {
			t.Fatalf("specs: %d", len(spy.specs))
		}
		for _, spec := range spy.specs {
			if strings.Contains(spec, "Source set") || !strings.Contains(spec, "Candidate commit: `"+string(commitA)+"`") {
				t.Fatalf("a v1 specification changed:\n%s", spec)
			}
		}
	})

	t.Run("the provenance names every checkout of the pinned source set", func(t *testing.T) {
		w := newWorld(t)
		w.facts.ContextVersion, w.facts.Sources = 3, sourceSetOf()
		s, spy := w.spyService(t)
		if _, err := s.Record(context.Background(), cmd(system)); err != nil {
			t.Fatal(err)
		}
		for _, spec := range spy.specs {
			for _, want := range []string{
				"Source set: `" + string(srcDig) + "`",
				"`backend` at `" + string(commitA) + "` (repository `repo_01`, write, pinned `" + string(srcBack) + "`)",
				"`frontend` at `" + string(srcFront) + "` (repository `repo_02`, read, pinned `" + string(srcFront) + "`)",
			} {
				if !strings.Contains(spec, want) {
					t.Fatalf("the specification lacks %q:\n%s", want, spec)
				}
			}
		}
	})

	t.Run("the candidate may be any pinned or advanced commit of the set", func(t *testing.T) {
		for name, c := range map[string]runtimev1.GitCommit{"advanced head": commitA, "pinned input": srcBack, "other source": srcFront} {
			w := newWorld(t)
			w.facts.ContextVersion, w.facts.Sources = 3, sourceSetOf()
			w.report.Deployment.CandidateCommit = c
			w.publish()
			d := w.deployments["dep_1"]
			d.Candidate = c
			w.deployments["dep_1"] = d
			if _, err := w.service(t).Record(context.Background(), cmd(system)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})

	t.Run("a commit an earlier attempt of the run published is held too", func(t *testing.T) {
		w := newWorld(t)
		set := sourceSetOf()
		set.Published = []qa.Published{{RepositoryID: "repo_01", Commit: srcHead}} // an intermediate commit, no longer any head
		w.facts.ContextVersion, w.facts.Sources = 3, set
		w.report.Deployment.CandidateCommit = srcHead
		w.publish()
		d := w.deployments["dep_1"]
		d.Candidate = srcHead
		w.deployments["dep_1"] = d
		if _, err := w.service(t).Record(context.Background(), cmd(system)); err != nil {
			t.Fatal(err)
		}
		if !set.Holds(srcHead) || sourceSetOf().Holds(srcHead) {
			t.Fatal("Holds must include exactly the lineage's published commits")
		}
	})

	refusals := []struct {
		name string
		mut  func(w *world)
	}{
		{"a candidate that is no commit of the set", func(w *world) {
			w.report.Deployment.CandidateCommit = srcHead
			w.publish()
			d := w.deployments["dep_1"]
			d.Candidate = srcHead // the deployment module agrees with the report: the set still does not hold it
			w.deployments["dep_1"] = d
		}},
		{"a source set on a context that is not version 3", func(w *world) { w.facts.ContextVersion = 1 }},
		{"a source set on a context of version 2", func(w *world) { w.facts.ContextVersion = 2 }},
		{"a set without a digest", func(w *world) { w.facts.Sources.Digest = "" }},
		{"a set with a malformed digest", func(w *world) { w.facts.Sources.Digest = "sha256:abc" }},
		{"a set out of path order", func(w *world) {
			s := w.facts.Sources.Sources
			s[0], s[1] = s[1], s[0]
		}},
		{"a source with a hidden path", func(w *world) { w.facts.Sources.Sources[0].Path = ".git" }},
		{"a source that escapes the workspace", func(w *world) { w.facts.Sources.Sources[0].Path = "a/../b" }},
		{"a source with a malformed commit", func(w *world) { w.facts.Sources.Sources[1].HeadCommit = "main" }},
		{"a source with an unknown access", func(w *world) { w.facts.Sources.Sources[1].Access = "admin" }},
		{"a published commit of a repository outside the set", func(w *world) {
			w.facts.Sources.Published = []qa.Published{{RepositoryID: "repo_99", Commit: srcHead}}
		}},
		{"a published commit that is no commit", func(w *world) {
			w.facts.Sources.Published = []qa.Published{{RepositoryID: "repo_01", Commit: "main"}}
		}},
		{"a set of digest only", func(w *world) { w.facts.Sources.Sources = nil }},
	}
	for _, c := range refusals {
		t.Run("refused: "+c.name, func(t *testing.T) {
			w := newWorld(t)
			w.facts.ContextVersion, w.facts.Sources = 3, sourceSetOf()
			c.mut(w)
			_, err := w.service(t).Record(context.Background(), cmd(system))
			if !errors.Is(err, qa.ErrContext) {
				t.Fatalf("got %v, want a context refusal", err)
			}
			if len(w.pins) != 0 || len(w.defects) != 0 || w.opens != 0 {
				t.Fatalf("a refused context wrote: pins=%v defects=%d", w.pins, len(w.defects))
			}
		})
	}

	t.Run("the report cannot supply the set: the candidate check never loosens", func(t *testing.T) {
		// A report claiming a candidate outside the set is refused even when every
		// other check (deployment, subject, evidence) would pass: the set is read
		// from the context, never from the report.
		w := newWorld(t)
		w.facts.ContextVersion, w.facts.Sources = 3, sourceSetOf()
		w.report.Deployment.CandidateCommit = srcHead
		w.publish()
		if _, err := w.service(t).Record(context.Background(), cmd(system)); !errors.Is(err, qa.ErrContext) {
			t.Fatalf("got %v", err)
		}
		if len(w.defects) != 0 {
			t.Fatal("the refusal wrote a defect")
		}
	})

	t.Run("the other pinned-context requirements still hold for a source-set attempt", func(t *testing.T) {
		w := newWorld(t)
		w.facts.ContextVersion, w.facts.Sources = 3, sourceSetOf()
		w.facts.Context.WorkItem = nil
		if _, err := w.service(t).Record(context.Background(), cmd(system)); !errors.Is(err, qa.ErrContext) {
			t.Fatalf("a push-origin attempt has no work item to file defects under: %v", err)
		}
		w = newWorld(t)
		w.facts.ContextVersion, w.facts.Sources = 3, sourceSetOf()
		w.facts.Completed = false
		if _, err := w.service(t).Record(context.Background(), cmd(system)); !errors.Is(err, qa.ErrNotReady) {
			t.Fatalf("got %v", err)
		}
	})
}
