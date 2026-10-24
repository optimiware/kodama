package kube_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// stepSpec describes the one container step of a test run.
type stepSpec struct {
	Script  string
	Timeout int
	Outputs []string
	// Retry adds `retry: {maxAttempts: n, on: [infrastructure]}`.
	RetryMax int
	// Image overrides the workload image (for example an unpullable one).
	Image string
}

// definition registers a single-step pipeline running the script through the
// entrypoint in the workload image.
func (s *stack) definition(scope domain.Scope, name string, st stepSpec) pipeline.Definition {
	s.t.Helper()
	if st.Timeout == 0 {
		st.Timeout = 120
	}
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: %s}\nspec:\n  inputs: {}\n  steps:\n", name)
	img := st.Image
	if img == "" {
		img = s.c.workload
	}
	fmt.Fprintf(&b, "    - id: work\n      kind: container\n      image: %s\n      workspace: {access: none}\n      command: [\"/bin/sh\", \"-ec\"]\n", img)
	b.WriteString("      args:\n        - |\n")
	for _, l := range strings.Split(strings.TrimRight(st.Script, "\n"), "\n") {
		b.WriteString("          " + l + "\n")
	}
	fmt.Fprintf(&b, "      timeoutSeconds: %d\n", st.Timeout)
	if st.RetryMax > 1 {
		fmt.Fprintf(&b, "      retry: {maxAttempts: %d, backoffSeconds: 1, on: [infrastructure]}\n", st.RetryMax)
	}
	if len(st.Outputs) > 0 {
		b.WriteString("      outputs:\n")
		for _, o := range st.Outputs {
			fmt.Fprintf(&b, "        %s: {type: string}\n", o)
		}
	}
	return s.e.Define(scope, b.String())
}

// submit admits a run of the script for the scope's project. The run is an
// internal cleanup run (project-scoped, workspace access none, bound to a
// recorded deployment): the entrypoint then needs no Git checkout, and nothing
// else in execution, runtime admission or the executor differs from a work run.
func (s *stack) submit(scope domain.Scope, st stepSpec) (*execution.Run, struct{}) {
	s.t.Helper()
	n := s.ids.Add(1)
	def := s.definition(scope, fmt.Sprintf("t089-%d", n), st)
	run, err := s.svc.AdmitCleanup(context.Background(), execution.CleanupCommand{Scope: scope, DeploymentID: fmt.Sprintf("dep_t089_%d", n), DefinitionID: def.ID})
	must(s.t, err)
	return run, struct{}{}
}

// claim claims the next runnable step of the tenant.
func (s *stack) claim(tenant domain.TenantID) *execution.AttemptSpec {
	s.t.Helper()
	in, err := s.svc.ClaimRunnable(context.Background(), tenant)
	if err != nil {
		s.t.Fatalf("claim: %v", err)
	}
	return in
}

// tick is one iteration of the periodic loops T-027 schedules: execution's own
// reconciliation (deadlines, redrive, termination, settlement) and the
// Kubernetes reconciliation pass.
func (s *stack) tick() {
	s.t.Helper()
	ctx := context.Background()
	if _, err := s.svc.Reconcile(ctx); err != nil {
		s.t.Logf("execution reconcile: %v", err)
	}
	if _, err := s.rec.Pass(ctx); err != nil {
		s.t.Logf("kube pass: %v", err)
	}
}

func (s *stack) attempt(in *execution.AttemptSpec) (*execution.Attempt, *execution.Control) {
	s.t.Helper()
	agg, err := s.e.Store.Get(context.Background(), in.Scope, in.RunID)
	must(s.t, err)
	at := agg.Attempt(in.AttemptID)
	if at == nil {
		s.t.Fatal("attempt not found")
	}
	return at, agg.Control(in.AttemptID)
}

func (s *stack) run(in *execution.AttemptSpec) *execution.Aggregate {
	agg, err := s.e.Store.Get(context.Background(), in.Scope, in.RunID)
	must(s.t, err)
	return agg
}

// until ticks the loops until cond holds.
func (s *stack) until(timeout time.Duration, what string, cond func() (bool, string)) {
	s.t.Helper()
	eventually(s.t, timeout, what, func() (bool, string) {
		if ok, _ := cond(); ok {
			return true, ""
		}
		s.tick()
		return cond()
	})
}

func (s *stack) attemptStatus(in *execution.AttemptSpec) execution.AttemptStatus {
	at, _ := s.attempt(in)
	return at.Status
}

func (s *stack) ns(tenant domain.TenantID) string {
	if tenant == executiontest.ScopeA.TenantID {
		return s.nsA
	}
	return s.nsB
}

func (s *stack) jobOf(in *execution.AttemptSpec) (kubernetes.Job, bool) {
	j, err := s.c.client.GetJob(context.Background(), s.ns(in.Scope.TenantID), in.WorkloadName)
	if errors.Is(err, kubernetes.ErrNotFound) {
		return kubernetes.Job{}, false
	}
	must(s.t, err)
	return j, true
}

func (s *stack) podsOf(in *execution.AttemptSpec) []kubernetes.Pod {
	pods, err := s.c.client.ListPods(context.Background(), s.ns(in.Scope.TenantID), kube.LabelJob+"="+in.WorkloadName)
	must(s.t, err)
	return pods
}

func (s *stack) secretExists(in *execution.AttemptSpec) bool {
	_, err := s.c.client.GetSecret(context.Background(), s.ns(in.Scope.TenantID), kube.SecretName(in.WorkloadName))
	return err == nil
}

