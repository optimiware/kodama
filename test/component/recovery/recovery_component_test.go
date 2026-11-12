//go:build component

// Package recoveryconformance is the real-boundary conformance suite for
// the installation fence, backup and restore drivers.
//
// Every test drives real PostgreSQL and (where the scenario needs it) real
// JetStream and a real Podman API service. Destructive restore drills are out
// of scope; this suite restores only into a fresh, isolated shard.
package recoveryconformance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	recovery "github.com/optimiware/kodama/internal/shell/app/drivers/recovery"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	recoverysupport "github.com/optimiware/kodama/test/support/infrastructure/recovery"
	"github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestMain installs the harness signal handler and the label-scoped orphan
// sweep. The harness also supplies the 15-minute shard timeout and the
// 1 CPU / 1 GiB container bound.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m))
}

// TestDurableRoundTrip backs up every durable family, restores it into a fresh
// shard and compares exact state. The disposable workspace is excluded from the
// backup and is reported as unrecoverable after the restore.
func TestDurableRoundTrip(t *testing.T) {
	source := recoverysupport.Start(t)
	wantDurable := recoverysupport.SeedDurable(t, source.Postgres)
	recoverysupport.WriteDurableFiles(t, source)

	source.NATS.EnsureStreams(t)
	source.NATS.PublishEvents(t, 3)
	source.NATS.AckFirst(t, 2)
	source.StopNATS(t)

	backupDriver, err := recovery.NewBackupDriver(recovery.BackupConfig{
		Executor:  source.Executor,
		Container: source.Postgres.Container.Name,
		Database:  source.Postgres.Database,
		Root:      source.BackupRoot,
		Sources: []recovery.BackupSource{
			{Name: "retained-logs", Root: source.RetainedLogsDir},
			{Name: "keys", Root: source.KeysDir},
			{Name: "jetstream", Root: source.JetStreamDir, Ready: func(ctx context.Context) error {
				running, err := runtimesupport.ContainerRunning(ctx, source.NATS.Container.Name)
				if err != nil {
					return err
				}
				if running {
					return errors.New("jetstream store is still being written")
				}
				return nil
			}},
		},
		Workspaces: []recovery.Workspace{{ID: recoverysupport.WorkspaceID, Root: source.WorkspaceRoot}},
		Preserved: func(ctx context.Context) (installation.PreservedState, error) {
			return recoverysupport.Preserved(ctx, source.Postgres)
		},
		NewIdentity: func() string { return "backup-i02f" },
	})
	if err != nil {
		t.Fatalf("NewBackupDriver: %v", err)
	}

	backup, err := (installation.Recovery{Backup: backupDriver}).RunBackup(
		t.Context(), installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	if backup.Identity != (installation.BackupIdentity{ID: "backup-i02f"}) {
		t.Fatalf("backup identity = %+v; want backup-i02f", backup.Identity)
	}
	if backup.Scope != installation.DefaultBackupScope() {
		t.Fatalf("backup scope = %+v; want the default scope", backup.Scope)
	}
	setDir := filepath.Join(source.BackupRoot, "backup-i02f")
	recoverysupport.RequireTreeLacks(t, setDir, recoverysupport.WorkspaceSentinel)
	recoverysupport.RequireTreeLacks(t, setDir, "disposable workspace bytes")

	target := recoverysupport.StartTarget(t)
	restoreDriver, err := recovery.NewRestoreDriver(recovery.RestoreConfig{
		Executor:  target.Executor,
		Container: target.Postgres.Container.Name,
		Database:  target.Postgres.Database,
		Root:      source.BackupRoot,
		Targets: []recovery.RestoreTarget{
			{Name: "retained-logs", Root: target.RetainedLogsDir},
			{Name: "keys", Root: target.KeysDir},
			{Name: "jetstream", Root: target.JetStreamDir},
		},
		Services: []string{"controller", "publisher"},
	})
	if err != nil {
		t.Fatalf("NewRestoreDriver: %v", err)
	}
	dispatch := &recordingDispatch{}
	restored, err := (installation.Recovery{
		Restore:  restoreDriver,
		Fence:    noopFence{},
		Dispatch: dispatch,
	}).RunRestore(t.Context(), backup.Identity, installation.RestorePlan{Scope: backup.Scope})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if !restored.Fenced || !restored.Reconciled {
		t.Fatalf("restore evidence fenced=%v reconciled=%v; want both true", restored.Fenced, restored.Reconciled)
	}
	if dispatch.calls != 1 {
		t.Fatalf("dispatch calls = %d; want 1", dispatch.calls)
	}
	wantPreserved := installation.PreservedState{Receipts: 1, Decisions: 1, QueuedExecutions: 1}
	if restored.Preserved != wantPreserved {
		t.Fatalf("preserved = %+v; want %+v", restored.Preserved, wantPreserved)
	}
	if !reflect.DeepEqual(restored.UnrecoverableWorkspaces, []string{recoverysupport.WorkspaceID}) {
		t.Fatalf("unrecoverable workspaces = %v; want [%s]", restored.UnrecoverableWorkspaces, recoverysupport.WorkspaceID)
	}
	if !reflect.DeepEqual(restored.Services, []string{"controller", "publisher"}) {
		t.Fatalf("services = %v; want [controller publisher]", restored.Services)
	}

	recoverysupport.RequireDurableEqual(t, wantDurable, recoverysupport.SnapshotDurable(t, target.Postgres))
	recoverysupport.RequireTreesEqual(t, source.RetainedLogsDir, target.RetainedLogsDir)
	recoverysupport.RequireTreesEqual(t, source.KeysDir, target.KeysDir)
	recoverysupport.RequireTreeEmpty(t, target.WorkspaceRoot)

	// The restored database must be usable by the same non-owner roles as the
	// source. The read probes run first so a missing grant fails as the
	// permission error, not merely a catalog difference.
	roleReads := []struct {
		role      string
		columns   int
		statement string
	}{
		{role: "kodama_runtime", columns: 1, statement: `SELECT count(*) FROM executions`},
		{role: "kodama_controller", columns: 1, statement: `SELECT count(*) FROM executions`},
		{role: "kodama_publisher", columns: 1, statement: `SELECT count(*) FROM events`},
		{role: "kodama_auth", columns: 4, statement: `SELECT id, tenant_id, project_id, principal_id FROM executions ORDER BY id`},
	}
	for _, read := range roleReads {
		want := recoverysupport.RoleRows(t, source.Postgres, read.role, read.columns, read.statement)
		got := recoverysupport.RoleRows(t, target.Postgres, read.role, read.columns, read.statement)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("role %s read %q differs after restore\nwant: %v\ngot:  %v",
				read.role, read.statement, want, got)
		}
	}
	// Object ownership and every table/column grant must survive too, not just
	// the reads this fixture happens to exercise. --no-owner and
	// --no-privileges would leave the administrator as owner and the runtime,
	// controller, publisher and auth roles without their declared grants.
	recoverysupport.RequireRoleACLEqual(t, source.Postgres, target.Postgres)
	// SET ROLE from an administrator session must also work. The runtime
	// policy scopes rows to the transaction setting, so only the statement
	// succeeding (not a row count) is asserted here.
	admin := target.Postgres.Admin()
	conn, err := admin.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin set-role probe: %v", err)
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := conn.Exec(t.Context(), `SET LOCAL ROLE kodama_runtime`); err != nil {
		t.Fatalf("SET LOCAL ROLE kodama_runtime: %v", err)
	}
	rows, err := conn.Query(t.Context(), `SELECT count(*) FROM executions`)
	if err != nil {
		t.Fatalf("runtime role read after restore: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close runtime role read: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("runtime role read after restore: %v", err)
	}

	target.StartNATS(t)
	if got := target.NATS.StreamMessages(t); got != 3 {
		t.Fatalf("restored stream messages = %d; want 3", got)
	}
	if got := target.NATS.AckFloor(t); got != 2 {
		t.Fatalf("restored consumer ack floor = %d; want 2", got)
	}
}

// TestReconcileBeforeDispatch proves the real fence stops the inventoried
// controller, reconciliation observes stable workload identities through real
// Podman, an unknown effect blocks dispatch, and nothing is launched or
// dispatched before reconciliation completes.
func TestReconcileBeforeDispatch(t *testing.T) {
	shard := recoverysupport.StartTarget(t)
	libpod := shard.Libpod(t)
	image := shard.AlpineImage(t)

	controller := shard.RunWorkload(t, image, shard.Harness.Name("i02f-controller"), "sleep", "60")
	stopped := shard.RunWorkload(t, image, shard.Harness.Name("i02f-workload-stopped"), "sh", "-c", "exit 0")
	recoverysupport.WaitContainerStopped(t, libpod, stopped.Name)

	observer := &recoverysupport.ContainerObserver{
		Libpod: libpod,
		Names: map[string]string{
			"workload-stopped": stopped.Name,
			"workload-missing": shard.Harness.Name("i02f-workload-missing"),
		},
	}
	scripted := recoverysupport.NewScriptedObserver(observer)
	dispatch := &recordingDispatch{}
	guard := &dispatchGuard{base: scripted, dispatch: dispatch}
	executor := &countingExecutor{inner: shard.Executor}

	restoreDriver, err := recovery.NewRestoreDriver(recovery.RestoreConfig{
		Executor:  executor,
		Container: shard.Postgres.Container.Name,
		Database:  shard.Postgres.Database,
		Root:      shard.BackupRoot,
		Targets: []recovery.RestoreTarget{
			{Name: "retained-logs", Root: shard.RetainedLogsDir},
			{Name: "keys", Root: shard.KeysDir},
			{Name: "jetstream", Root: shard.JetStreamDir},
		},
		Workloads: []recovery.WorkloadIdentity{
			{ID: "workload-stopped"},
			{ID: "workload-missing"},
			{ID: "workload-unknown"},
		},
		Observer: guard,
		Services: []string{"controller"},
	})
	if err != nil {
		t.Fatalf("NewRestoreDriver: %v", err)
	}
	fence, err := recovery.NewFenceDriver(recovery.FenceConfig{
		Libpod: libpod,
		Inventory: []recovery.Actor{
			{Kind: recovery.ActorController, Name: controller.Name},
		},
	})
	if err != nil {
		t.Fatalf("NewFenceDriver: %v", err)
	}

	runner := installation.Recovery{
		Restore:  reconcileOnly{driver: restoreDriver},
		Fence:    fence,
		Dispatch: dispatch,
	}

	// First pass: the scripted identity is unobservable, so reconciliation is
	// unresolved and dispatch stays blocked.
	scripted.SetUnknown("workload-unknown", true)
	if _, err := runner.RunRestore(t.Context(),
		installation.BackupIdentity{ID: "backup-i02f"},
		installation.RestorePlan{Scope: installation.DefaultBackupScope()}); err == nil {
		t.Fatal("RunRestore dispatched with an unresolved external effect")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
		t.Fatalf("unresolved reconciliation class = %v; want uncertain_external_effect (%v)", class, err)
	}
	if dispatch.calls != 0 {
		t.Fatalf("dispatch calls after unresolved reconciliation = %d; want 0", dispatch.calls)
	}
	recoverysupport.WaitContainerStopped(t, libpod, controller.Name)

	// Second pass: every identity is observable, so dispatch is enabled once.
	scripted.SetUnknown("workload-unknown", false)
	restored, err := runner.RunRestore(t.Context(),
		installation.BackupIdentity{ID: "backup-i02f"},
		installation.RestorePlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if dispatch.calls != 1 {
		t.Fatalf("dispatch calls = %d; want 1", dispatch.calls)
	}
	if !restored.Reconciled {
		t.Fatal("restore evidence did not report reconciliation")
	}
	if !reflect.DeepEqual(restored.UnrecoverableWorkspaces, []string{"workspace-missing"}) {
		t.Fatalf("unrecoverable workspaces = %v; want [workspace-missing]", restored.UnrecoverableWorkspaces)
	}
	if guard.violations != 0 {
		t.Fatalf("dispatch ran before reconciliation %d times; want 0", guard.violations)
	}
	if executor.calls != 0 {
		t.Fatalf("reconciliation made %d executor calls; want 0 (never launch or replay)", executor.calls)
	}
}

// noopFence is the fence stand-in for the durable round trip, which exercises
// the backup/restore path rather than the Podman fence.
type noopFence struct{}

func (noopFence) Fence(context.Context) error { return nil }

// recordingDispatch records the single dispatch transition.
type recordingDispatch struct {
	calls int
}

func (d *recordingDispatch) EnableDispatch(context.Context) error {
	d.calls++
	return nil
}

// countingExecutor records whether the reconcile path ever touched the
// containerized restore boundary.
type countingExecutor struct {
	inner recovery.Executor
	calls int
}

func (e *countingExecutor) Exec(ctx context.Context, request recovery.ExecRequest) (recovery.ExecResult, error) {
	e.calls++
	return e.inner.Exec(ctx, request)
}

// dispatchGuard fails the scenario if dispatch has already run when an
// observation is requested.
type dispatchGuard struct {
	base       recovery.Observer
	dispatch   *recordingDispatch
	violations int
}

func (g *dispatchGuard) Observe(ctx context.Context, identity recovery.WorkloadIdentity) (recovery.WorkloadObservation, error) {
	if g.dispatch.calls != 0 {
		g.violations++
	}
	return g.base.Observe(ctx, identity)
}

// reconcileOnly supplies a successful preflight and an inert restore while
// delegating reconciliation to the real RestoreDriver. It keeps the dispatch
// ordering scenario independent of a full backup set.
type reconcileOnly struct {
	driver *recovery.RestoreDriver
}

func (r reconcileOnly) Preflight(_ context.Context, identity installation.BackupIdentity) (installation.BackupEvidence, error) {
	return installation.BackupEvidence{
		Identity:          identity,
		Scope:             installation.DefaultBackupScope(),
		KeysPresent:       true,
		Preserved:         installation.PreservedState{Receipts: 1, Decisions: 1, QueuedExecutions: 1},
		MissingWorkspaces: []string{"workspace-missing"},
	}, nil
}

func (reconcileOnly) Restore(context.Context, installation.RestorePlan) (installation.RestoreResult, error) {
	return installation.RestoreResult{}, nil
}

func (r reconcileOnly) Reconcile(ctx context.Context) (installation.ReconcileResult, error) {
	return r.driver.Reconcile(ctx)
}
