package postgres_test

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const secretEnv = `env: {TOKEN: {secretRef: "secret://project/token"}}`

// Test_admission_pins: PX-02, PX-01, PX-A01. Input precedence and type
// rejection, specification-edit races, secret revocation races, immutable
// snapshots and atomic secret-pin registration.
func Test_admission_pins(t *testing.T) {
	ctx := context.Background()

	t.Run("precedence: default, project variable, run input", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.DocNamed("prec", "{region: {type: string, default: eu}}", job("a", readRun, "timeoutSeconds: 60")))
		source := func(r *execution.Run) (string, string) {
			in, _ := inputByName(r, "region")
			for _, pin := range r.Snapshot.Inputs {
				if pin.Name == "region" {
					return in.String(), pin.Source
				}
			}
			return "", ""
		}
		if v, src := source(e.MustAdmit(p, def, nil)); v != "eu" || src != "default" {
			t.Fatalf("default: %s from %s", v, src)
		}
		e.Variable(p.Scope, "region", "us")
		if v, src := source(e.MustAdmit(p, def, nil)); v != "us" || src != "project_variable" {
			t.Fatalf("variable: %s from %s", v, src)
		}
		explicit := map[string]runtimev1.Value{"region": runtimev1.StringValue("ap")}
		if v, src := source(e.MustAdmit(p, def, explicit)); v != "ap" || src != "run_input" {
			t.Fatalf("run input: %s from %s", v, src)
		}
		// The variable's exact version is recorded with its provenance.
		r := e.MustAdmit(p, def, nil)
		for _, pin := range r.Snapshot.Inputs {
			if pin.Name == "region" && (pin.VariableID == "" || pin.VariableVersion < 1) {
				t.Fatalf("variable provenance missing: %+v", pin)
			}
		}
	})

	t.Run("invalid inputs, kinds and sources fail before any state exists", func(t *testing.T) {
		e := executiontest.New(t, executiontest.Options{Kinds: []pipeline.StepKind{pipeline.KindContainer}})
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.DocNamed("typed", "{count: {type: integer, required: true}}", job("a", readRun, "timeoutSeconds: 60")))
		gate := e.Define(p.Scope, pipelinetest.Doc(job("a", readRun, "timeoutSeconds: 60"), "{id: g, kind: gate, needs: [a]}"))
		for name, run := range map[string]func() error{
			"missing required input": func() error { _, err := e.Admit(p, def, nil); return err },
			"type mismatch": func() error {
				_, err := e.Admit(p, def, map[string]runtimev1.Value{"count": runtimev1.StringValue("seven")})
				return err
			},
			"unknown input": func() error {
				_, err := e.Admit(p, def, map[string]runtimev1.Value{"count": runtimev1.IntValue(1), "nope": runtimev1.IntValue(1)})
				return err
			},
			"disabled step kind": func() error { _, err := e.Admit(p, gate, nil); return err },
			"commit outside the workspace history": func() error {
				_, err := e.Svc.Admit(ctx, engineer, execution.AdmitCommand{Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID,
					ContentRevision: e.ContentRevision(p), SourceCommit: executiontest.CommitC, Inputs: map[string]runtimev1.Value{"count": runtimev1.IntValue(1)}})
				return err
			},
		} {
			if err := run(); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if e.Count(`SELECT count(*) FROM runs`) != 0 || e.Count(`SELECT count(*) FROM run_history`) != 0 {
			t.Fatal("a rejected admission left state")
		}
		if _, err := e.Svc.Admit(ctx, engineer, execution.AdmitCommand{Scope: p.Scope, WorkItemID: p.Item, DefinitionID: "pd_missing",
			ContentRevision: e.ContentRevision(p), SourceCommit: executiontest.CommitA}); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("unknown definition: %v", err)
		}
	})

	t.Run("a specification edit during admission is a conflict, never a stale pin", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("a", readRun, "timeoutSeconds: 60")))
		revision := e.ContentRevision(p)
		old := e.MustAdmit(p, def, nil)
		plan, err := e.Svc.Prepare(ctx, engineer, execution.AdmitCommand{Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: revision, SourceCommit: executiontest.CommitA})
		if err != nil {
			t.Fatal(err)
		}
		e.EditSpec(p.Scope, p.Item, "# Plan\nChanged while the run was being admitted.")
		before := e.Count(`SELECT count(*) FROM runs`)
		if _, _, err := e.Store.Admit(ctx, plan); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("racing admission: %v", err)
		}
		if e.Count(`SELECT count(*) FROM runs`) != before {
			t.Fatal("a conflicting admission created a run")
		}
		// A request naming the old revision is refused outright.
		if _, err := e.Svc.Admit(ctx, engineer, execution.AdmitCommand{Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: revision, SourceCommit: executiontest.CommitA}); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("old revision: %v", err)
		}
		// The run admitted earlier keeps its immutable snapshot.
		if got := e.Get(old).Run; got.Specification.Spec != "# Plan\nDo the thing." || got.ContentRevision != revision {
			t.Fatalf("pinned specification changed: %q at %d", got.Specification.Spec, got.ContentRevision)
		}
		fresh := e.MustAdmit(p, def, nil)
		if fresh.ContentRevision != revision+1 || fresh.Specification.Spec == old.Specification.Spec {
			t.Fatalf("the new run did not pin the new specification (%d)", fresh.ContentRevision)
		}
	})

	t.Run("secret pins: exact versions, per-step bindings, revocation races", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		sec := e.Secret(executiontest.ScopeA, "token", "v1-value")
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(
			job("one", readRun, "timeoutSeconds: 60", secretEnv),
			job("two", readRun, "timeoutSeconds: 60", secretEnv, `secretFiles: {cert: "secret://project/token"}`),
		))
		run := e.MustAdmit(p, def, nil)
		// The same binding name in two steps is not flattened into one namespace.
		var env1, env2, files int
		for _, s := range run.Snapshot.Secrets {
			if s.Binding.Name == "TOKEN" && s.Step == "one" {
				env1++
			}
			if s.Binding.Name == "TOKEN" && s.Step == "two" {
				env2++
			}
			if s.Binding.Name == "cert" && s.Step == "two" && s.Binding.Delivery == runtimev1.DeliveryFile {
				files++
			}
			if s.Binding.Version != 1 {
				t.Fatalf("pinned version %d", s.Binding.Version)
			}
		}
		if env1 != 1 || env2 != 1 || files != 1 || len(run.Snapshot.Secrets) != 3 {
			t.Fatalf("bindings: %+v", run.Snapshot.Secrets)
		}
		if n := e.Count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1 AND secret_version = 1 AND released_at IS NULL`, string(run.ID)); n != 1 {
			t.Fatalf("%d pin rows for the run, want one per distinct version", n)
		}
		// Rotation never changes a pinned run; a new run pins the new version.
		e.RotateSecret(p.Scope, sec.ID(), "v2-value")
		if r2 := e.MustAdmit(p, def, nil); r2.Snapshot.Secrets[0].Binding.Version != 2 {
			t.Fatalf("new run pinned %d", r2.Snapshot.Secrets[0].Binding.Version)
		}
		if got := e.Get(run).Run; got.Snapshot.Secrets[0].Binding.Version != 1 {
			t.Fatal("rotation rewrote a pinned run")
		}
		// Revocation committed between resolution and admission is refused, and
		// registers no pin.
		plan, err := e.Svc.Prepare(ctx, engineer, execution.AdmitCommand{Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.ContentRevision(p), SourceCommit: executiontest.CommitA})
		if err != nil {
			t.Fatal(err)
		}
		pins := e.Count(`SELECT count(*) FROM secret_pins`)
		runs := e.Count(`SELECT count(*) FROM runs`)
		if err := e.RevokeSecret(p.Scope, sec.ID(), 2); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.Store.Admit(ctx, plan); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("admission racing a revocation: %v", err)
		}
		if e.Count(`SELECT count(*) FROM secret_pins`) != pins || e.Count(`SELECT count(*) FROM runs`) != runs {
			t.Fatal("a refused admission left a run or a pin")
		}
		// And a request after the revocation is refused at resolution.
		if _, err := e.Admit(p, def, nil); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("admission after revocation: %v", err)
		}
	})

	t.Run("the pinned snapshot holds no plaintext", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		e.Secret(executiontest.ScopeA, "token", "PLAINTEXT-SENTINEL")
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("one", readRun, "timeoutSeconds: 60", secretEnv)))
		run := e.MustAdmit(p, def, nil)
		var inputs, steps string
		e.Scalar(&inputs, `SELECT inputs::text FROM runs WHERE id = $1`, string(run.ID))
		e.Scalar(&steps, `SELECT string_agg(spec::text, '') FROM step_runs WHERE run_id = $1`, string(run.ID))
		var dump string
		e.Scalar(&dump, `SELECT coalesce(string_agg(metadata::text, ''), '') FROM audit`)
		for _, s := range []string{inputs, steps, dump} {
			if contains(s, "PLAINTEXT-SENTINEL") {
				t.Fatal("secret plaintext reached execution state")
			}
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