func strOutput(t *testing.T, agg *execution.Aggregate, name string) string {
	t.Helper()
	step := agg.Step("work")
	v, ok := step.Outputs[name]
	if !ok {
		t.Fatalf("output %q missing (step %s): %v", name, step.Status, step.Outputs)
	}
	return v.String()
}

var _ = runtimev1.StringValue

// quick is a script whose command completes at once with one output.
const quick = `printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`

// slow completes after n seconds.
func slow(n int) string {
	return fmt.Sprintf("sleep %d\n%s", n, quick)
}

func (s *stack) observeDispatched(in *execution.AttemptSpec) {
	s.t.Helper()
	must(s.t, s.svc.ObserveAttempt(context.Background(), in.Scope, in.AttemptID, execution.Observation{
		Kind: execution.ObservedDispatched, WorkloadName: in.WorkloadName, SpecFingerprint: in.SpecFingerprint, ObservedAt: time.Now(),
	}))
}

// newRuntime is a fresh executor over the real client: a restarted controller
// that discarded its memory.
func (s *stack) newRuntime(c kube.Cluster) *kube.Runtime { return s.newRuntimeWith(c, s.registry) }

func (s *stack) newRuntimeWith(c kube.Cluster, reg kube.Registrar) *kube.Runtime {
	s.t.Helper()
	x, err := kube.NewRuntime(kube.Dependencies{Policy: s.policy, Cluster: c, Attempts: s.attempts, Registrar: reg, Logs: s.c.client},
		kube.Options{Clock: s.e.Clock.Now, CreateQuiescence: 3 * time.Second, OrphanGrace: 2 * time.Second, PreflightTTL: time.Second})
	must(s.t, err)
	return x
}

func (s *stack) count(sql string, args ...any) int { return s.e.Count(sql, args...) }

func (s *stack) jobsNamed(in *execution.AttemptSpec) int {
	jobs, err := s.c.client.ListJobs(context.Background(), s.ns(in.Scope.TenantID), kube.LabelJob+"="+in.WorkloadName)
	must(s.t, err)
	return len(jobs)
}

func (s *stack) grants(in *execution.AttemptSpec) int {
	return s.count(`SELECT count(*) FROM runtime_grants WHERE attempt_id = $1`, string(in.AttemptID))
}

func (s *stack) held() int {
	return s.count(`SELECT count(*) FROM execution_reservations WHERE released_at IS NULL`)
}

func (s *stack) attemptsOf(run *execution.Run) int {
	return s.count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID))
}

func (s *stack) caller() execution.Caller {
	return execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t089"}
}

var kubeUnavailable = kubernetes.Error{Op: "get job", Kind: kubernetes.ErrUnavailable}

// ownerUID returns the UID of the fixture ConfigMap used as a foreign owner.
func (s *stack) ownerUID(ns string) string {
	s.t.Helper()
	if _, err := s.kubectl("-n", ns, "get", "configmap", "t089-owner"); err != nil {
		s.mustKubectl("-n", ns, "create", "configmap", "t089-owner")
	}
	return s.mustKubectl("-n", ns, "get", "configmap", "t089-owner", "-o", "jsonpath={.metadata.uid}")
}

func (s *stack) admitCount(in *execution.AttemptSpec) int {
	return len(s.srv.admitEvents(in.AttemptID))
}

// unpullable is a digest-pinned image no node can pull: its registry address
// refuses connections at once, so the Pod fails to pull (and deletes) quickly
// instead of hanging on name resolution. The Pod stays in ImagePullBackOff and the
// primary container never starts.
const unpullable = "127.0.0.1:1/t089-absent@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// faultRegistrar fails registration calls before or after they are applied.
type faultRegistrar struct {
	inner kube.Registrar
	mu    sync.Mutex
	calls int
	// Before returns an error instead of registering; After registers and then
	// reports a lost response.
	Before func(n int) error
	After  func(n int) error
}

func (f *faultRegistrar) Register(ctx context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.Before != nil {
		if err := f.Before(n); err != nil {
			return runtimeauth.Registration{}, err
		}
	}
	reg, err := f.inner.Register(ctx, cmd)
	if err == nil && f.After != nil {
		if aerr := f.After(n); aerr != nil {
			return runtimeauth.Registration{}, aerr
		}
	}
	return reg, err
}

// storedCredential returns the credential digest the attempt's Secret holds.
func (s *stack) storedDigest(in *execution.AttemptSpec) (runtimeauth.Digest, bool) {
	sec, err := s.c.client.GetSecret(context.Background(), s.ns(in.Scope.TenantID), kube.SecretName(in.WorkloadName))
	if err != nil {
		return runtimeauth.Digest{}, false
	}
	c, err := runtimeauth.ParseCredential(string(sec.Data[kube.KeyAdmission]))
	if err != nil {
		return runtimeauth.Digest{}, false
	}
	return c.Digest(), true
}

// grantRow returns the registered credential digest and expiry of the attempt.
func (s *stack) grantRow(in *execution.AttemptSpec) (digest []byte, created, expires time.Time) {
	s.t.Helper()
	must(s.t, s.e.Migration.QueryRow(context.Background(), `SELECT credential_digest, created_at, expires_at FROM runtime_grants WHERE attempt_id = $1`, string(in.AttemptID)).Scan(&digest, &created, &expires))
	return
}

// attemptOfRun reads the committed attempt and control of a spec without a *testing.T
// failure on a transient read.
func (s *stack) attemptOfRun(in execution.AttemptSpec) (*execution.Attempt, *execution.Control) {
	return s.attempt(&in)
}
