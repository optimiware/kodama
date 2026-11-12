//go:build qualification

package restorequal

// Q03 local restore drill. It drives the real I02f recovery harness (Podman,
// PostgreSQL, NATS/JetStream) and the I02c local registry, and records a
// timestamped recovery timeline. Run it only inside the host slot:
//
//	scripts/infra-slot.sh stack -- env KODAMA_REQUIRE_INFRA=1 \
//	    go test -race -tags=qualification -count=1 -timeout=30m ./test/qualification/restore/...
//
// Destructive stage drills are coordinator-owned; this file never contacts a
// cloud API.

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/installation"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	recovery "github.com/optimiware/kodama/internal/shell/app/drivers/recovery"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	"github.com/optimiware/kodama/test/support/infrastructure/messaging"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
	recoverysupport "github.com/optimiware/kodama/test/support/infrastructure/recovery"
)

// TestMain installs the harness signal handler and the label-scoped orphan
// sweep for the real Podman/PostgreSQL/NATS drills.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m))
}

// TestLocalRestore backs up every durable control-plane family, restores it into
// a fresh local stack and compares exact state. It records the measured
// timeline against RPO ≤15 minutes and RTO ≤4 hours and writes allowlisted
// evidence.
func TestLocalRestore(t *testing.T) {
	acquireRestoreSlot(t)
	drill := runLocalDrill(t)
	source, target := drill.Source, drill.Target

	recoverysupport.RequireDurableEqual(t, drill.Want, recoverysupport.SnapshotDurable(t, target.Postgres))
	recoverysupport.RequireTreesEqual(t, source.RetainedLogsDir, target.RetainedLogsDir)
	recoverysupport.RequireTreesEqual(t, source.KeysDir, target.KeysDir)
	recoverysupport.RequireTreeEmpty(t, target.WorkspaceRoot)
	recoverysupport.RequireTreeLacks(t, source.BackupRoot, recoverysupport.WorkspaceSentinel)

	target.StartNATS(t)
	if got := target.NATS.StreamMessages(t); got != 3 {
		t.Fatalf("restored stream messages = %d; want 3", got)
	}
	if got := target.NATS.AckFloor(t); got != 2 {
		t.Fatalf("restored consumer ack floor = %d; want 2", got)
	}

	// S10 §4: the external registry owns its bytes, so it is not part of the
	// control-plane backup. The retained artifact reference must still resolve
	// after the restore.
	registry := messaging.StartRegistry(t, false)
	if err := registry.Shard.WaitReady(registry.Shard.Context(), "registry after restore", harness.RegistryProbe(registry.BaseURL)); err != nil {
		t.Fatalf("registry is not usable after the restore: %v", err)
	}

	// The local drill's committed and restored watermarks are the same commit:
	// its rpo_ms is 0 by construction and is labelled local-same-commit, so it
	// asserts neither MeetsRPO nor a backup-age RPO measurement here. RPO and
	// RTO are recommendations (U14): a missed RTO is reported with the measured
	// timeline and written into the evidence (meets_rto=false), never treated as
	// a failed restore. A lost ID or a failed restore still fails above.
	if missed := drill.Assessment.MissedObjectives(); len(missed) > 0 {
		t.Logf("the local restore missed recommended objective(s) %v: measured RTO %v against %v; reporting the limitation instead of failing the restore",
			missed, drill.Assessment.RTO, drill.Assessment.Objectives.RTO)
	}
	if len(drill.Assessment.LostIDs) != 0 {
		t.Fatalf("lost IDs = %v; want none", drill.Assessment.LostIDs)
	}

	evidence, err := TimelineEvidence("local-restore", drill.RunID, "local-podman", string(OutcomeConfirmed), RPOBaseLocalSameCommit, drill.Timeline)
	if err != nil {
		t.Fatalf("TimelineEvidence: %v", err)
	}
	evidence.SourceRevision = os.Getenv("KODAMA_Q03_SOURCE_REVISION")
	evidence.UnrecoverableWorkspaces = append([]string(nil), drill.Result.UnrecoverableWorkspaces...)
	evidence.Images = []ImageRecord{
		{Tag: source.PostgresImageTag, Digest: source.PostgresImageDigest},
		{Tag: source.NATSImageTag, Digest: source.NATSImageDigest},
		{Tag: harness.ImageRegistry, Digest: registry.Digest},
	}
	recordDrillEvidence(t, "run-"+drill.RunID+".json", evidence)
}

