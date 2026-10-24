// Package runtime_test is the end-to-end roundtrip of attempt workloads
// through the real step entrypoint on the non-Kubernetes runtimes: real
// PostgreSQL, the real execution service, the real runtime admission service
// and routes behind TLS, and `kodama step` run by the Process runtime on the
// host or by the Podman runtime in the step's pinned image. The only stand-in
// is the result route (T-022), which authorizes through runtime admission and
// commits through execution.CommitResult.
package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/storage"
	podmantest "github.com/optimiware/kodama/test/support/podman"
)

// runtime is what the roundtrip needs of a backend.
type runtime interface {
	execution.Runtime
	runtimeauth.JobObserver
}

// backend builds one runtime against the stack's runtime API.
type backend struct {
	name      string
	namespace string
	// requirements is the admission policy the backend can meet.
	requirements execution.Requirements
	new          func(t *testing.T, srv *server, issuer bootstrap.Issuer) runtime
}

var backends = []backend{
	{
		name: process.Backend, namespace: process.Namespace, requirements: runtimetest.NoRequirements,
		new: func(t *testing.T, srv *server, issuer bootstrap.Issuer) runtime {
			dir := t.TempDir()
			ca := filepath.Join(dir, "ca.crt")
			must(t, os.WriteFile(ca, srv.CA, 0o600))
			rt, err := process.New(process.Config{
				Program: process.Entrypoint{Binary: stepbin(t, dir, ""), ServerURL: srv.URL("127.0.0.1"), CAFile: ca, Issuer: issuer},
				Dir:     filepath.Join(dir, "workloads"),
			})
			must(t, err)
			return rt
		},
	},
	{
		name: podman.Backend, namespace: podman.Namespace,
		new: func(t *testing.T, srv *server, issuer bootstrap.Issuer) runtime {
			podmantest.Require(t)
			dir := podmantest.SharedDir(t)
			rt, err := podman.New(podman.Config{
				Program:  podman.Entrypoint{Binary: stepbin(t, dir, podmantest.Arch(t)), ServerURL: srv.URL("host.containers.internal"), CA: srv.CA, Issuer: issuer},
				StateDir: filepath.Join(dir, "workloads"), Network: "bridge", StopTimeout: 2 * time.Second,
			})
			must(t, err)
			return rt
		},
	},
}

// stepbin builds the entrypoint into dir, for the host or for Linux on arch.
func stepbin(t *testing.T, dir, arch string) string {
	t.Helper()
	bin := filepath.Join(dir, "kodama")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./test/integration/runtime/stepbin")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if arch != "" {
		cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH="+arch)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the entrypoint: %v: %s", err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	must(t, err)
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// stack is one backend wired to execution and runtime admission.
type stack struct {
	e   *executiontest.Env
	srv *server
	rt  runtime
	svc *execution.Service
}

func newStack(t *testing.T, b backend) *stack {
	t.Helper()
	e := executiontest.New(t)
	e.GrantAll()
	for _, m := range rtpg.Migrations().Migrations {
		sql := strings.ReplaceAll(m.SQL, storage.RuntimeRolePlaceholder, `"`+e.Base.DB.RuntimeRole+`"`)
		if _, err := e.Migration.Exec(context.Background(), sql); err != nil {
			t.Fatalf("runtimeauth migration %s: %v", m.ID, err)
		}
	}
	srv := newServer(t)
	authority, err := rpg.NewAttemptAuthority(e.Store, func(domain.Scope) (string, error) { return b.namespace, nil }, nil)
	must(t, err)
	grants, err := rtpg.NewStore(e.Runtime, authority)
	must(t, err)
	key, err := jwt.GenerateKey()
	must(t, err)
	ring, err := jwt.NewKeyRing(1, key)
	must(t, err)
	const issuer = "https://runtime.t086.invalid"
	signer, err := jwt.NewSigner(ring, issuer)
	must(t, err)
	policy := runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: time.Hour}
	verifier, err := jwt.NewVerifier(jwt.VerifierConfig{Ring: ring, Issuer: issuer, MaxLifetime: policy.MaxSession, Clock: e.Clock.Now})
	must(t, err)

	s := &stack{e: e, srv: srv}
	admission, err := runtimeauth.NewService(runtimeauth.Config{
		Store: grants, Attempts: authority, Jobs: jobs(func(ctx context.Context, o runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
			return s.rt.ObserveJob(ctx, o)
		}),
		Tokens: signer, Verifier: verifier, Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{entrypoint.Version}},
		Policy: policy, Clock: e.Clock.Now,
	})
	must(t, err)
	s.rt = b.new(t, srv, bootstrap.Issuer{Registrar: admission})
	s.svc = e.NewService(executiontest.Options{Runtime: s.rt, Requirements: b.requirements, Cleanup: allowCleanup{}})
	srv.start(t, admission, s.svc)
	return s
}

