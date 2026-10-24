package adapters_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates/adapters"
	"github.com/optimiware/kodama/internal/pipeline"
)

const (
	commitA = "1111111111111111111111111111111111111111"
	commitB = "2222222222222222222222222222222222222222"
)

var scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

func baseline(t *testing.T, checks ...string) runtimev1.DesignBaseline {
	t.Helper()
	b, err := runtimev1.NewDesignBaseline([]string{"SD-01"}, []string{"C-16"},
		[]runtimev1.SourceRef{{Repository: "https://git.example.test/acme/design.git", Revision: commitA, Path: "docs/specs/software-design.md"}}, checks)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pinned builds the immutable context of an attempt the way the control plane
// pins it: canonical JSON and its digest.
func pinned(t *testing.T, c runtimev1.Context) ([]byte, string) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	return canonical, "sha256:" + hex.EncodeToString(sum[:])
}

type fixture struct {
	agg      *execution.Aggregate
	producer *execution.StepRun
	attempt  *execution.Attempt
	base     runtimev1.DesignBaseline
}

// newFixture is a run whose agent step "implement" succeeded with one attempt
// that pinned a baseline, published commitB and declared a conformance report.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := baseline(t, "module-boundaries", "tenant-authority")
	ctx := runtimev1.Context{TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: "run_1", StepID: "implement", AttemptID: "att_1", DesignBaseline: &base}
	canonical, digest := pinned(t, ctx)
	pub := runtimev1.GitCommit(commitB)
	at := &execution.Attempt{
		Scope: scope, ID: "att_1", RunID: "run_1", StepID: "implement", Ordinal: 1, Status: execution.AttemptSucceeded, InputCommit: commitA,
		Context: canonical, ContextDigest: digest,
		Result: &runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, SourceCommit: &pub, Outputs: map[string]runtimev1.Value{"conformance": runtimev1.StringValue("art_report")}},
	}
	step := execution.NewStepRun("run_1", 0, pipeline.Step{ID: "implement", Kind: pipeline.KindAgent,
		Outputs: map[string]pipeline.Output{"conformance": {Type: pipeline.TypeArtifact}}}, time.Now())
	step.Status, step.OutputCommit = execution.StepSucceeded, commitB
	step.Outputs = map[string]runtimev1.Value{"conformance": runtimev1.StringValue("art_report")}
	agg := &execution.Aggregate{Run: &execution.Run{Scope: scope, ID: "run_1"}, Steps: []*execution.StepRun{step}, Attempts: []*execution.Attempt{at}}
	return &fixture{agg: agg, producer: step, attempt: at, base: base}
}

func (f *fixture) expect(t *testing.T) (got struct {
	baseline *runtimev1.DesignBaseline
	report   domain.ArtifactID
	attempt  domain.AttemptID
	commit   string
}, err error) {
	t.Helper()
	exp, err := adapters.ConformanceMapping{ReportOutput: "conformance"}.Expected(context.Background(), f.agg, execution.GateCandidate{Commit: commitB, Producer: "implement", ProducerKind: pipeline.KindAgent})
	got.baseline, got.report, got.attempt, got.commit = exp.Baseline, exp.ReportID, exp.ProducerAttempt, exp.PublishedCommit
	return got, err
}