// TestLocalRestoreRuntimeRoleAccess asserts that the restored database grants
// the non-owner runtime role read access and that the role sees the same
// tenant-scoped rows as the source database. Row-level security hides rows
// outside a tenant scope, so the permission proof is the unscoped read (which
// must not be "permission denied") and the data proof is the scoped read.
func TestLocalRestoreRuntimeRoleAccess(t *testing.T) {
	acquireRestoreSlot(t)
	drill := runLocalDrill(t)
	scope := recoverysupport.FixtureScope()
	ctx := t.Context()

	// Permission proof: the same unscoped query must not error. A stripped or
	// unrestored grant shows up as "permission denied for table receipts"
	// (SQLSTATE 42501) and fails here.
	unscoped, err := pgtest.QueryStringsN(ctx, drill.Target.Postgres.Runtime(2), 1, `SELECT count(*) FROM receipts`)
	if err != nil {
		t.Fatalf("runtime role has no read grant on restored receipts: %v", err)
	}
	if len(unscoped) != 1 || len(unscoped[0]) != 1 {
		t.Fatalf("unscoped runtime count = %v; want one count row", unscoped)
	}

	// Data proof: both runtime roles read the seeded receipt through the
	// fixture tenant/project scope, and the restored count matches the source.
	sourceCount := scopedReceiptCount(t, drill.Source.Postgres.Runtime(2), scope)
	targetCount := scopedReceiptCount(t, drill.Target.Postgres.Runtime(2), scope)
	if sourceCount < 1 {
		t.Fatalf("source scoped receipt count = %d; want the seeded receipt", sourceCount)
	}
	if targetCount != sourceCount {
		t.Fatalf("restored scoped receipt count = %d; want the source count %d", targetCount, sourceCount)
	}
}

