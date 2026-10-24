// Package environmentpinningruntime_test qualifies the environment and
// credential pin of an admitted run over the REAL persistence (task T-067,
// transferred from T-077): real execution, secrets, delivery and runtime-
// authority PostgreSQL stores read and written by the non-owner runtime role
// under forced row-level security, the real delivery <-> execution adapters
// (environment resolver, transaction-bound binding, retained snapshots), real
// credential edits and rotations, failure rollback and restarts.
//
// Controlled doubles are only the fixtures the underlying modules document: the
// workload runtime (Manual), the OCI registry (a bounded HTTP fixture the real
// OCI verifier fetches from) and Git observation. Unit policy evidence does not
// satisfy these checks.
//
// Resource prefix t067.
package environmentpinningruntime_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

var (
	scopeA  = deliverytest.ScopeA
	scopeA2 = deliverytest.ScopeA2
	scopeB  = deliverytest.ScopeB
)

const (
	namespace      = "t067-pin"
	kubeconfigV1   = "KUBECONFIG-SENTINEL-V1"
	kubeconfigV2   = "KUBECONFIG-SENTINEL-V2"
	deployStepYAML = "{id: deploy, kind: container, workspace: {access: read, from: run}, environment: %s, image: " + pipelinetest.Image + ", command: [\"true\"], timeoutSeconds: 60}"
)

// world is the assembled stack with one project's staging environment bound to
// a kubeconfig secret, and a definition whose step declares the environment.
type world struct {
	*stack.Stack
	secret  secrets.Secret
	env     delivery.Environment
	project executiontest.Project
	def     pipeline.Definition
}

func newWorld(t *testing.T, o stack.Options) *world {
	t.Helper()
	s := stack.New(t, o)
	w := &world{Stack: s}
	w.secret = s.Kubeconfig(scopeA, "kubeconfig", kubeconfigV1)
	w.env = s.Environment(scopeA, stack.EnvSpec{Name: "staging", Namespace: namespace, Kubeconfig: "kubeconfig"})
	w.project = s.X.NewProject(scopeA)
	w.def = s.X.Define(scopeA, pipelinetest.Doc(fmt.Sprintf(deployStepYAML, w.env.ID())))
	return w
}

func stackOptions() stack.Options { return stack.Options{} }

// docWith is a one-step definition declaring the environment.
func docWith(env string) string { return pipelinetest.Doc(fmt.Sprintf(deployStepYAML, env)) }

func (w *world) command() execution.AdmitCommand {
	return execution.AdmitCommand{Scope: scopeA, WorkItemID: w.project.Item, DefinitionID: w.def.ID,
		ContentRevision: w.X.ContentRevision(w.project), SourceCommit: executiontest.CommitA}
}

func caller() execution.Caller {
	return execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t067"}
}

func (w *world) prepare(t *testing.T) *execution.AdmissionPlan {
	t.Helper()
	plan, err := w.Exec.Prepare(context.Background(), caller(), w.command())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return plan
}

func (w *world) admit(t *testing.T) *execution.Run {
	t.Helper()
	run, err := w.Exec.Admit(context.Background(), caller(), w.command())
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return run
}

// tables are the relations an admission writes; a failed admission must leave
// every one exactly as it was.
var tables = []string{"runs", "step_runs", "run_history", "secret_pins", "workspace_retained_refs", "audit", "outbox"}

func (w *world) counts() map[string]int {
	out := map[string]int{}
	for _, table := range tables {
		out[table] = w.Count("SELECT count(*) FROM " + table)
	}
	return out
}

func (w *world) requireUnchanged(t *testing.T, before map[string]int, what string) {
	t.Helper()
	for table, n := range before {
		if after := w.Count("SELECT count(*) FROM " + table); after != n {
			t.Errorf("%s changed %s: %d -> %d", what, table, n, after)
		}
	}
}

func (w *world) snapshots() int {
	return w.Count(`SELECT count(*) FROM environment_snapshots WHERE tenant_id = $1 AND project_id = $2 AND environment_id = $3`,
		string(scopeA.TenantID), string(scopeA.ProjectID), string(w.env.ID()))
}

func (w *world) runPins(run domain.RunID, version int64) int {
	return w.Count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1 AND secret_version = $2 AND released_at IS NULL`, string(run), version)
}

// claimContext dispatches the next runnable step and returns its persisted,
// pinned runtime context.
func (w *world) claimContext(t *testing.T, run *execution.Run) runtimev1.Context {
	t.Helper()
	in := w.X.MustClaim(run.Scope.TenantID)
	if in.RunID != run.ID {
		t.Fatalf("claimed %s, wanted %s", in.RunID, run.ID)
	}
	c, err := runtimev1.DecodeRequest[runtimev1.Context](in.Context)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(in.Context), "SENTINEL") {
		t.Fatal("credential plaintext reached the runtime context")
	}
	return c
}

// pinDigest returns the environment pin of the run's only step.
func pinOf(t *testing.T, run *execution.Run) runtimev1.EnvironmentPin {
	t.Helper()
	if len(run.Snapshot.Environments) != 1 {
		t.Fatalf("the run pins %d environments, want 1", len(run.Snapshot.Environments))
	}
	return run.Snapshot.Environments[0].Pin
}
