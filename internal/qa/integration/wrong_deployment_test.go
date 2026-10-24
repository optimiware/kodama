package integration_test

import (
	"context"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/qa/qatest"
)

// Test_qa_wrong_deployment proves DQ-10 (QA identifies the deployment and
// configuration it inspected) and WM-01/AR-05 for the report: the deployment a
// report names must be the deployment module's succeeded record of exactly the
// environment and immutable configuration the QA attempt was pinned to. Every
// other claim, absent or foreign, is one refusal that files nothing.
func Test_qa_wrong_deployment(t *testing.T) {
	// refuse completes the attempt with a report whose deployment identification
	// is edited, then proves the mapping is refused and wrote nothing.
	refuse := func(t *testing.T, s *stack, edit func(d *qa.DeploymentRef)) error {
		t.Helper()
		r := s.Run.ReportFor(s.Dep, s.finding("Order form shows a blank page", "/orders/new", 0))
		edit(&r.Deployment)
		s.Run.Complete(r)
		_, err := s.record()
		is(t, err, qa.ErrWrongDeployment)
		s.nothingFiled(t, scopeA)
		return err
	}

	t.Run("the correct deployment is mapped and its identity is recorded in the defect", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		res := s.mustRecord(t)
		if len(res.Defects) != 1 || !res.Defects[0].Created {
			t.Fatalf("result: %+v", res)
		}
		it, err := s.Work(scopeA, res.Defects[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		facts := s.Dep.Facts()
		for _, want := range []string{"`" + facts.ID + "`", string(facts.ConfigDigest), string(facts.CandidateCommit), string(facts.ImageDigest), string(facts.EnvironmentID)} {
			if !strings.Contains(it.Spec, want) {
				t.Errorf("the defect does not identify the inspected deployment: missing %s", want)
			}
		}
	})

	// trusted starts a second QA attempt of the same work item, pinned to the
	// first environment, whose trusted deployment input is dep, and completes it
	// with a report that states dep's own (correct) facts. Whatever it refuses is
	// refused by the deployment checks, not by the input binding.
	trusted := func(t *testing.T, s *stack, dep *qatest.Deployed) (*qatest.QARun, error) {
		t.Helper()
		run := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1, TrustedDeployment: dep.ID()})
		id, d := run.Screenshot(1)
		run.Complete(run.ReportFor(dep, qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(id, d, qa.EvidenceScreenshot, ""))))
		_, err := s.Service.Record(context.Background(), run.Command())
		return run, err
	}

	// pinned is refuse for a deployment the attempt itself was pinned to
	// (its trusted input names ref.ID) and whose identification the report
	// repeats faithfully: the refusal comes from the deployment module's
	// answer, not from a disagreement between report and input.
	pinned := func(t *testing.T, s *stack, ref qa.DeploymentRef) error {
		t.Helper()
		run := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1, TrustedDeployment: ref.ID})
		id, d := run.Screenshot(1)
		r := run.ReportFor(s.Dep, qatest.Observation("Order form shows a blank page", "/orders/new", qatest.Evidence(id, d, qa.EvidenceScreenshot, "")))
		r.Deployment = ref
		run.Complete(r)
		_, err := s.Service.Record(context.Background(), run.Command())
		is(t, err, qa.ErrWrongDeployment)
		s.nothingFiled(t, scopeA)
		return err
	}

	t.Run("an unknown deployment", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		refuse(t, s, func(d *qa.DeploymentRef) { d.ID = "dep_00000000000000000000000000000000" })
		ref := s.Dep.Facts()
		ref.ID = "dep_00000000000000000000000000000000"
		pinned(t, s, ref)
	})

	t.Run("a deployment of another project of the same tenant", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.Kubeconfig(qatest.ScopeA2, "kubeconfig", "KUBE-A2")
		other := s.DeployTo(qatest.ScopeA2, s.Environment(qatest.ScopeA2, "preview", "kubeconfig"), deliverytest.FlowOptions{}, true)
		refuse(t, s, func(d *qa.DeploymentRef) { d.ID = other.ID() })
		pinned(t, s, other.Facts())
	})

	t.Run("a deployment of another tenant", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		foreign := s.Deploy(qatest.ScopeB, "preview")
		err := refuse(t, s, func(d *qa.DeploymentRef) { d.ID = foreign.ID() })
		pinnedErr := pinned(t, s, foreign.Facts())
		// A foreign deployment is indistinguishable from an absent one.
		s2 := newStack(t, qatest.Options{})
		err2 := refuse(t, s2, func(d *qa.DeploymentRef) { d.ID = "dep_00000000000000000000000000000000" })
		absent := s2.Dep.Facts()
		absent.ID = "dep_00000000000000000000000000000000"
		pinnedErr2 := pinned(t, s2, absent)
		if err.Error() != err2.Error() || pinnedErr.Error() != pinnedErr2.Error() {
			t.Fatalf("a foreign deployment is told apart from an absent one:\n%v\n%v\n%v\n%v", err, err2, pinnedErr, pinnedErr2)
		}
	})

	t.Run("a report naming another environment than the attempt was pinned to", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		refuse(t, s, func(d *qa.DeploymentRef) { d.EnvironmentID = "env_other" })
	})

	t.Run("a deployment of another environment of the same project", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		env2 := s.Environment(scopeA, "preview-2", "kubeconfig")
		dep2 := s.DeployTo(scopeA, env2, deliverytest.FlowOptions{}, true)
		// The QA attempt is pinned to the first environment; even when the
		// attempt's own input names the second deployment, and the report states
		// that deployment's correct identification, it is not what was pinned.
		_, err := trusted(t, s, dep2)
		is(t, err, qa.ErrWrongDeployment)
		s.nothingFiled(t, scopeA)
	})

	t.Run("a report naming another succeeded deployment than the attempt's pinned input", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		// The same environment and configuration hold a second succeeded
		// deployment, built from another candidate commit.
		other := s.DeployCandidate(scopeA, s.Dep.Env, executiontest.CommitB)
		mine, theirs := s.Dep.Facts(), other.Facts()
		if mine.EnvironmentID != theirs.EnvironmentID || mine.ConfigDigest != theirs.ConfigDigest || mine.CandidateCommit == theirs.CandidateCommit || mine.ID == theirs.ID {
			t.Fatalf("the fixture is not two candidates of one environment and configuration:\n%+v\n%+v", mine, theirs)
		}
		// The report states the other deployment's facts exactly (every field of
		// its own record is true); only the attempt's pinned input says which
		// deployment this attempt was to inspect.
		r := s.Run.ReportFor(other, s.finding("Order form shows a blank page", "/orders/new", 0))
		s.Run.Complete(r)
		_, err := s.record()
		is(t, err, qa.ErrWrongDeployment)
		s.nothingFiled(t, scopeA)

		// Control: an attempt whose pinned input names that deployment maps its
		// report of it, so the refusal above is the binding and not the fixture.
		run, err := trusted(t, s, other)
		if err != nil {
			t.Fatalf("an attempt pinned to the other deployment: %v", err)
		}
		if s.DefectRows(scopeA) != 1 {
			t.Fatalf("defects: %d", s.DefectRows(scopeA))
		}
		_ = run
	})

	t.Run("an attempt without a pinned deployment input maps nothing", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		other, err := qa.NewService(qa.Config{Attempts: s.Attempts, Deployments: s.Deploys, Artifacts: s.Evidence, Defects: s.Defects,
			ReportOutput: qatest.ReportOutput, DeploymentInput: "no-such-input"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = other.Record(context.Background(), s.Run.Command())
		is(t, err, qa.ErrContext)
		s.nothingFiled(t, scopeA)
	})

	t.Run("a stated configuration that is not the pinned one", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		refuse(t, s, func(d *qa.DeploymentRef) {
			d.ConfigDigest = runtimev1.Digest("sha256:" + strings.Repeat("c", 64))
		})
	})

	t.Run("a stated candidate or image that is not the deployment's", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		refuse(t, s, func(d *qa.DeploymentRef) { d.CandidateCommit = runtimev1.GitCommit(strings.Repeat("9", 40)) })
		s = newStack(t, qatest.Options{})
		refuse(t, s, func(d *qa.DeploymentRef) { d.ImageDigest = runtimev1.Digest("sha256:" + strings.Repeat("d", 64)) })
	})

	t.Run("a deployment that failed", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		failed := s.FailedDeployment(scopeA, s.Dep.Env)
		_, err := trusted(t, s, failed)
		is(t, err, qa.ErrWrongDeployment)
		s.nothingFiled(t, scopeA)
	})

	t.Run("a deployment whose apply never settled", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		running := s.DeployTo(scopeA, s.Dep.Env, deliverytest.FlowOptions{}, false)
		_, err := trusted(t, s, running)
		is(t, err, qa.ErrWrongDeployment)
		s.nothingFiled(t, scopeA)
	})

	t.Run("an environment edited after admission does not change what the attempt was pinned to", func(t *testing.T) {
		s := newStack(t, qatest.Options{})
		ctx := context.Background()
		before := s.Run.Pinned().Environment.ConfigDigest
		cfg := map[string]string{"REPLICAS": "7"}
		env, err := s.Svc.GetEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, s.Dep.Env.ID())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, env.ID(), env.Version(), delivery.EnvironmentEdit{Config: &cfg}); err != nil {
			t.Fatal(err)
		}

		// The attempt admitted before the edit still maps its (old) deployment.
		s.complete(s.finding("Order form shows a blank page", "/orders/new", 0))
		if _, err := s.record(); err != nil {
			t.Fatalf("the pin must survive the edit: %v", err)
		}

		// An attempt admitted after the edit is pinned to the new configuration,
		// so the old deployment is no longer what it inspected.
		later := s.StartQAFor(s.Run.Project, s.Dep, qatest.QAOptions{Screenshots: 1})
		if later.Pinned().Environment.ConfigDigest == before {
			t.Fatal("the edit did not change the configuration digest")
		}
		id, d := later.Screenshot(1)
		r := later.ReportFor(s.Dep, qatest.Observation("Totals are wrong", "/orders", qatest.Evidence(id, d, qa.EvidenceScreenshot, "")))
		later.Complete(r)
		_, err = s.Service.Record(ctx, later.Command())
		is(t, err, qa.ErrWrongDeployment)
		if n := s.DefectRows(scopeA); n != 1 {
			t.Fatalf("only the first, correctly bound defect exists: %d", n)
		}
	})
}