// scopedReceiptCount reads the receipt count for one scope through the runtime
// role, using the product's transaction-local scope helper.
func scopedReceiptCount(t *testing.T, db *pgdriver.DB, scope tenancy.Scope) int64 {
	t.Helper()
	rows, err := pgtest.ScopedQuery(t.Context(), db, scope, 1, `SELECT count(*) FROM receipts`)
	if err != nil {
		t.Fatalf("scoped receipt count: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("scoped receipt count = %v; want one count row", rows)
	}
	count, err := strconv.ParseInt(rows[0][0], 10, 64)
	if err != nil {
		t.Fatalf("parse scoped receipt count %q: %v", rows[0][0], err)
	}
	return count
}

// localDrill is one completed local restore with its measured timeline.
type localDrill struct {
	RunID      string
	Source     *recoverysupport.Shard
	Target     *recoverysupport.Shard
	Want       recoverysupport.DurableSnapshot
	Result     installation.RestoreResult
	Timeline   *Timeline
	Assessment Assessment
}

// runLocalDrill performs the durable backup and restore. Every milestone is
// recorded through the Timeline's injected clock so the measured RPO/RTO is
// derived from the same timestamps the evidence carries.
func runLocalDrill(t *testing.T) *localDrill {
	t.Helper()
	timeline, err := NewTimeline(func() time.Time { return time.Now().UTC() }, DefaultObjectives())
	if err != nil {
		t.Fatalf("NewTimeline: %v", err)
	}
	runID, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	if _, err := timeline.Mark(MilestoneBackupStarted); err != nil {
		t.Fatalf("Mark(backup.started): %v", err)
	}

	source := recoverysupport.Start(t)
	wantDurable := recoverysupport.SeedDurable(t, source.Postgres)
	recoverysupport.WriteDurableFiles(t, source)
	source.NATS.EnsureStreams(t)
	source.NATS.PublishEvents(t, 3)
	source.NATS.AckFirst(t, 2)

	committedAt := time.Now().UTC()
	if err := timeline.Commit(Watermark{At: committedAt, IDs: durableIDs(wantDurable)}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	source.StopNATS(t)

	backupDriver, err := recovery.NewBackupDriver(recovery.BackupConfig{
		Executor:  source.Executor,
		Container: source.Postgres.Container.Name,
		Database:  source.Postgres.Database,
		Root:      source.BackupRoot,
		Sources: []recovery.BackupSource{
			{Name: "retained-logs", Root: source.RetainedLogsDir},
			{Name: "keys", Root: source.KeysDir},
			{Name: "jetstream", Root: source.JetStreamDir, Ready: func(context.Context) error { return nil }},
		},
		Workspaces: []recovery.Workspace{{ID: recoverysupport.WorkspaceID, Root: source.WorkspaceRoot}},
		Preserved: func(ctx context.Context) (installation.PreservedState, error) {
			return recoverysupport.Preserved(ctx, source.Postgres)
		},
		NewIdentity: func() string { return "backup-" + runID },
	})
	if err != nil {
		t.Fatalf("NewBackupDriver: %v", err)
	}
	backup, err := (installation.Recovery{Backup: backupDriver}).RunBackup(
		t.Context(), installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	if _, err := timeline.Mark(MilestoneBackupCompleted); err != nil {
		t.Fatalf("Mark(backup.completed): %v", err)
	}
	if _, err := timeline.Mark(MilestoneFailureInjected); err != nil {
		t.Fatalf("Mark(failure.injected): %v", err)
	}
	if _, err := timeline.Mark(MilestoneRestoreStarted); err != nil {
		t.Fatalf("Mark(restore.started): %v", err)
	}

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
	result, err := (installation.Recovery{
		Restore:  restoreDriver,
		Fence:    localNoopFence{},
		Dispatch: localRecordingDispatch{},
	}).RunRestore(t.Context(), backup.Identity, installation.RestorePlan{Scope: backup.Scope})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if _, err := timeline.Mark(MilestoneRestoreCompleted); err != nil {
		t.Fatalf("Mark(restore.completed): %v", err)
	}
	if _, err := timeline.Mark(MilestoneServiceResumed); err != nil {
		t.Fatalf("Mark(service.resumed): %v", err)
	}

	restored := recoverysupport.SnapshotDurable(t, target.Postgres)
	if err := timeline.Restore(Watermark{At: committedAt, IDs: durableIDs(restored)}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assessment, err := timeline.Assess()
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	return &localDrill{
		RunID:      runID,
		Source:     source,
		Target:     target,
		Want:       wantDurable,
		Result:     result,
		Timeline:   timeline,
		Assessment: assessment,
	}
}

// durableIDs returns every durable row ID in a stable order. The snapshot
// queries already order by id, so the concatenation is deterministic.
func durableIDs(snapshot recoverysupport.DurableSnapshot) []string {
	var ids []string
	for _, family := range [][][]string{
		snapshot.Bindings,
		snapshot.Verifications,
		snapshot.Pipelines,
		snapshot.Conversations,
		snapshot.Decisions,
		snapshot.Receipts,
		snapshot.QueuedExecutions,
		snapshot.Events,
	} {
		for _, row := range family {
			if len(row) > 0 {
				ids = append(ids, row[0])
			}
		}
	}
	return ids
}

type localNoopFence struct{}

func (localNoopFence) Fence(context.Context) error { return nil }

type localRecordingDispatch struct{}

func (localRecordingDispatch) EnableDispatch(context.Context) error { return nil }
