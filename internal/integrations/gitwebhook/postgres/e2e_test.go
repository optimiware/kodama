package postgres_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	gwpg "github.com/optimiware/kodama/internal/integrations/gitwebhook/postgres"
	"github.com/optimiware/kodama/internal/workspace"
)

// sqlWork reads the work item's content revision for the planner, as the core's
// work module does for run admission.
type sqlWork struct {
	e    *env
	item executiontest.Project
}

func (w sqlWork) Snapshot(_ context.Context, scope domain.Scope, id domain.WorkItemID) (execution.WorkSnapshot, error) {
	if scope != w.item.Scope || id != w.item.Item {
		return execution.WorkSnapshot{}, execution.ErrNotFound
	}
	return execution.WorkSnapshot{ID: id, Actionable: true, ContentRevision: w.e.x.ContentRevision(w.item)}, nil
}

// TestPostgresTriggerRoundtrip runs the module's own Service over the PostgreSQL
// store and the admission adapter over the core's real execution admission:
// durable claim, admission lease, the core's run, the durable refusal ledger and
// recovery across a restart. Git and authorization remain the module's doubles
// (the real Git join is T-038's native_hook_roundtrip).
func TestPostgresTriggerRoundtrip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	scope := executiontest.ScopeA
	project := e.x.NewProject(scope)
	def := e.def(scope)
	secret := []byte(strings.Repeat("hook-secret-", 4))

	auth, secrets, git, repos := &gitwebhooktest.Authorizer{}, &gitwebhooktest.Secrets{}, &gitwebhooktest.Git{}, &gitwebhooktest.Repositories{}
	auth.Grant(executiontest.Engineer, scope, gw.ActionInstall)
	auth.Grant(executiontest.Engineer, scope, gw.ActionRun)
	secrets.Put(scope, "secret://project/hook", secret)
	repos.Set(scope, workspace.RepositoryConfig{ID: "repo_t020", URL: "https://git.example.invalid/t020.git", CheckoutSecretRef: "secret://project/checkout", PublisherSecretRef: "secret://project/publisher", BaseRef: "main"})
	git.SetBranch("refs/heads/main", workspace.BranchInfo{Tip: executiontest.CommitA, Exists: true, Contains: true})

	// build wires a "process": new connections, store, ledger, planner and service.
	build := func(t *testing.T) *gw.Service {
		pool := e.restartedPool()
		store, err := gwpg.NewStore(pool, gwpg.Options{})
		if err != nil {
			t.Fatal(err)
		}
		led, err := gwpg.NewLedger(pool, e.x.Store)
		if err != nil {
			t.Fatal(err)
		}
		planner, err := adapters.NewExecutionPlanner(adapters.PlannerConfig{Preparer: e.x.Svc, Work: sqlWork{e, project}, Repositories: repos, Workspaces: e.x.Workspaces})
		if err != nil {
			t.Fatal(err)
		}
		runs, err := adapters.NewRuns(planner, led)
		if err != nil {
			t.Fatal(err)
		}
		defs, err := adapters.NewDefinitions(e.x.Pipeline)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := gw.New(gw.Dependencies{Store: store, Authorizer: auth, Secrets: secrets, Definitions: defs, Repositories: repos, Git: git, Runs: runs})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	svc := build(t)
	trigger, err := svc.Install(ctx, gw.InstallCommand{
		Scope: scope, Principal: executiontest.Engineer, DefinitionID: def.ID, WorkItemID: project.Item, Branches: []string{"main"}, SecretRef: "secret://project/hook",
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	send := func(svc *gw.Service, delivery string) (gw.Receipt, error) {
		body := gitwebhooktest.Marshal(t, gw.Payload{SchemaVersion: gw.SchemaVersion, Repository: "repo_t020", Ref: "refs/heads/main", Before: gitwebhooktest.Zero, After: executiontest.CommitA})
		ts := time.Now().Unix()
		return svc.Receive(ctx, gw.Notification{
			TriggerID: trigger.ID, DeliveryID: delivery, Timestamp: strconv.FormatInt(ts, 10),
			Signature: gw.Sign(secret, trigger.ID, delivery, ts, body), Body: body,
		})
	}
	key := func(d string) string { return "git-hook:" + string(trigger.ID) + ":" + d }
	entry := func(d string) (adapters.Entry, bool) {
		led, _ := gwpg.NewLedger(e.x.Runtime, e.x.Store)
		en, found, err := led.Lookup(ctx, scope, key(d))
		if err != nil {
			t.Fatal(err)
		}
		return en, found
	}

	// d1: the installer's run authority is gone in the core (the trigger's own
	// authorizer still allows it), so admission refuses permanently.
	e.x.Revoke(scope, executiontest.Engineer)
	r, err := send(svc, "d1")
	if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonAdmissionRefused || r.RunID != "" {
		t.Fatalf("d1: %+v, %v", r, err)
	}
	if en, found := entry("d1"); !found || en.Refusal != gw.ReasonAdmissionRefused || en.RunID != "" {
		t.Fatalf("d1 ledger: %+v %v", en, found)
	}
	if n := e.runs(scope); n != 0 {
		t.Fatalf("%d runs after a refusal", n)
	}

	// Authority returns, the process restarts, d1 is redelivered: the refusal
	// stands, durably, and still creates nothing.
	e.regrant(scope)
	svc = build(t)
	r, err = send(svc, "d1")
	if !errors.Is(err, gw.ErrRejected) || !r.Duplicate || r.RunID != "" {
		t.Fatalf("d1 replay after restart: %+v, %v", r, err)
	}
	if n := e.runs(scope); n != 0 {
		t.Fatalf("%d runs after a replayed refusal", n)
	}

	// d2: a new delivery starts the core's exact-commit run.
	r, err = send(svc, "d2")
	if err != nil || r.Outcome != gw.OutcomeStarted || r.RunID == "" || r.Duplicate {
		t.Fatalf("d2: %+v, %v", r, err)
	}
	var commit, definition, item, requestedBy string
	if err := e.x.Migration.QueryRow(ctx, `SELECT source_commit, definition_id, work_item_id, requested_by_id FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(r.RunID)).Scan(&commit, &definition, &item, &requestedBy); err != nil {
		t.Fatalf("the run is not in the core's runs table: %v", err)
	}
	if commit != executiontest.CommitA || definition != string(def.ID) || item != string(project.Item) || requestedBy != executiontest.Engineer {
		t.Fatalf("run = commit %s definition %s item %s requested by %s", commit, definition, item, requestedBy)
	}
	if en, found := entry("d2"); !found || en.RunID != r.RunID || en.Refusal != "" {
		t.Fatalf("d2 ledger: %+v %v", en, found)
	}

	// Redelivery, after another restart and with the installer's authority
	// revoked, returns the same run and creates no other.
	e.x.Revoke(scope, executiontest.Engineer)
	again, err := send(build(t), "d2")
	if err != nil || !again.Duplicate || again.RunID != r.RunID || again.Outcome != gw.OutcomeStarted {
		t.Fatalf("d2 replay: %+v, %v", again, err)
	}
	if n := e.runs(scope); n != 1 {
		t.Fatalf("%d runs, want exactly the one d2 started", n)
	}

	// Success and refusal are exclusive in the database: the refused delivery
	// has no run, the started one no refusal, and both outcomes are durable.
	var rejected, started int
	_ = e.x.Migration.QueryRow(ctx, `SELECT count(*) FILTER (WHERE outcome = 'rejected' AND run_id IS NULL), count(*) FILTER (WHERE outcome = 'started' AND run_id IS NOT NULL)
FROM git_trigger_deliveries WHERE tenant_id = $1 AND trigger_id = $2 AND done`, string(scope.TenantID), string(trigger.ID)).Scan(&rejected, &started)
	if rejected != 1 || started != 1 {
		t.Fatalf("durable outcomes: %d rejected, %d started", rejected, started)
	}
	for action, want := range map[string]int{"git_trigger.installed": 1, "git_hook.rejected": 1, "git_hook.run_started": 1, "git_hook.duplicate": 2} {
		if n := e.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND resource_type = 'git_trigger' AND resource_id = $2 AND action = $3`,
			string(scope.TenantID), string(trigger.ID), action); n != want {
			t.Errorf("audit %s = %d, want %d", action, n, want)
		}
	}
	var leaked int
	_ = e.x.Migration.QueryRow(ctx, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND (metadata::text LIKE '%' || $2 || '%' OR coalesce(reason, '') LIKE '%' || $2 || '%')`,
		string(scope.TenantID), string(secret)).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("the hook secret appears in the audit trail")
	}
}

// TestPostgresTriggerRefusesWorkspaceOfAnotherRepository: the work item keeps a
// workspace of repository A (seeded by the execution fixture), while the project
// binding and the trigger now name repository B and the pushed commit exists in
// both. The core's source check passes (it follows the work item's workspace),
// so the planner must refuse permanently before the ledger admits anything.
func TestPostgresTriggerRefusesWorkspaceOfAnotherRepository(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	scope := executiontest.ScopeA
	project := e.x.NewProject(scope) // workspace repository: repo_t020 (A)
	def := e.def(scope)
	secret := []byte(strings.Repeat("hook-secret-", 4))

	auth, secrets, git, repos := &gitwebhooktest.Authorizer{}, &gitwebhooktest.Secrets{}, &gitwebhooktest.Git{}, &gitwebhooktest.Repositories{}
	auth.Grant(executiontest.Engineer, scope, gw.ActionInstall)
	auth.Grant(executiontest.Engineer, scope, gw.ActionRun)
	secrets.Put(scope, "secret://project/hook", secret)
	// The project is now bound to repository B.
	repos.Set(scope, workspace.RepositoryConfig{ID: "repo_b", URL: "https://git.example.invalid/b.git", CheckoutSecretRef: "secret://project/checkout", PublisherSecretRef: "secret://project/publisher", BaseRef: "main"})
	git.SetBranch("refs/heads/main", workspace.BranchInfo{Tip: executiontest.CommitA, Exists: true, Contains: true})

	pool := e.restartedPool()
	store, _ := gwpg.NewStore(pool, gwpg.Options{})
	led, _ := gwpg.NewLedger(pool, e.x.Store)
	planner, err := adapters.NewExecutionPlanner(adapters.PlannerConfig{Preparer: e.x.Svc, Work: sqlWork{e, project}, Repositories: repos, Workspaces: e.x.Workspaces})
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := adapters.NewRuns(planner, led)
	defs, _ := adapters.NewDefinitions(e.x.Pipeline)
	svc, err := gw.New(gw.Dependencies{Store: store, Authorizer: auth, Secrets: secrets, Definitions: defs, Repositories: repos, Git: git, Runs: runs})
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := svc.Install(ctx, gw.InstallCommand{Scope: scope, Principal: executiontest.Engineer, DefinitionID: def.ID, WorkItemID: project.Item, Branches: []string{"main"}, SecretRef: "secret://project/hook"})
	if err != nil {
		t.Fatal(err)
	}
	if trigger.RepositoryID != "repo_b" {
		t.Fatalf("trigger bound to %s", trigger.RepositoryID)
	}
	body := gitwebhooktest.Marshal(t, gw.Payload{SchemaVersion: gw.SchemaVersion, Repository: "repo_b", Ref: "refs/heads/main", Before: gitwebhooktest.Zero, After: executiontest.CommitA})
	ts := time.Now().Unix()
	r, err := svc.Receive(ctx, gw.Notification{TriggerID: trigger.ID, DeliveryID: "d1", Timestamp: strconv.FormatInt(ts, 10), Signature: gw.Sign(secret, trigger.ID, "d1", ts, body), Body: body})
	if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonAdmissionRefused || r.RunID != "" {
		t.Fatalf("receive = %+v, %v; want a recorded admission refusal", r, err)
	}
	if n := e.runs(scope); n != 0 {
		t.Fatalf("%d run(s) created against the other repository's workspace", n)
	}
	en, found, err := led.Lookup(ctx, scope, "git-hook:"+string(trigger.ID)+":d1")
	if err != nil || !found || en.RunID != "" || en.Refusal != gw.ReasonAdmissionRefused {
		t.Fatalf("ledger = %+v %v %v", en, found, err)
	}
}