type jobs func(context.Context, runtimeauth.JobObservation) (runtimeauth.JobEvidence, error)

func (f jobs) ObserveJob(ctx context.Context, o runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
	return f(ctx, o)
}

type allowCleanup struct{}

func (allowCleanup) AuthorizeCleanup(context.Context, domain.Scope, string) error { return nil }

// admit admits a one-step run of script in the pinned image. It is a cleanup
// run (workspace access none), so the entrypoint needs no Git checkout.
func (s *stack) admit(t *testing.T, name, script string) *execution.Run {
	t.Helper()
	scope := executiontest.ScopeA
	def := s.e.Define(scope, fmt.Sprintf(`apiVersion: kodama/v1alpha1
kind: Pipeline
metadata: {name: %s}
spec:
  inputs: {}
  steps:
    - id: work
      kind: container
      image: %s
      workspace: {access: none}
      command: ["/bin/sh", "-ec"]
      args: [%q]
      timeoutSeconds: 120
      outputs: {v: {type: string}}
`, name, podmantest.Image, script))
	run, err := s.svc.AdmitCleanup(context.Background(), execution.CleanupCommand{Scope: scope, DeploymentID: "dep_" + name, DefinitionID: def.ID})
	must(t, err)
	return run
}

// until reconciles until cond holds: the periodic loop the server schedules.
func (s *stack) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for !cond() {
		if _, err := s.svc.ReconcileTenant(ctx, executiontest.ScopeA.TenantID); err != nil && ctx.Err() == nil {
			t.Logf("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s", what)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Test_runtime_roundtrip: on each backend, a dispatched attempt runs the real
// entrypoint, which is admitted against the runtime-verified workload
// identity, runs the declared command and submits its result; the run
// succeeds with the command's output, and the settled workload is removed
// and its removal recorded.
func Test_runtime_roundtrip(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := newStack(t, b)
			run := s.admit(t, "t086-"+b.name, `printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`)
			if got := run.Snapshot.Runtime; got == nil || got.Backend != b.name {
				t.Fatalf("binding %+v", got)
			}
			if claimed, err := s.svc.DispatchNext(ctx, executiontest.ScopeA.TenantID); err != nil || !claimed {
				t.Fatalf("dispatch: %v %v", claimed, err)
			}
			at := s.e.Get(run).Attempts[0]
			if at.WorkloadUID == "" {
				t.Fatal("dispatch bound no workload instance")
			}

			s.until(t, "the run to succeed", func() bool { return s.e.Get(run).Run.Status.Terminal() })
			agg := s.e.Get(run)
			got := agg.Attempt(at.ID)
			if agg.Run.Status != execution.RunSucceeded || got.Status != execution.AttemptSucceeded || s.srv.Results.Load() != 1 || s.srv.admits.Load() == 0 {
				var logs strings.Builder
				_ = s.rt.Logs(ctx, at.Workload(), &logs)
				t.Fatalf("run %s attempt %s/%s results %d admits %d\n%s", agg.Run.Status, got.Status, got.Reason, s.srv.Results.Load(), s.srv.admits.Load(), logs.String())
			}
			if v := agg.Step("work").Outputs["v"]; v != runtimev1.StringValue("ok") {
				t.Fatalf("output %+v", v)
			}

			s.until(t, "the workload to be removed", func() bool {
				return s.e.Count(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND workload_removed_at IS NOT NULL`, string(at.ID)) == 1
			})
			if _, err := s.rt.Observe(ctx, at.Workload()); !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("the removed workload is still observed: %v", err)
			}
		})
	}
}
