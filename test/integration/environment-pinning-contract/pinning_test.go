// These tests use real execution, secret, authority and non-owner PostgreSQL
// adapters. The environment provider is a component-contract double: real
// delivery snapshot retention/revocation and cleanup remain T-077 qualification.
package environmentpinning_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

type resolver struct {
	snapshot execution.EnvironmentSnapshot
	calls    int
}

type cleanupAuthority struct{}

func (cleanupAuthority) AuthorizeCleanup(context.Context, domain.Scope, string) error { return nil }

func (r *resolver) ResolveEnvironment(_ context.Context, _ execution.EnvironmentRequest) (execution.EnvironmentSnapshot, error) {
	r.calls++
	return r.snapshot, nil
}

func provider() *resolver {
	return &resolver{snapshot: execution.EnvironmentSnapshot{
		Scope:   executiontest.ScopeA,
		Pin:     runtimev1.EnvironmentPin{ID: "env_staging", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("a", 64))},
		Secrets: []runtimev1.SecretBinding{{Ref: "secret://project/kubeconfig", Version: 1, Name: "KUBECONFIG", Delivery: runtimev1.DeliveryFile}},
	}}
}

const deploy = `{id: deploy, kind: container, workspace: {access: read, from: run}, environment: env_staging, image: "` + pipelinetest.Image + `", command: ["true"], timeoutSeconds: 60}`

func command(e *executiontest.Env, p executiontest.Project, definitionID domain.DefinitionID) execution.AdmitCommand {
	return execution.AdmitCommand{Scope: p.Scope, WorkItemID: p.Item, DefinitionID: definitionID,
		ContentRevision: e.ContentRevision(p), SourceCommit: executiontest.CommitA}
}