// TestConformanceMappingPinnedContext (DQ-14, SD-08): the expected baseline and
// required checks come from the successful attempt's pinned context, the report
// from its declared and accepted output, the commit from its accepted
// publication; never from the report or from anything the agent claims.
// Ambiguous or inconsistent records fail closed.
func TestConformanceMappingPinnedContext(t *testing.T) {
	t.Run("pinned data", func(t *testing.T) {
		f := newFixture(t)
		got, err := f.expect(t)
		if err != nil {
			t.Fatal(err)
		}
		if got.baseline == nil || got.baseline.Digest != f.base.Digest || len(got.baseline.RequiredChecks) != 2 {
			t.Fatalf("baseline %+v", got.baseline)
		}
		if got.report != "art_report" || got.attempt != "att_1" || got.commit != commitB {
			t.Fatalf("expectation %+v", got)
		}
	})

	t.Run("no pinned baseline stays absent", func(t *testing.T) {
		f := newFixture(t)
		canonical, digest := pinned(t, runtimev1.Context{TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: "run_1", StepID: "implement", AttemptID: "att_1"})
		f.attempt.Context, f.attempt.ContextDigest = canonical, digest
		got, err := f.expect(t)
		if err != nil || got.baseline != nil {
			t.Fatalf("baseline %+v err %v", got.baseline, err)
		}
	})

	t.Run("an undeclared or unaccepted report output identifies nothing", func(t *testing.T) {
		f := newFixture(t)
		delete(f.producer.Spec.Outputs, "conformance")
		if got, err := f.expect(t); err != nil || got.report != "" {
			t.Fatalf("undeclared: %q %v", got.report, err)
		}
		f = newFixture(t)
		delete(f.attempt.Result.Outputs, "conformance")
		if got, err := f.expect(t); err != nil || got.report != "" {
			t.Fatalf("not in the accepted result: %q %v", got.report, err)
		}
		f = newFixture(t)
		f.attempt.Result.Outputs["conformance"] = runtimev1.StringValue("art_other")
		if got, err := f.expect(t); err != nil || got.report != "" {
			t.Fatalf("finalized output differs from the accepted result: %q %v", got.report, err)
		}
		f = newFixture(t)
		f.producer.Spec.Outputs["conformance"] = pipeline.Output{Type: pipeline.TypeString}
		if got, err := f.expect(t); err != nil || got.report != "" {
			t.Fatalf("not an artifact output: %q %v", got.report, err)
		}
	})

	for name, mutate := range map[string]func(f *fixture){
		"producer did not succeed": func(f *fixture) { f.producer.Status = execution.StepFailed },
		"no successful attempt":    func(f *fixture) { f.attempt.Status = execution.AttemptFailed },
		"several successful attempts": func(f *fixture) {
			dup := *f.attempt
			dup.ID, dup.Ordinal = "att_2", 2
			f.agg.Attempts = append(f.agg.Attempts, &dup)
		},
		"no accepted publication": func(f *fixture) { f.attempt.Result.SourceCommit = nil },
		"no accepted result":      func(f *fixture) { f.attempt.Result = nil },
		"context altered after pinning": func(f *fixture) {
			f.attempt.Context = append([]byte(nil), f.attempt.Context...)
			f.attempt.Context[len(f.attempt.Context)-2] ^= 1
		},
		"context digest cleared": func(f *fixture) { f.attempt.ContextDigest = "" },
		"context of another attempt": func(f *fixture) {
			canonical, digest := pinned(t, runtimev1.Context{TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: "run_1", StepID: "implement", AttemptID: "att_other", DesignBaseline: &f.base})
			f.attempt.Context, f.attempt.ContextDigest = canonical, digest
		},
		"context of another project": func(f *fixture) {
			canonical, digest := pinned(t, runtimev1.Context{TenantID: scope.TenantID, ProjectID: "p_other", RunID: "run_1", StepID: "implement", AttemptID: "att_1", DesignBaseline: &f.base})
			f.attempt.Context, f.attempt.ContextDigest = canonical, digest
		},
		"unreadable context": func(f *fixture) {
			f.attempt.Context = []byte("{")
			sum := sha256.Sum256(f.attempt.Context)
			f.attempt.ContextDigest = "sha256:" + hex.EncodeToString(sum[:])
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(f)
			got, err := f.expect(t)
			if err == nil {
				t.Fatalf("an inconsistent record produced an expectation: %+v", got)
			}
			if got.baseline != nil || got.report != "" {
				t.Fatalf("a partial expectation leaked: %+v", got)
			}
		})
	}

	t.Run("an unknown producer step", func(t *testing.T) {
		f := newFixture(t)
		f.agg.Steps = nil
		if _, err := f.expect(t); err == nil {
			t.Fatal("expectation for a step that is not in the run")
		}
	})

	t.Run("no report output configured", func(t *testing.T) {
		f := newFixture(t)
		exp, err := adapters.ConformanceMapping{}.Expected(context.Background(), f.agg, execution.GateCandidate{Commit: commitB, Producer: "implement", ProducerKind: pipeline.KindAgent})
		if err != nil || exp.ReportID != "" {
			t.Fatalf("%+v %v", exp, err)
		}
	})
}
