package adapters_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/memory"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// These are deterministic unit tests over controlled ports. They prove the
// binding contract (what is pinned, what the admission transaction
// revalidates, what is undone on failure). They do not certify SQL, row-level
// security or restart behavior: T-067 qualifies those against real persistence.

var (
	scope  = domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}
	other  = domain.Scope{TenantID: "t_other", ProjectID: "p_inv"}
	t0     = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	commit = strings.Repeat("1", 40)
	engnr  = execution.Actor{Type: execution.ActorUser, ID: "u_engineer"}
)

const (
	kubeRef  = "secret://project/kubeconfig"
	tokenRef = "secret://project/token"

	deployStep = `{id: deploy, kind: container, workspace: {access: read, from: run}, environment: env_staging, image: "` + pipelinetest.Image + `", command: ["true"], timeoutSeconds: 60}`
	cleanStep  = `{id: teardown, kind: container, workspace: {access: none}, environment: env_staging, image: "` + pipelinetest.Image + `", command: ["true"], timeoutSeconds: 60}`
)

// ---- delivery fixtures --------------------------------------------------------

func environment(t *testing.T, sc domain.Scope, config map[string]string) delivery.Environment {
	t.Helper()
	env, err := delivery.NewEnvironment(delivery.EnvironmentRecord{
		Scope: sc, ID: "env_staging", Name: "staging", Kind: delivery.KindStage, Target: "kind", Namespace: "staging",
		Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: kubeRef, Delivery: runtimev1.DeliveryFile},
		Secrets:    []delivery.Credential{{Name: "TOKEN", Ref: tokenRef, Delivery: runtimev1.DeliveryEnv}},
		Config:     config,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func snapshot(t *testing.T, sc domain.Scope, config map[string]string, kubeVersion int64) delivery.ConfigurationSnapshot {
	t.Helper()
	s, err := delivery.NewSnapshot(environment(t, sc, config), map[string]int64{kubeRef: kubeVersion, tokenRef: 3}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// deployment is a settled deployment whose cleanup was requested and which
// pinned snap.
func deployment(t *testing.T, snap delivery.ConfigurationSnapshot) delivery.Deployment {
	t.Helper()
	exp := t0.Add(time.Hour)
	d, err := delivery.NewDeployment(delivery.Registration{
		Scope: snap.Scope, Environment: snap.Environment, ConfigDigest: snap.Digest, Namespace: snap.Namespace,
		Source:   delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: commit},
		Image:    delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
		Producer: delivery.Producer{Attempt: "att_build", Run: "run_build", Step: "build", ToolImage: "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		Run:      "run_deploy", Step: "deploy", ActionKey: "act:0123456789abcdef", Attempt: "att_deploy", ExpiresAt: &exp,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	rec := d.Record()
	rec.Status = delivery.StatusSucceeded
	requested := t0.Add(time.Minute)
	rec.CleanupRequestedAt, rec.CleanupState = &requested, delivery.CleanupRequested
	d, err = delivery.RestoreDeployment(rec)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func withDeleted(t *testing.T, d delivery.Deployment) delivery.Deployment {
	t.Helper()
	rec := d.Record()
	gone := t0.Add(time.Hour)
	rec.DeletedAt = &gone
	d, err := delivery.RestoreDeployment(rec)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// ---- controlled delivery ports ------------------------------------------------

type fakeResolver struct {
	snap  delivery.ConfigurationSnapshot
	err   error
	calls int
}

func (f *fakeResolver) ResolveSnapshot(_ context.Context, sc domain.Scope, a delivery.Actor, id domain.EnvironmentID) (delivery.ConfigurationSnapshot, error) {
	f.calls++
	if a.ID != engnr.ID || sc != scope || id != "env_staging" {
		return delivery.ConfigurationSnapshot{}, delivery.ErrDenied
	}
	return f.snap, f.err
}

type fakeViewer struct {
	view  delivery.CleanupView
	err   error
	calls int
}

func (f *fakeViewer) RetainedView(context.Context, domain.Scope, delivery.DeploymentID) (delivery.CleanupView, error) {
	f.calls++
	return f.view, f.err
}

// txReader is delivery's reads inside one open transaction.
// ---- service fixtures ----------------------------------------------------------

type defs map[domain.DefinitionID]pipeline.Definition

func (d defs) Get(_ context.Context, _ domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	def, ok := d[id]
	if !ok {
		return pipeline.Definition{}, execution.ErrNotFound
	}
	return def, nil
}

type allow struct{}

func (allow) ResolveActor(_ context.Context, _ domain.Scope, p string) (execution.Actor, error) {
	return execution.Actor{Type: execution.ActorUser, ID: p}, nil
}
func (allow) Authorize(context.Context, execution.Actor, domain.Scope, execution.Permission) error {
	return nil
}

type work struct{}

func (work) Snapshot(context.Context, domain.Scope, domain.WorkItemID) (execution.WorkSnapshot, error) {
	return execution.WorkSnapshot{ID: "wi_1", Actionable: true, ContentRevision: 1, Spec: "s"}, nil
}

type noConfig struct{}

func (noConfig) Resolve(context.Context, execution.ConfigRequest) (execution.ConfigResult, error) {
	return execution.ConfigResult{}, nil
}

type sources struct{}

func (sources) Verify(context.Context, domain.Scope, domain.WorkItemID, string) (execution.SourceInfo, error) {
	return execution.SourceInfo{WorkspaceID: "ws_1", RepositoryURL: "https://git.example.invalid/r.git", Branch: "refs/heads/w", AcceptedTip: commit}, nil
}
func (sources) Describe(context.Context, domain.Scope, string, bool) (execution.SourceInfo, error) {
	return execution.SourceInfo{}, nil
}
func (sources) Retain(context.Context, domain.Scope, string, domain.RunID, string) (string, error) {
	return "", nil
}

type cleanupAllowed struct{ err error }

func (c cleanupAllowed) AuthorizeCleanup(context.Context, domain.Scope, string) error { return c.err }

func definition(t *testing.T, name string, step string) pipeline.Definition {
	t.Helper()
	v := pipelinetest.NewValidator(t, pipeline.KindContainer)
	actor := pipeline.Actor{ID: "u1", Type: pipeline.ActorUser}
	validated, err := v.ValidateYAML(context.Background(), scope, actor, []byte(pipelinetest.DocNamed(name, "{}", step)))
	if err != nil {
		t.Fatal(err)
	}
	def, err := pipeline.NewDefinition(pipeline.Candidate{Scope: scope, ID: domain.DefinitionID("pd_" + validated.Name()), Content: validated, CreatedBy: actor, CreatedAt: t0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return def
}

type wiring struct {
	resolver   execution.EnvironmentResolver
	cleanup    execution.DeploymentCleanup
	cleanupEnv execution.DeploymentEnvironments
	definition pipeline.Definition
	// store is the configured execution store; nil is a store with no
	// transactional environment support (as the in-memory store).
	store execution.Store
}

// plainStore is a store without the optional environment capability.
type plainStore struct{ execution.Store }

func service(t *testing.T, w wiring) *execution.Service {
	t.Helper()
	if w.store == nil {
		w.store = plainStore{}
	}
	d := execution.Dependencies{
		Store: w.store, Authority: allow{}, Definitions: defs{w.definition.ID: w.definition},
		Capabilities: pipeline.EnabledKinds{pipeline.KindContainer}, Work: work{}, Configuration: noConfig{}, Sources: sources{},
		Limits: adapters.StaticLimits{Default: execution.Limit{TenantConcurrency: 4, ProjectConcurrency: 2, StartupBudget: time.Minute, MaxOutputBytes: 1, MaxOutputFiles: 1,
			MaxDiagnosticBytes: 1, DefaultTimeout: time.Minute, MaxTimeout: time.Hour}},
		Hooks: execution.EnabledKinds{pipeline.KindContainer}, Cleanup: w.cleanup, CleanupEnvironments: w.cleanupEnv,
		Clock: func() time.Time { return t0 },
	}
	var svc *execution.Service
	var err error
	if w.resolver != nil {
		svc, err = execution.NewServiceWithEnvironments(d, execution.Options{}, w.resolver)
	} else {
		svc, err = execution.NewService(d, execution.Options{})
	}
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func admitCommand(def pipeline.Definition) execution.AdmitCommand {
	return execution.AdmitCommand{Scope: scope, WorkItemID: "wi_1", DefinitionID: def.ID, ContentRevision: 1, SourceCommit: commit}
}

func resolverFor(t *testing.T, f *fakeResolver) execution.EnvironmentResolver {
	t.Helper()
	r, err := adapters.NewDeliveryEnvironments(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func bind(t *testing.T, r *txReader) execution.TxEnvironments {
	t.Helper()
	tx, err := adapters.NewTxEnvironments(r)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// withDigest presents content under a retained digest it does not cover.
func withDigest(s delivery.ConfigurationSnapshot, d runtimev1.Digest) delivery.ConfigurationSnapshot {
	s.Digest = d
	return s
}

// altered is content that no longer matches its retained digest.
func altered(s delivery.ConfigurationSnapshot) delivery.ConfigurationSnapshot {
	s.Config = map[string]string{"REPLICAS": "99"}
	return s
}

// prepareWork returns the plan of a work run admitted through the delivery
// resolver, prepared for the harness's production store.
func prepareWork(t *testing.T, h *harness, f *fakeResolver, def pipeline.Definition) *execution.AdmissionPlan {
	t.Helper()
	plan, err := service(t, wiring{resolver: resolverFor(t, f), definition: def, store: h.store}).Prepare(context.Background(), execution.Caller{PrincipalID: engnr.ID}, admitCommand(def))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// ---- environment_binding --------------------------------------------------------

func Test_environment_binding(t *testing.T) {
	ctx := context.Background()
	s1 := snapshot(t, scope, map[string]string{"REPLICAS": "1"}, 1)
	def := definition(t, "deploy", deployStep)

	t.Run("the resolver pins the retained snapshot and its exact credential bindings", func(t *testing.T) {
		f := &fakeResolver{snap: s1}
		r := resolverFor(t, f)
		got, err := r.ResolveEnvironment(ctx, execution.EnvironmentRequest{Scope: scope, Actor: engnr, EnvironmentID: "env_staging"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Scope != scope || got.Pin != s1.Pin() || len(got.Secrets) != 2 {
			t.Fatalf("snapshot %+v", got)
		}
		got.Secrets[0].Version = 99
		if s1.Secrets[0].Version == 99 {
			t.Fatal("the resolver aliases the retained snapshot")
		}
	})

	t.Run("the resolver refuses what delivery refuses, trusts no unverifiable snapshot and never grants on an outage", func(t *testing.T) {
		tampered := s1
		tampered.Digest = runtimev1.Digest("sha256:" + strings.Repeat("e", 64))
		foreign := snapshot(t, other, map[string]string{"REPLICAS": "1"}, 1)
		for name, f := range map[string]*fakeResolver{
			"denied":                      {err: delivery.ErrDenied},
			"missing":                     {err: delivery.ErrNotFound},
			"revoked credential":          {err: delivery.ErrCredentialUnavailable},
			"digest not covering content": {snap: tampered},
			"foreign scope":               {snap: foreign},
		} {
			_, err := resolverFor(t, f).ResolveEnvironment(ctx, execution.EnvironmentRequest{Scope: scope, Actor: engnr, EnvironmentID: "env_staging"})
			if !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Errorf("%s: %v", name, err)
			}
		}
		_, err := resolverFor(t, &fakeResolver{err: errors.New("provider outage")}).ResolveEnvironment(ctx, execution.EnvironmentRequest{Scope: scope, Actor: engnr, EnvironmentID: "env_staging"})
		if !errors.Is(err, execution.ErrUnavailable) {
			t.Errorf("outage: %v", err)
		}
		if _, err := resolverFor(t, &fakeResolver{snap: s1}).ResolveEnvironment(ctx, execution.EnvironmentRequest{Scope: scope, Actor: execution.Actor{}, EnvironmentID: "env_staging"}); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Errorf("anonymous actor: %v", err)
		}
		if _, err := adapters.NewDeliveryEnvironments(nil); err == nil {
			t.Error("constructor accepted no delivery service")
		}
		if _, err := adapters.NewTxEnvironments(nil); err == nil {
			t.Error("constructor accepted no transaction reader")
		}
	})

	t.Run("the production admission binds the pinned snapshot before it writes anything", func(t *testing.T) {
		tx := newTx(s1)
		h := newHarness(t, tx)
		plan := prepareWork(t, h, &fakeResolver{snap: s1}, def)
		if len(plan.Environments) != 1 || plan.Environments[0].Pin != s1.Pin() || plan.Environments[0].Actor != engnr || plan.Environments[0].Deployment != "" ||
			len(plan.Environments[0].Secrets) != 2 || plan.Environments[0].Scope != scope || len(plan.Secrets) != 2 {
			t.Fatalf("plan %+v", plan.Environments)
		}
		run, created, err := h.admit(plan)
		if err != nil || !created || run.ID != plan.Aggregate.Run.ID {
			t.Fatalf("%v created=%v", err, created)
		}
		if len(tx.calls) != 2 || tx.calls[0] != "authorize:u_engineer:deployment:manage" || tx.calls[1] != "snapshot" {
			t.Fatalf("the transaction did not recheck authority then retention: %v", tx.calls)
		}
		d := h.db
		if bound, insert := d.first("bind:snapshot"), d.first("exec:runs"); bound < 0 || insert < 0 || bound > insert || d.first("authority:") > bound {
			t.Fatalf("binding must follow the authority check and precede the run row: %v", d.trace)
		}
		if len(d.runs) != 1 || d.runs[0].ID != string(run.ID) || d.runs[0].Digest != run.SnapshotDigest || !strings.Contains(d.runs[0].Inputs, string(s1.Digest)) {
			t.Fatalf("the committed run does not carry the pinned snapshot: %+v", d.runs)
		}
		want := []string{string(run.ID) + "|" + kubeRef + "@1", string(run.ID) + "|" + tokenRef + "@3"}
		if len(d.pins) != 2 || d.pins[0] != want[0] || d.pins[1] != want[1] {
			t.Fatalf("exact credential pins %v, want %v", d.pins, want)
		}
		if d.commits != 1 || d.rollbacks != 0 || d.committedTable("step_runs") != 1 || d.committedTable("workspace") != 1 || len(d.events) == 0 {
			t.Fatalf("commits=%d rollbacks=%d rows=%v", d.commits, d.rollbacks, d.rows)
		}
	})

	t.Run("an edit or rotation never rewrites an admitted pin", func(t *testing.T) {
		tx := newTx(s1)
		h := newHarness(t, tx)
		f := &fakeResolver{snap: s1}
		first, _, err := h.admit(prepareWork(t, h, f, def))
		if err != nil {
			t.Fatal(err)
		}
		// The environment is edited and a credential rotates: delivery now
		// retains a different snapshot, and the old one stays retained.
		s2 := snapshot(t, scope, map[string]string{"REPLICAS": "3"}, 2)
		if s1.Digest == s2.Digest {
			t.Fatal("fixture: the edit must change the identity")
		}
		f.snap = s2
		tx.snapshots[key(s2.Environment, s2.Digest)] = s2
		second, _, err := h.admit(prepareWork(t, h, f, def))
		if err != nil || second.ID == first.ID || second.SnapshotDigest == first.SnapshotDigest {
			t.Fatalf("a new run did not select the new snapshot: %v", err)
		}
		runs := h.db.runs
		if len(runs) != 2 || !strings.Contains(runs[0].Inputs, string(s1.Digest)) || strings.Contains(runs[0].Inputs, string(s2.Digest)) ||
			!strings.Contains(runs[1].Inputs, string(s2.Digest)) || runs[0].Digest != first.SnapshotDigest {
			t.Fatalf("the edit rewrote an admitted run: %+v", runs)
		}
		if h.db.pins[0] != string(first.ID)+"|"+kubeRef+"@1" || h.db.pins[2] != string(second.ID)+"|"+kubeRef+"@2" {
			t.Fatalf("credential pins %v", h.db.pins)
		}
	})

	t.Run("a missing, foreign, altered, revoked or unavailable retained snapshot refuses before any write", func(t *testing.T) {
		rotated := snapshot(t, scope, map[string]string{"REPLICAS": "1"}, 2) // same environment, other credential version
		foreign := snapshot(t, other, map[string]string{"REPLICAS": "1"}, 1)
		for name, tc := range map[string]struct {
			mutate func(*txReader)
			want   error
		}{
			"missing":              {func(r *txReader) { delete(r.snapshots, key(s1.Environment, s1.Digest)) }, execution.ErrAdmissionRefused},
			"foreign scope":        {func(r *txReader) { r.snapshots[key(s1.Environment, s1.Digest)] = foreign }, execution.ErrAdmissionRefused},
			"altered content":      {func(r *txReader) { r.snapshots[key(s1.Environment, s1.Digest)] = altered(s1) }, execution.ErrAdmissionRefused},
			"other credential set": {func(r *txReader) { r.snapshots[key(s1.Environment, s1.Digest)] = withDigest(rotated, s1.Digest) }, execution.ErrAdmissionRefused},
			"authority revoked":    {func(r *txReader) { r.authErr = delivery.ErrDenied }, execution.ErrDenied},
			"actor hidden":         {func(r *txReader) { r.authErr = delivery.ErrNotFound }, execution.ErrAdmissionRefused},
			"authority outage":     {func(r *txReader) { r.authErr = errors.New("identity down") }, execution.ErrUnavailable},
			"snapshot outage":      {func(r *txReader) { r.snapErr = errors.New("database down") }, execution.ErrUnavailable},
			"credential revoked":   {func(r *txReader) { r.snapErr = delivery.ErrCredentialUnavailable }, execution.ErrAdmissionRefused},
		} {
			tx := newTx(s1)
			h := newHarness(t, tx)
			plan := prepareWork(t, h, &fakeResolver{snap: s1}, def)
			tc.mutate(tx)
			if _, _, err := h.admit(plan); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
			if !h.wroteNothing() || !h.db.empty() || h.db.rollbacks != 1 {
				t.Errorf("%s: a refused binding wrote or committed state: %v", name, h.db.trace)
			}
		}
	})

	t.Run("a store without the delivery binding fails closed, no-environment admission is unaffected", func(t *testing.T) {
		unbound := newHarness(t, nil)
		if unbound.store.AdmitsEnvironments() {
			t.Fatal("a store without the binding reports support")
		}
		plan := prepareWork(t, unbound, &fakeResolver{snap: s1}, def)
		if _, _, err := unbound.admit(plan); !errors.Is(err, execution.ErrNotConfigured) || !unbound.wroteNothing() || !unbound.db.empty() {
			t.Fatalf("an unbound store admitted pinned environments: %v %v", err, unbound.db.trace)
		}
		bare := definition(t, "plain", pipelinetest.Container("build", "workspace: {access: read, from: run}"))
		pp, err := service(t, wiring{definition: bare, store: unbound.store}).Prepare(ctx, execution.Caller{PrincipalID: engnr.ID}, admitCommand(bare))
		if err != nil || len(pp.Environments) != 0 || len(pp.Aggregate.Run.Snapshot.Environments) != 0 {
			t.Fatalf("%v %+v", err, pp)
		}
		run, created, err := unbound.admit(pp)
		if err != nil || !created || unbound.db.commits != 1 || strings.Contains(unbound.db.runs[0].Inputs, "environments") || run.ID != pp.Aggregate.Run.ID {
			t.Fatalf("historical admission needs no binding: %v", err)
		}
		if unbound.db.first("bind:") != -1 {
			t.Fatal("a run without environments consulted delivery")
		}
	})

	t.Run("bindings must be exactly the admitted pins of the admitted run", func(t *testing.T) {
		svc := service(t, wiring{resolver: resolverFor(t, &fakeResolver{snap: s1}), definition: def})
		for name, alter := range map[string]func(*execution.AdmissionPlan){
			"no binding":    func(p *execution.AdmissionPlan) { p.Environments = nil },
			"foreign scope": func(p *execution.AdmissionPlan) { p.Environments[0].Scope = other },
			"other pin": func(p *execution.AdmissionPlan) {
				p.Environments[0].Pin.ConfigDigest = snapshot(t, scope, nil, 1).Digest
			},
			"other actor": func(p *execution.AdmissionPlan) {
				p.Environments[0].Actor = execution.Actor{Type: execution.ActorUser, ID: "u_other"}
			},
			"cleanup binding": func(p *execution.AdmissionPlan) { p.Environments[0].Deployment = "dep_1" },
		} {
			plan, err := svc.Prepare(ctx, execution.Caller{PrincipalID: engnr.ID}, admitCommand(def))
			if err != nil {
				t.Fatal(err)
			}
			alter(plan)
			tx := newTx(s1)
			err = execution.BindEnvironments(ctx, bind(t, tx), plan)
			if !errors.Is(err, execution.ErrAdmissionRefused) || len(tx.calls) != 0 {
				t.Errorf("%s: %v (calls %v)", name, err, tx.calls)
			}
		}
	})
}

// ---- environment_binding_rollback --------------------------------------------------

func Test_environment_binding_rollback(t *testing.T) {
	ctx := context.Background()
	s1 := snapshot(t, scope, map[string]string{"REPLICAS": "1"}, 1)
	def := definition(t, "deploy", deployStep)

	t.Run("a transaction-port failure writes nothing and the same plan admits once it recovers", func(t *testing.T) {
		tx := newTx(s1)
		h := newHarness(t, tx)
		plan := prepareWork(t, h, &fakeResolver{snap: s1}, def)
		for name, inject := range map[string]func(){
			"authority outage": func() { tx.authErr = errors.New("identity down") },
			"snapshot outage":  func() { tx.snapErr = errors.New("database down") },
		} {
			tx.authErr, tx.snapErr = nil, nil
			inject()
			if _, _, err := h.admit(plan); !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("%s: %v", name, err)
			}
			if !h.wroteNothing() || !h.db.empty() || h.db.rollbacks == 0 {
				t.Fatalf("%s: wrote before binding: %v", name, h.db.trace)
			}
		}
		tx.authErr, tx.snapErr = nil, nil
		run, created, err := h.admit(plan)
		if err != nil || !created || run.ID != plan.Aggregate.Run.ID || len(h.db.runs) != 1 || h.db.runs[0].Digest != plan.Aggregate.Run.SnapshotDigest {
			t.Fatalf("the plan did not recover with its prepared identity: %v", err)
		}
	})

	t.Run("a credential revoked after revalidation rolls back the rows and the earlier pins", func(t *testing.T) {
		h := newHarness(t, newTx(s1))
		plan := prepareWork(t, h, &fakeResolver{snap: s1}, def)
		h.db.revoked[tokenRef] = true // the second pin of the sorted set
		if _, _, err := h.admit(plan); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("%v", err)
		}
		if h.db.first("exec:runs") < 0 || h.db.first("pin:"+kubeRef) < 0 || h.db.first("pin:"+tokenRef) < 0 {
			t.Fatalf("the failure must happen after the rows and the first pin were written: %v", h.db.trace)
		}
		if !h.db.empty() || h.db.rollbacks != 1 || h.db.first("event:") >= 0 {
			t.Fatalf("a refused pin left state or events: %+v", h.db)
		}
		delete(h.db.revoked, tokenRef)
		run, created, err := h.admit(plan)
		if err != nil || !created || run.ID != plan.Aggregate.Run.ID || len(h.db.pins) != 2 {
			t.Fatalf("retry after repair: %v pins=%v", err, h.db.pins)
		}
	})

	t.Run("history, event and row failures after the pins propagate to rollback and the retry keeps its identities", func(t *testing.T) {
		boom := errors.New("write failed")
		for name, inject := range map[string]func(*fakeDB){
			"step row":     func(d *fakeDB) { d.failTable["step_runs"] = boom },
			"history row":  func(d *fakeDB) { d.failTable["run_history"] = boom },
			"audit row":    func(d *fakeDB) { d.failTable["audit"] = boom },
			"event append": func(d *fakeDB) { d.failEventAt, d.eventErr = 1, boom },
		} {
			h := newHarness(t, newTx(s1))
			plan := prepareWork(t, h, &fakeResolver{snap: s1}, def)
			inject(h.db)
			_, _, err := h.admit(plan)
			if !errors.Is(err, boom) {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if !h.db.empty() || h.db.rollbacks != 1 || h.db.commits != 0 {
				t.Errorf("%s: a failed admission left state: %+v", name, h.db)
			}
			if h.db.first("bind:snapshot") < 0 || h.db.first("exec:runs") < 0 {
				t.Errorf("%s: the failure did not follow binding and the run row: %v", name, h.db.trace)
			}
			failedEvents := h.eventIDs()
			h.db.failTable, h.db.failEventAt = map[string]error{}, 0
			run, created, err := h.admit(plan)
			if err != nil || !created || run.ID != plan.Aggregate.Run.ID {
				t.Errorf("%s: retry: %v", name, err)
				continue
			}
			if len(h.db.runs) != 1 || h.db.runs[0].ID != string(plan.Aggregate.Run.ID) || h.db.runs[0].Digest != plan.Aggregate.Run.SnapshotDigest || len(h.db.pins) != 2 {
				t.Errorf("%s: the retry changed the prepared identity: %+v %v", name, h.db.runs, h.db.pins)
			}
			for _, id := range failedEvents {
				found := false
				for _, c := range h.db.events {
					found = found || c == id
				}
				if !found {
					t.Errorf("%s: event %s of the failed attempt is not an event of the retry", name, id)
				}
			}
		}
	})

	t.Run("an unknown commit outcome is unavailable, not an acknowledgment", func(t *testing.T) {
		h := newHarness(t, newTx(s1))
		plan := prepareWork(t, h, &fakeResolver{snap: s1}, def)
		h.db.commitErr = errors.New("connection reset")
		if _, _, err := h.admit(plan); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		if h.db.commits != 0 {
			t.Fatal("a failed commit was counted")
		}
	})

	t.Run("revalidation stops at the first refused pin", func(t *testing.T) {
		h := newHarness(t, newTx(s1))
		plan := prepareWork(t, h, &fakeResolver{snap: s1}, def)
		second := snapshot(t, scope, map[string]string{"REPLICAS": "9"}, 1)
		run := plan.Aggregate.Run
		run.Snapshot.Environments = append(run.Snapshot.Environments, execution.StepEnvironment{Step: "other", Pin: second.Pin()})
		b := plan.Environments[0]
		b.Pin = second.Pin()
		plan.Environments = []execution.EnvironmentBinding{b, plan.Environments[0]}
		tx := newTx(s1) // the second pin is not retained
		if err := execution.BindEnvironments(ctx, bind(t, tx), plan); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("%v", err)
		}
		if len(tx.calls) != 2 { // authorize+snapshot of the first; nothing after the failure
			t.Fatalf("calls %v", tx.calls)
		}
		// An unbound pin cannot be skipped by omitting its binding.
		plan.Environments = plan.Environments[1:]
		if err := execution.BindEnvironments(ctx, bind(t, newTx(s1)), plan); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("an unbound pin was admitted: %v", err)
		}
	})
}

// ---- cleanup_retained_configuration --------------------------------------------------

func Test_cleanup_retained_configuration(t *testing.T) {
	ctx := context.Background()
	s1 := snapshot(t, scope, map[string]string{"REPLICAS": "1"}, 1)
	dep := deployment(t, s1)
	depID := string(dep.ID())
	def := definition(t, "teardown", cleanStep)
	cmd := execution.CleanupCommand{Scope: scope, DeploymentID: depID, DefinitionID: def.ID}

	// The environment has since been edited and its credential rotated. Cleanup
	// must still use what the deployment pinned.
	edited := &fakeResolver{snap: snapshot(t, scope, map[string]string{"REPLICAS": "7"}, 2)}
	viewer := func(d delivery.Deployment, s delivery.ConfigurationSnapshot) *fakeViewer {
		return &fakeViewer{view: delivery.CleanupView{Deployment: d, Snapshot: s}}
	}
	cleanupEnv := func(t *testing.T, v *fakeViewer) execution.DeploymentEnvironments {
		t.Helper()
		c, err := adapters.NewCleanupEnvironments(v)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	prepareOn := func(t *testing.T, st execution.Store, v *fakeViewer) (*execution.AdmissionPlan, error) {
		t.Helper()
		return service(t, wiring{resolver: resolverFor(t, edited), cleanup: cleanupAllowed{}, cleanupEnv: cleanupEnv(t, v), definition: def, store: st}).PrepareCleanup(ctx, cmd)
	}
	harnessFor := func(t *testing.T) *harness {
		t.Helper()
		tx := newTx(s1)
		tx.deployments[dep.ID()] = dep
		return newHarness(t, tx)
	}

	t.Run("production cleanup admission binds the retained configuration and registers its exact pins", func(t *testing.T) {
		h := harnessFor(t)
		plan, err := prepareOn(t, h.store, viewer(dep, s1))
		if err != nil {
			t.Fatal(err)
		}
		run := plan.Aggregate.Run
		if run.Purpose != execution.PurposeCleanup || run.DeploymentID != depID || run.WorkItemID != "" || run.WorkspaceID != "" ||
			len(run.Snapshot.Environments) != 1 || run.Snapshot.Environments[0].Pin != s1.Pin() || edited.calls != 0 {
			t.Fatalf("%+v (current environment consulted %d times)", run.Snapshot.Environments, edited.calls)
		}
		if len(plan.Environments) != 1 || plan.Environments[0].Deployment != depID || plan.Environments[0].Actor != execution.System || plan.Environments[0].Pin != s1.Pin() {
			t.Fatalf("binding %+v", plan.Environments)
		}
		for _, sec := range run.Snapshot.Secrets {
			if sec.Binding.Ref == kubeRef && sec.Binding.Version != 1 {
				t.Fatalf("the rotated credential version replaced the retained one: %+v", sec)
			}
		}
		admitted, created, err := h.admit(plan)
		if err != nil || !created {
			t.Fatalf("%v created=%v", err, created)
		}
		d := h.db
		if len(h.reader.calls) != 2 || h.reader.calls[0] != "snapshot" || h.reader.calls[1] != "deployment" {
			t.Fatalf("the transaction did not recheck the retained snapshot and the deployment: %v", h.reader.calls)
		}
		if bound, insert := d.first("bind:deployment"), d.first("exec:runs"); bound < 0 || bound > insert {
			t.Fatalf("binding must precede the run row: %v", d.trace)
		}
		want := []string{string(admitted.ID) + "|" + kubeRef + "@1", string(admitted.ID) + "|" + tokenRef + "@3"}
		if len(d.pins) != 2 || d.pins[0] != want[0] || d.pins[1] != want[1] {
			t.Fatalf("a cleanup run did not register the exact retained pins: %v want %v", d.pins, want)
		}
		if len(d.runs) != 1 || d.runs[0].Purpose != "cleanup" || d.runs[0].Deployment != depID || d.committedTable("workspace") != 0 || d.first("work-lock") >= 0 ||
			d.first("authority:") >= 0 || d.commits != 1 {
			t.Fatalf("cleanup run %+v trace %v", d.runs, d.trace)
		}
	})

	t.Run("a store without transactional environment support refuses environment-backed cleanup", func(t *testing.T) {
		// The in-memory store neither binds environments nor registers a
		// cleanup run's pins, so it must not be offered this path.
		if _, ok := any((*memory.Store)(nil)).(execution.EnvironmentStore); ok {
			t.Fatal("the in-memory store claims transactional environment support")
		}
		v := viewer(dep, s1)
		for name, st := range map[string]execution.Store{
			"plain store":              plainStore{},
			"postgres without binding": newHarness(t, nil).store,
		} {
			_, err := prepareOn(t, st, v)
			if !errors.Is(err, execution.ErrNotConfigured) || v.calls != 0 {
				t.Errorf("%s: %v (retained configuration read %d times)", name, err, v.calls)
			}
		}
		// The supported store enables the same wiring.
		h := harnessFor(t)
		if !h.store.AdmitsEnvironments() {
			t.Fatal("the bound PostgreSQL store reports no support")
		}
		if _, err := prepareOn(t, h.store, v); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("without the retained-configuration binding cleanup of an environment still fails closed", func(t *testing.T) {
		h := harnessFor(t)
		svc := service(t, wiring{cleanup: cleanupAllowed{}, definition: def, store: h.store})
		if _, err := svc.PrepareCleanup(ctx, cmd); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("%v", err)
		}
		if _, err := adapters.NewCleanupEnvironments(nil); err == nil {
			t.Fatal("constructor accepted no delivery service")
		}
		// An environment-free cleanup is unaffected by the new port and store capability.
		bare := definition(t, "bare", "{id: t, kind: container, workspace: {access: none}, image: \""+pipelinetest.Image+"\", command: [\"true\"], timeoutSeconds: 60}")
		plan, err := service(t, wiring{cleanup: cleanupAllowed{}, definition: bare}).PrepareCleanup(ctx, execution.CleanupCommand{Scope: scope, DeploymentID: depID, DefinitionID: bare.ID})
		if err != nil || len(plan.Environments) != 0 {
			t.Fatalf("%v", err)
		}
	})

	t.Run("an unavailable or inconsistent retained configuration fails closed", func(t *testing.T) {
		otherSnap := snapshot(t, scope, map[string]string{"REPLICAS": "5"}, 1)
		foreign := snapshot(t, other, map[string]string{"REPLICAS": "1"}, 1)
		notRequested := func() delivery.Deployment {
			rec := dep.Record()
			rec.CleanupRequestedAt, rec.CleanupState = nil, ""
			d, err := delivery.RestoreDeployment(rec)
			if err != nil {
				t.Fatal(err)
			}
			return d
		}()
		for name, tc := range map[string]struct {
			v    *fakeViewer
			want error
		}{
			"snapshot missing":            {&fakeViewer{err: delivery.ErrNotFound}, execution.ErrAdmissionRefused},
			"credential unavailable":      {&fakeViewer{err: delivery.ErrCredentialUnavailable}, execution.ErrAdmissionRefused},
			"delivery outage":             {&fakeViewer{err: errors.New("database down")}, execution.ErrUnavailable},
			"digest not covering content": {viewer(dep, altered(s1)), execution.ErrAdmissionRefused},
			"another snapshot":            {viewer(dep, otherSnap), execution.ErrAdmissionRefused},
			"foreign scope":               {viewer(dep, foreign), execution.ErrAdmissionRefused},
			"cleanup not requested":       {viewer(notRequested, s1), execution.ErrAdmissionRefused},
			"already deleted":             {viewer(withDeleted(t, dep), s1), execution.ErrAdmissionRefused},
		} {
			plan, err := prepareOn(t, harnessFor(t).store, tc.v)
			if !errors.Is(err, tc.want) || plan != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("a cleanup step may declare only the deployment's environment", func(t *testing.T) {
		rec := environment(t, scope, map[string]string{"REPLICAS": "1"}).Record()
		rec.ID = "env_production"
		prod, err := delivery.NewEnvironment(rec, t0)
		if err != nil {
			t.Fatal(err)
		}
		prodSnap, err := delivery.NewSnapshot(prod, map[string]int64{kubeRef: 1, tokenRef: 3}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prepareOn(t, harnessFor(t).store, viewer(deployment(t, prodSnap), prodSnap)); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a definition declaring another environment was admitted: %v", err)
		}
	})

	t.Run("authority is rechecked in the admission transaction and a refusal rolls everything back", func(t *testing.T) {
		other2 := snapshot(t, scope, map[string]string{"REPLICAS": "5"}, 1)
		for name, tc := range map[string]struct {
			mutate func(*txReader)
			want   error
		}{
			"cleanup completed meanwhile": {func(r *txReader) { r.deployments[dep.ID()] = withDeleted(t, dep) }, execution.ErrDenied},
			"deployment gone":             {func(r *txReader) { delete(r.deployments, dep.ID()) }, execution.ErrAdmissionRefused},
			"deployment now pins another": {func(r *txReader) { r.deployments[dep.ID()] = deployment(t, other2) }, execution.ErrAdmissionRefused},
			"retained snapshot missing":   {func(r *txReader) { delete(r.snapshots, key(s1.Environment, s1.Digest)) }, execution.ErrAdmissionRefused},
			"deployment read outage":      {func(r *txReader) { r.depErr = errors.New("database down") }, execution.ErrUnavailable},
			"snapshot read outage":        {func(r *txReader) { r.snapErr = errors.New("database down") }, execution.ErrUnavailable},
			"credential versions diverged": {func(r *txReader) {
				r.snapshots[key(s1.Environment, s1.Digest)] = withDigest(snapshot(t, scope, map[string]string{"REPLICAS": "1"}, 2), s1.Digest)
			}, execution.ErrAdmissionRefused},
		} {
			h := harnessFor(t)
			plan, err := prepareOn(t, h.store, viewer(dep, s1))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(h.reader)
			if _, _, err := h.admit(plan); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
			if !h.wroteNothing() || !h.db.empty() || h.db.rollbacks != 1 {
				t.Errorf("%s: wrote before binding: %v", name, h.db.trace)
			}
		}
		// A credential revoked after revalidation refuses the exact pin and
		// rolls back the run and the pins already registered; repair then retry.
		for _, ref := range []string{kubeRef, tokenRef} {
			h := harnessFor(t)
			plan, err := prepareOn(t, h.store, viewer(dep, s1))
			if err != nil {
				t.Fatal(err)
			}
			h.db.revoked[ref] = true
			if _, _, err := h.admit(plan); !errors.Is(err, execution.ErrAdmissionRefused) || !h.db.empty() || h.db.rollbacks != 1 || h.db.first("exec:runs") < 0 {
				t.Fatalf("%s: %v %v", ref, err, h.db.trace)
			}
			delete(h.db.revoked, ref)
			run, created, err := h.admit(plan)
			if err != nil || !created || run.ID != plan.Aggregate.Run.ID || len(h.db.pins) != 2 {
				t.Fatalf("%s retry: %v", ref, err)
			}
		}
		// A failed event append after the pins rolls the cleanup run back too.
		h := harnessFor(t)
		plan, err := prepareOn(t, h.store, viewer(dep, s1))
		if err != nil {
			t.Fatal(err)
		}
		boom := errors.New("event store down")
		h.db.failEventAt, h.db.eventErr = 1, boom
		if _, _, err := h.admit(plan); !errors.Is(err, boom) || !h.db.empty() || h.db.rollbacks != 1 || h.db.first("pin:"+tokenRef) < 0 {
			t.Fatalf("%v %v", err, h.db.trace)
		}
	})
}