func Test_environment_admission_roundtrip(t *testing.T) {
	ctx := context.Background()
	r := provider()
	e := executiontest.New(t, executiontest.Options{Environments: r})
	e.GrantAll()
	secret := e.Secret(executiontest.ScopeA, "kubeconfig", "CREDENTIAL-SENTINEL")
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.Doc(deploy))
	run := e.MustAdmit(p, def, nil)
	if r.calls != 1 || len(run.Snapshot.Environments) != 1 || run.Snapshot.Environments[0].Pin != r.snapshot.Pin {
		t.Fatalf("admission did not pin the environment: %+v", run.Snapshot.Environments)
	}
	if n := e.Count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1 AND secret_version = 1 AND released_at IS NULL`, string(run.ID)); n != 1 {
		t.Fatalf("environment credential was not registered with run admission: %d", n)
	}
	before := run.SnapshotDigest
	// A different process now sees a changed environment and credential, but an
	// existing run must reconstruct solely from persisted admission inputs.
	e.RotateSecret(p.Scope, secret.ID(), "ROTATED-SENTINEL")
	r.snapshot.Pin.ConfigDigest = runtimev1.Digest("sha256:" + strings.Repeat("b", 64))
	r.snapshot.Secrets[0].Version = 2
	r.calls = 0
	e.Svc = e.NewService(executiontest.Options{Environments: r})
	intent := e.MustClaim(p.Scope.TenantID)
	loaded := e.Get(run)
	attempt := loaded.Attempt(intent.AttemptID)
	if attempt == nil {
		t.Fatal("committed dispatch attempt missing")
	}
	c, err := runtimev1.DecodeResponse[runtimev1.Context](attempt.Context)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 || c.Environment == nil || c.Environment.ConfigDigest != runtimev1.Digest("sha256:"+strings.Repeat("a", 64)) ||
		len(c.Secrets) != 1 || c.Secrets[0].Version != 1 || loaded.Run.SnapshotDigest != before {
		t.Fatalf("restart/dispatch resolved a mutable environment: calls=%d, environment=%+v", r.calls, c.Environment)
	}
	if strings.Contains(string(attempt.Context), "SENTINEL") {
		t.Fatal("credential plaintext reached the runtime context")
	}
	if _, err := e.Store.Get(ctx, executiontest.ScopeB, run.ID); !errors.Is(err, execution.ErrNotFound) {
		t.Fatalf("foreign tenant read the pinned run: %v", err)
	}
	fresh := e.MustAdmit(p, def, nil)
	if fresh.Snapshot.Environments[0].Pin != r.snapshot.Pin || fresh.Snapshot.Secrets[0].Binding.Version != 2 || fresh.SnapshotDigest == before {
		t.Fatal("a new run did not select the new immutable environment snapshot")
	}
}

func Test_environment_credential_rollback(t *testing.T) {
	ctx := context.Background()
	r := provider()
	e := executiontest.New(t, executiontest.Options{Environments: r})
	e.GrantAll()
	secret := e.Secret(executiontest.ScopeA, "kubeconfig", "CREDENTIAL-SENTINEL")
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.Doc(deploy))
	plan, err := e.Svc.Prepare(ctx, execution.Caller{PrincipalID: executiontest.Engineer}, command(e, p, def.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RevokeSecret(p.Scope, secret.ID(), 1); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, table := range []string{"runs", "step_runs", "run_history", "secret_pins", "workspace_retained_refs", "audit", "outbox"} {
		counts[table] = e.Count("SELECT count(*) FROM " + table)
	}
	if _, _, err := e.Store.Admit(ctx, plan); !errors.Is(err, execution.ErrAdmissionRefused) {
		t.Fatalf("credential revoked during preparation was admitted: %v", err)
	}
	for table, before := range counts {
		if after := e.Count("SELECT count(*) FROM " + table); after != before {
			t.Errorf("failed environment admission changed %s: %d -> %d", table, before, after)
		}
	}
}

func Test_environment_missing_resolver_and_scope(t *testing.T) {
	ctx := context.Background()
	e := executiontest.New(t)
	e.GrantAll()
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.Doc(deploy))
	if _, err := e.Admit(p, def, nil); !errors.Is(err, execution.ErrNotConfigured) {
		t.Fatalf("environment admission without a resolver succeeded: %v", err)
	}
	r := provider()
	r.snapshot.Scope = executiontest.ScopeB
	e.Svc = e.NewService(executiontest.Options{Environments: r})
	if _, err := e.Admit(p, def, nil); !errors.Is(err, execution.ErrAdmissionRefused) {
		t.Fatalf("foreign environment snapshot admitted: %v", err)
	}
	if e.Count(`SELECT count(*) FROM runs`) != 0 {
		t.Fatal("rejected environment admission persisted a run")
	}
	// Existing no-environment definitions remain available without the optional
	// integration; its absence must not disable ordinary M1 admission.
	e.Svc = e.NewService(executiontest.Options{})
	plain := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("plain")))
	run := e.MustAdmit(p, plain, nil)
	if len(run.Snapshot.Environments) != 0 {
		t.Fatal("ordinary admission acquired an environment pin")
	}
	if _, err := e.Store.Get(ctx, p.Scope, run.ID); err != nil {
		t.Fatal(err)
	}
	// Until the retained-deployment binding is qualified, cleanup must never
	// accidentally select the environment's current configuration.
	r = provider()
	e.Svc = e.NewService(executiontest.Options{Environments: r, Cleanup: cleanupAuthority{}})
	cleanupDef := e.Define(p.Scope, pipelinetest.Doc(strings.Replace(deploy, "{access: read, from: run}", "{access: none}", 1)))
	if _, err := e.Svc.PrepareCleanup(ctx, execution.CleanupCommand{Scope: p.Scope, DeploymentID: "dep_recorded", DefinitionID: cleanupDef.ID}); !errors.Is(err, execution.ErrNotConfigured) {
		t.Fatalf("cleanup selected an unqualified mutable environment: %v", err)
	}
	if r.calls != 0 {
		t.Fatal("cleanup queried the current environment instead of requiring its retained binding")
	}
}

func Test_environment_alias_rejected(t *testing.T) {
	r := provider()
	r.snapshot.Secrets[0].Ref = "secret://project/token"
	e := executiontest.New(t, executiontest.Options{Environments: r})
	e.GrantAll()
	e.Secret(executiontest.ScopeA, "token", "CREDENTIAL-SENTINEL")
	p := e.NewProject(executiontest.ScopeA)
	for _, fromDefinition := range []bool{true, false} {
		name := "provider aliases"
		step := deploy
		if fromDefinition {
			name = "explicit and environment aliases"
			step = strings.TrimSuffix(deploy, "}") + `, env: {TOKEN: {secretRef: "secret://project/token"}}}`
		} else {
			alias := r.snapshot.Secrets[0]
			alias.Name, alias.Delivery = "TOKEN", runtimev1.DeliveryEnv
			r.snapshot.Secrets = append(r.snapshot.Secrets, alias)
		}
		t.Run(name, func(t *testing.T) {
			def := e.Define(p.Scope, pipelinetest.Doc(step))
			if _, err := e.Admit(p, def, nil); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("undeliverable credential aliases admitted: %v", err)
			}
			if e.Count(`SELECT count(*) FROM runs`) != 0 || e.Count(`SELECT count(*) FROM secret_pins`) != 0 {
				t.Fatal("rejected alias left an admitted run or credential pin")
			}
		})
	}
}
