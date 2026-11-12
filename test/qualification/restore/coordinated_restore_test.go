//go:build qualification

package restorequal

// Q03 coordinated local restore drill.
//
// The drill composes the durable control-plane families the product backup
// driver owns (PostgreSQL, retained logs, encryption keys, JetStream) with the
// service-owned state S10 §4 says to coordinate and never copy behind an
// owner's back: a Git repository, its durable receive journal, and a checkpoint
// store. It also fences live old actors through the real Podman seam, reconciles
// explicit workload identities before enabling dispatch, and reports the
// disposable workspace as unrecoverable instead of replaying it.
//
// The stage version of this drill runs later in the exclusive stage slot
// (scripts/q03-stage-drill); this file contacts only local Podman containers,
// the local registry and the host Git binary.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	recovery "github.com/optimiware/kodama/internal/shell/app/drivers/recovery"
	gitpkg "github.com/optimiware/kodama/internal/userland/git"
	sessionpkg "github.com/optimiware/kodama/internal/userland/session"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	"github.com/optimiware/kodama/test/support/infrastructure/messaging"
	recoverysupport "github.com/optimiware/kodama/test/support/infrastructure/recovery"
)

// coordinatedDrill is one completed coordinated restore and its measured
// timeline.
type coordinatedDrill struct {
	RunID      string
	Source     *recoverysupport.Shard
	Target     *recoverysupport.Shard
	Want       recoverysupport.DurableSnapshot
	Result     installation.RestoreResult
	Timeline   *Timeline
	Assessment Assessment

	GitTenant     id.TenantID
	GitRepository string
	GitRetained   string
	GitCandidate  string
	GitExpected   string
	JournalKey    string
	CheckpointRef sessionpkg.Ref
	RawContent    string
	RawWithheld   sessionpkg.Ref
	Artifact      string
	Registry      *messaging.Registry
}

// TestLocalCoordinatedRestore is the Q03 local drill for a coordinated restore:
// fence old actors, restore control-plane and service-owned state, reconcile
// workload identities, retain durable records, and only then enable dispatch.
func TestLocalCoordinatedRestore(t *testing.T) {
	acquireRestoreSlot(t)
	drill := runCoordinatedDrill(t)
	source, target := drill.Source, drill.Target

	// Control-plane state is exactly restored. Bindings, verifications,
	// pipeline IDs, conversations, decisions, receipts and queued executions
	// are part of the same snapshot.
	recoverysupport.RequireDurableEqual(t, drill.Want, recoverysupport.SnapshotDurable(t, target.Postgres))
	recoverysupport.RequireTreesEqual(t, source.RetainedLogsDir, target.RetainedLogsDir)
	recoverysupport.RequireTreesEqual(t, source.KeysDir, target.KeysDir)

	// Disposable workspaces are excluded and never replayed.
	recoverysupport.RequireTreeEmpty(t, target.WorkspaceRoot)
	recoverysupport.RequireTreeLacks(t, source.BackupRoot, recoverysupport.WorkspaceSentinel)
	if len(drill.Result.UnrecoverableWorkspaces) != 1 || drill.Result.UnrecoverableWorkspaces[0] != recoverysupport.WorkspaceID {
		t.Fatalf("unrecoverable workspaces = %v; want [%s]", drill.Result.UnrecoverableWorkspaces, recoverysupport.WorkspaceID)
	}

	// JetStream state resumes with the same event count and consumer position.
	target.StartNATS(t)
	if got := target.NATS.StreamMessages(t); got != 3 {
		t.Fatalf("restored stream messages = %d; want 3", got)
	}
	if got := target.NATS.AckFloor(t); got != 2 {
		t.Fatalf("restored consumer ack floor = %d; want 2", got)
	}

	// Service-owned Git repository, keep refs and receive journal are restored
	// byte-for-byte and remain usable. The retained commit is unreachable from
	// the restored branch but still resolvable through its keep ref.
	recoverysupport.RequireTreesEqual(t, filepath.Join(source.Root, "git-storage"), filepath.Join(target.Root, "git-storage"))
	repositoryPath := filepath.Join(target.Root, "git-storage", string(drill.GitTenant), drill.GitRepository)
	if got := requireGit(t, repositoryPath, "rev-parse", "refs/heads/main"); got != drill.GitCandidate {
		t.Fatalf("restored main = %q; want candidate %q", got, drill.GitCandidate)
	}
	if got := requireGit(t, repositoryPath, "rev-parse", gitpkg.KeepRefPrefix+drill.GitRetained); got != drill.GitRetained {
		t.Fatalf("restored keep ref = %q; want retained commit %q", got, drill.GitRetained)
	}
	if _, code := gitRun(t, repositoryPath, "cat-file", "-e", drill.GitRetained); code != 0 {
		t.Fatalf("retained commit %q is not readable after the coordinated restore", drill.GitRetained)
	}

	journal, err := gitpkg.NewFileJournal(filepath.Join(target.Root, "git-journal"))
	if err != nil {
		t.Fatalf("open restored Git journal: %v", err)
	}
	entry, ok, err := journal.Load(t.Context(), drill.JournalKey)
	if err != nil {
		t.Fatalf("load restored Git journal: %v", err)
	}
	if !ok {
		t.Fatalf("restored Git journal has no entry %q", drill.JournalKey)
	}
	if entry.State != gitpkg.JournalIntended || entry.Candidate != drill.GitCandidate || entry.ExpectedBase != drill.GitExpected {
		t.Fatalf("restored journal entry = %+v; want the intended candidate %q over base %q", entry, drill.GitCandidate, drill.GitExpected)
	}
	observed := gitpkg.RefState{Ref: "refs/heads/main", Commit: drill.GitCandidate}
	outcome, err := entry.Reconcile(observed)
	if err != nil {
		t.Fatalf("reconcile restored journal entry: %v", err)
	}
	if outcome != gitpkg.RecoveryApplied {
		t.Fatalf("restored journal reconciliation = %q; want applied (report, never reapply)", outcome)
	}

	// The service-owned checkpoint store restores through the product session
	// store: opted-in raw content is readable, default-off raw content is
	// withheld, and both records keep their identity.
	checkpointStore, err := sessionpkg.NewStore(sessionpkg.StoreConfig{
		Storage: checkpointStorage(t, filepath.Join(target.Root, "checkpoints")),
		Clock:   fixedClock{now: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("open restored checkpoint store: %v", err)
	}
	restored, err := checkpointStore.Restore(t.Context(), drill.CheckpointRef)
	if err != nil {
		t.Fatalf("restore opted-in checkpoint: %v", err)
	}
	if len(restored.Messages) != 1 || restored.Messages[0].Content != drill.RawContent {
		t.Fatalf("restored opted-in checkpoint content = %+v; want %q", restored.Messages, drill.RawContent)
	}
	withheld, err := checkpointStore.Restore(t.Context(), drill.RawWithheld)
	if err != nil {
		t.Fatalf("restore default-off checkpoint: %v", err)
	}
	if len(withheld.Messages) != 1 || withheld.Messages[0].Content != "" {
		t.Fatalf("default-off checkpoint content = %+v; want raw content withheld", withheld.Messages)
	}

	// The external registry owns the artifact bytes. The same registry that
	// served the pre-failure reference still serves it after the restore.
	if body := fetchManifest(t, drill.Registry, drill.Artifact); messaging.DigestOf(body) != drill.Artifact {
		t.Fatalf("artifact reference resolved to a different manifest after the restore: %s", messaging.DigestOf(body))
	}

	if !drill.Result.Fenced || !drill.Result.Reconciled {
		t.Fatalf("restore result = %+v; want fenced and reconciled", drill.Result)
	}
	if drill.Result.Preserved.Receipts < 1 || drill.Result.Preserved.Decisions < 1 || drill.Result.Preserved.QueuedExecutions < 1 {
		t.Fatalf("preserved counts = %+v; want receipts, decisions and queued executions", drill.Result.Preserved)
	}
	if len(drill.Assessment.LostIDs) != 0 {
		t.Fatalf("lost IDs = %v; want none", drill.Assessment.LostIDs)
	}
	if missed := drill.Assessment.MissedObjectives(); len(missed) > 0 {
		t.Logf("the coordinated local restore missed recommended objective(s) %v: measured RTO %v against %v; reporting the limitation instead of failing the restore",
			missed, drill.Assessment.RTO, drill.Assessment.Objectives.RTO)
	}

	evidence, err := TimelineEvidence("local-coordinated-restore", drill.RunID, "local-podman", string(OutcomeConfirmed), RPOBaseLocalSameCommit, drill.Timeline)
	if err != nil {
		t.Fatalf("TimelineEvidence: %v", err)
	}
	evidence.SourceRevision = os.Getenv("KODAMA_Q03_SOURCE_REVISION")
	evidence.UnrecoverableWorkspaces = append([]string(nil), drill.Result.UnrecoverableWorkspaces...)
	evidence.Images = []ImageRecord{
		{Tag: source.PostgresImageTag, Digest: source.PostgresImageDigest},
		{Tag: source.NATSImageTag, Digest: source.NATSImageDigest},
		{Tag: harness.ImageRegistry, Digest: drill.Registry.Digest},
		{Tag: "kodama/w45:retained", Digest: drill.Artifact},
	}
	recordDrillEvidence(t, "run-coordinated-"+drill.RunID+".json", evidence)
}

// TestLocalCoordinatedRestoreUncertainEffect backs up a real shard and
// service-owned state, then runs installation.Recovery.RunRestore into a fresh
// target shard while an observed workload identity has an unknown outcome. The
// unresolved identity must fail RunRestore, keep dispatch closed, and be
// recorded alongside unrecoverable workspaces in drill evidence.
func TestLocalCoordinatedRestoreUncertainEffect(t *testing.T) {
	acquireRestoreSlot(t)
	prepared := prepareCoordinatedBackup(t)
	source := prepared.source

	target := recoverysupport.StartTarget(t)
	if _, err := prepared.timeline.Mark(MilestoneRestoreStarted); err != nil {
		t.Fatalf("Mark(restore.started): %v", err)
	}

	libpod := source.Libpod(t)
	fence, err := recovery.NewFenceDriver(recovery.FenceConfig{
		Libpod: libpod,
		Inventory: []recovery.Actor{
			{Kind: recovery.ActorController, Name: prepared.controllerName},
			{Kind: recovery.ActorService, Name: prepared.serviceName},
			{Kind: recovery.ActorConsumer, Name: prepared.consumerName},
		},
	})
	if err != nil {
		t.Fatalf("NewFenceDriver: %v", err)
	}
	if err := fence.Fence(t.Context()); err != nil {
		t.Fatalf("Fence before service-owned restore: %v", err)
	}
	for _, name := range []string{prepared.controllerName, prepared.serviceName, prepared.consumerName} {
		recoverysupport.WaitContainerStopped(t, libpod, name)
	}
	restoreServiceOwnedState(t, prepared.serviceBackup, target)

	unknown := "workload-unknown"
	observer := recoverysupport.NewScriptedObserver(&recoverysupport.ContainerObserver{
		Libpod: libpod,
		Names: map[string]string{
			"workload-settled": prepared.settledName,
		},
	})
	observer.SetUnknown(unknown, true)
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
		Workloads: []recovery.WorkloadIdentity{
			{ID: "workload-settled"},
			{ID: unknown},
		},
		Observer: observer,
		Services: []string{"controller", "publisher", "git"},
	})
	if err != nil {
		t.Fatalf("NewRestoreDriver: %v", err)
	}
	dispatch := &countingDispatch{}
	_, err = (installation.Recovery{
		Restore:  restoreDriver,
		Fence:    fence,
		Dispatch: dispatch,
	}).RunRestore(t.Context(), prepared.backup.Identity, installation.RestorePlan{Scope: prepared.backup.Scope})
	if err == nil {
		t.Fatal("RunRestore succeeded despite an unresolved workload effect")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
		t.Fatalf("RunRestore error = %v (class=%v, ok=%v); want %s", err, class, ok, fault.UncertainExternalEffect)
	}
	if got := dispatch.Count(); got != 0 {
		t.Fatalf("dispatch enabled %d times; want 0 when reconciliation is unresolved", got)
	}

	reconcile, err := restoreDriver.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile with an unknown identity: %v", err)
	}
	if reconcile.Reconciled || len(reconcile.Unresolved) != 1 || reconcile.Unresolved[0] != unknown {
		t.Fatalf("reconcile = %+v; want the unknown identity unresolved", reconcile)
	}
	preflight, err := restoreDriver.Preflight(t.Context(), prepared.backup.Identity)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(preflight.MissingWorkspaces) != 1 || preflight.MissingWorkspaces[0] != recoverysupport.WorkspaceID {
		t.Fatalf("missing workspaces = %v; want [%s]", preflight.MissingWorkspaces, recoverysupport.WorkspaceID)
	}
	if _, err := prepared.timeline.Mark(MilestoneRestoreCompleted); err != nil {
		t.Fatalf("Mark(restore.completed): %v", err)
	}

	restored := recoverysupport.SnapshotDurable(t, target.Postgres)
	recoverysupport.RequireDurableEqual(t, prepared.wantDurable, restored)

	milestones := make([]MilestoneRecord, 0, len(prepared.timeline.Milestones()))
	for _, milestone := range prepared.timeline.Milestones() {
		milestones = append(milestones, MilestoneRecord(milestone))
	}
	committedIDs := durableIDs(prepared.wantDurable)
	restoredIDs := durableIDs(restored)
	evidence := Evidence{
		Drill:                   "local-coordinated-restore-uncertain",
		RunID:                   prepared.runID,
		Environment:             "local-podman",
		Outcome:                 "blocked",
		SourceRevision:          os.Getenv("KODAMA_Q03_SOURCE_REVISION"),
		Milestones:              milestones,
		CommittedWatermark:      &WatermarkRecord{At: prepared.committedAt, IDs: committedIDs},
		RestoredWatermark:       &WatermarkRecord{At: prepared.committedAt, IDs: restoredIDs},
		LostIDs:                 []string{},
		UnresolvedResources:     append([]string(nil), reconcile.Unresolved...),
		UnrecoverableWorkspaces: append([]string(nil), preflight.MissingWorkspaces...),
		RPOBase:                 RPOBaseLocalSameCommit,
		RPOMilliseconds:         0,
		RTOMilliseconds:         0,
		MeetsRTO:                false,
		Images: []ImageRecord{
			{Tag: source.PostgresImageTag, Digest: source.PostgresImageDigest},
			{Tag: source.NATSImageTag, Digest: source.NATSImageDigest},
			{Tag: harness.ImageRegistry, Digest: prepared.service.registry.Digest},
			{Tag: "kodama/w45:retained", Digest: prepared.service.artifact},
		},
	}
	if path := recordDrillEvidence(t, "run-coordinated-uncertain-"+prepared.runID+".json", evidence); path == "" {
		t.Fatal("failed to write uncertain-effect coordinated drill evidence")
	}
}

type preparedCoordinatedBackup struct {
	runID          string
	source         *recoverysupport.Shard
	wantDurable    recoverysupport.DurableSnapshot
	committedAt    time.Time
	timeline       *Timeline
	backup         installation.BackupResult
	service        serviceOwnedState
	serviceBackup  serviceBackup
	controllerName string
	serviceName    string
	consumerName   string
	settledName    string
}

func prepareCoordinatedBackup(t *testing.T) *preparedCoordinatedBackup {
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

	service := seedServiceOwnedState(t, source)

	// Old actors are live before the failure. The fence inventory is explicit;
	// no host-wide discovery happens.
	alpine := source.AlpineImage(t)
	controller := source.RunWorkload(t, alpine, "q03-controller-"+runID, "sleep", "300")
	enforcing := source.RunWorkload(t, alpine, "q03-service-"+runID, "sleep", "300")
	consumer := source.RunWorkload(t, alpine, "q03-consumer-"+runID, "sleep", "300")
	settled := source.RunWorkload(t, alpine, "q03-workload-"+runID, "true")
	recoverysupport.WaitContainerStopped(t, source.Libpod(t), settled.Name)

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

	svcBackup := backupServiceOwnedState(t, source, runID)
	if _, err := timeline.Mark(MilestoneBackupCompleted); err != nil {
		t.Fatalf("Mark(backup.completed): %v", err)
	}
	if _, err := timeline.Mark(MilestoneFailureInjected); err != nil {
		t.Fatalf("Mark(failure.injected): %v", err)
	}
	return &preparedCoordinatedBackup{
		runID:          runID,
		source:         source,
		wantDurable:    wantDurable,
		committedAt:    committedAt,
		timeline:       timeline,
		backup:         backup,
		service:        service,
		serviceBackup:  svcBackup,
		controllerName: controller.Name,
		serviceName:    enforcing.Name,
		consumerName:   consumer.Name,
		settledName:    settled.Name,
	}
}

// runCoordinatedDrill performs the coordinated backup and restore.
func runCoordinatedDrill(t *testing.T) *coordinatedDrill {
	t.Helper()
	prepared := prepareCoordinatedBackup(t)
	source := prepared.source
	timeline := prepared.timeline

	target := recoverysupport.StartTarget(t)
	if _, err := timeline.Mark(MilestoneRestoreStarted); err != nil {
		t.Fatalf("Mark(restore.started): %v", err)
	}

	libpod := source.Libpod(t)
	fence, err := recovery.NewFenceDriver(recovery.FenceConfig{
		Libpod: libpod,
		Inventory: []recovery.Actor{
			{Kind: recovery.ActorController, Name: prepared.controllerName},
			{Kind: recovery.ActorService, Name: prepared.serviceName},
			{Kind: recovery.ActorConsumer, Name: prepared.consumerName},
		},
	})
	if err != nil {
		t.Fatalf("NewFenceDriver: %v", err)
	}
	// S10 §4 restore order: fence old actors before any state is restored.
	// installation.Recovery performs the same fence again around the
	// control-plane restore, so this first pass proves the coordinated
	// service-owned state is restored only after fencing.
	if err := fence.Fence(t.Context()); err != nil {
		t.Fatalf("Fence before service-owned restore: %v", err)
	}
	for _, name := range []string{prepared.controllerName, prepared.serviceName, prepared.consumerName} {
		recoverysupport.WaitContainerStopped(t, libpod, name)
	}
	restoreServiceOwnedState(t, prepared.serviceBackup, target)
	observer := &recoverysupport.ContainerObserver{
		Libpod: libpod,
		Names: map[string]string{
			"workload-settled": prepared.settledName,
		},
	}
	workloads := []recovery.WorkloadIdentity{
		{ID: "workload-settled"},
		{ID: "workload-absent"},
	}
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
		Workloads: workloads,
		Observer:  observer,
		Services:  []string{"controller", "publisher", "git"},
	})
	if err != nil {
		t.Fatalf("NewRestoreDriver: %v", err)
	}
	dispatch := &countingDispatch{}
	result, err := (installation.Recovery{
		Restore:  restoreDriver,
		Fence:    fence,
		Dispatch: dispatch,
	}).RunRestore(t.Context(), prepared.backup.Identity, installation.RestorePlan{Scope: prepared.backup.Scope})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if got := dispatch.Count(); got != 1 {
		t.Fatalf("dispatch enabled %d times; want 1", got)
	}
	for _, name := range []string{prepared.controllerName, prepared.serviceName, prepared.consumerName} {
		recoverysupport.WaitContainerStopped(t, libpod, name)
	}
	if _, err := timeline.Mark(MilestoneRestoreCompleted); err != nil {
		t.Fatalf("Mark(restore.completed): %v", err)
	}
	if _, err := timeline.Mark(MilestoneServiceResumed); err != nil {
		t.Fatalf("Mark(service.resumed): %v", err)
	}

	restored := recoverysupport.SnapshotDurable(t, target.Postgres)
	if err := timeline.Restore(Watermark{At: prepared.committedAt, IDs: durableIDs(restored)}); err != nil {
		t.Fatalf("Restore watermark: %v", err)
	}
	assessment, err := timeline.Assess()
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	return &coordinatedDrill{
		RunID:         prepared.runID,
		Source:        source,
		Target:        target,
		Want:          prepared.wantDurable,
		Result:        result,
		Timeline:      timeline,
		Assessment:    assessment,
		GitTenant:     prepared.service.tenant,
		GitRepository: prepared.service.repository,
		GitRetained:   prepared.service.retained,
		GitCandidate:  prepared.service.candidate,
		GitExpected:   prepared.service.expected,
		JournalKey:    prepared.service.journalKey,
		CheckpointRef: prepared.service.checkpointRef,
		RawContent:    prepared.service.rawContent,
		RawWithheld:   prepared.service.withheldRef,
		Artifact:      prepared.service.artifact,
		Registry:      prepared.service.registry,
	}
}

// serviceOwnedState is the state each owning service keeps outside the
// control-plane backup set.
type serviceOwnedState struct {
	tenant        id.TenantID
	repository    string
	retained      string
	candidate     string
	expected      string
	journalKey    string
	checkpointRef sessionpkg.Ref
	rawContent    string
	withheldRef   sessionpkg.Ref
	artifact      string
	registry      *messaging.Registry
}

// serviceBackup is the coordinated copy of each service-owned tree.
type serviceBackup struct {
	gitStorage  string
	gitJournal  string
	checkpoints string
}

func seedServiceOwnedState(t *testing.T, source *recoverysupport.Shard) serviceOwnedState {
	t.Helper()
	ctx := t.Context()
	tenant, err := id.ParseTenantID("tenant-q03")
	if err != nil {
		t.Fatalf("ParseTenantID: %v", err)
	}
	const repository = "shop/web"

	storageRoot := filepath.Join(source.Root, "git-storage")
	storage, err := gitpkg.NewLocalStorage(storageRoot)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	location, err := storage.EnsureRepository(tenant, repository)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	retained, expectedBase, candidate := seedGitHistory(t, location.Path)

	keepStore, err := gitpkg.NewKeepRefStore(storage, hostGitRunner{})
	if err != nil {
		t.Fatalf("NewKeepRefStore: %v", err)
	}
	kept, err := keepStore.KeepRefs(ctx, tenant, repository, []string{retained})
	if err != nil {
		t.Fatalf("KeepRefs: %v", err)
	}
	if len(kept.Kept) != 1 || kept.Kept[0] != retained {
		t.Fatalf("keep refs = %+v; want the retained commit %q", kept, retained)
	}

	journalDir := filepath.Join(source.Root, "git-journal")
	journal, err := gitpkg.NewFileJournal(journalDir)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	const journalKey = "event-q03-journal-1"
	entry := gitpkg.JournalEntry{
		Version:        gitpkg.JournalVersion,
		ResourceID:     "resource-q03-git",
		Repository:     repository,
		Ref:            "refs/heads/main",
		ExpectedBase:   expectedBase,
		Candidate:      candidate,
		SourceEventID:  journalKey,
		VerificationID: "verification-q03-git",
		State:          gitpkg.JournalIntended,
		At:             time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC),
	}
	if err := journal.Record(ctx, entry); err != nil {
		t.Fatalf("record receive journal: %v", err)
	}

	checkpointDir := filepath.Join(source.Root, "checkpoints")
	checkpointStore, err := sessionpkg.NewStore(sessionpkg.StoreConfig{
		Storage: fileCheckpointStorage{dir: checkpointDir},
		Clock:   fixedClock{now: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rawContent := "raw model content retained only by an authorized opt-in"
	createdAt := time.Now().UTC()
	optedIn, err := checkpointStore.Save(ctx, sessionpkg.Record{
		Version:     sessionpkg.RefVersion,
		TaskID:      "task-q03",
		ExecutionID: "execution-q03",
		SessionID:   "session-q03",
		Messages: []sessionpkg.Message{
			{Role: "user", Content: rawContent},
		},
		Retention: sessionpkg.RetentionPolicy{RawContent: true},
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("Save opted-in checkpoint: %v", err)
	}
	withheld, err := checkpointStore.Save(ctx, sessionpkg.Record{
		Version:     sessionpkg.RefVersion,
		TaskID:      "task-q03",
		ExecutionID: "execution-q03",
		SessionID:   "session-q03-off",
		Messages: []sessionpkg.Message{
			{Role: "user", Content: rawContent},
		},
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("Save default-off checkpoint: %v", err)
	}

	registry := messaging.StartRegistry(t, false)
	manifest, blobs := messaging.OCIManifest([]byte(`{"architecture":"amd64"}`), []byte("retained artifact bytes"))
	artifact := registry.Push(t, "kodama/w45", "retained", manifest, blobs)

	return serviceOwnedState{
		tenant:        tenant,
		repository:    repository,
		retained:      retained,
		candidate:     candidate,
		expected:      expectedBase,
		journalKey:    journalKey,
		checkpointRef: optedIn,
		rawContent:    rawContent,
		withheldRef:   withheld,
		artifact:      artifact,
		registry:      registry,
	}
}

func backupServiceOwnedState(t *testing.T, source *recoverysupport.Shard, runID string) serviceBackup {
	t.Helper()
	root := filepath.Join(source.Root, "service-backups", runID)
	files := recovery.OSFiles{}
	backup := serviceBackup{
		gitStorage:  filepath.Join(root, "git-storage"),
		gitJournal:  filepath.Join(root, "git-journal"),
		checkpoints: filepath.Join(root, "checkpoints"),
	}
	for _, pair := range [][2]string{
		{filepath.Join(source.Root, "git-storage"), backup.gitStorage},
		{filepath.Join(source.Root, "git-journal"), backup.gitJournal},
		{filepath.Join(source.Root, "checkpoints"), backup.checkpoints},
	} {
		if _, err := files.Snapshot(t.Context(), pair[0], pair[1]); err != nil {
			t.Fatalf("snapshot service-owned tree %s: %v", pair[0], err)
		}
	}
	return backup
}

func restoreServiceOwnedState(t *testing.T, backup serviceBackup, target *recoverysupport.Shard) {
	t.Helper()
	files := recovery.OSFiles{}
	for _, pair := range [][2]string{
		{backup.gitStorage, filepath.Join(target.Root, "git-storage")},
		{backup.gitJournal, filepath.Join(target.Root, "git-journal")},
		{backup.checkpoints, filepath.Join(target.Root, "checkpoints")},
	} {
		if err := files.EnsureEmpty(t.Context(), pair[1]); err != nil {
			t.Fatalf("ensure empty service-owned target %s: %v", pair[1], err)
		}
		if err := files.Restore(t.Context(), pair[0], pair[1]); err != nil {
			t.Fatalf("restore service-owned tree %s: %v", pair[0], err)
		}
	}
}

// seedGitHistory creates a repository whose main branch no longer reaches the
// retained commit. Only the keep ref keeps it resolvable.
func seedGitHistory(t *testing.T, repositoryPath string) (retained, expectedBase, candidate string) {
	t.Helper()
	requireGit(t, "", "init", "--bare", "--initial-branch=main", repositoryPath)
	work := filepath.Join(t.TempDir(), "git-work")
	requireGit(t, "", "init", "--initial-branch=main", work)
	requireGit(t, work, "config", "user.email", "q03@example.invalid")
	requireGit(t, work, "config", "user.name", "Q03 Drill")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("retained commit\n"), 0o644); err != nil {
		t.Fatalf("write first commit: %v", err)
	}
	requireGit(t, work, "add", "README.md")
	requireGit(t, work, "commit", "-m", "retained root commit")
	retained = requireGit(t, work, "rev-parse", "HEAD")
	requireGit(t, work, "remote", "add", "origin", repositoryPath)
	requireGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	expectedBase = retained

	requireGit(t, work, "checkout", "--orphan", "rebuilt")
	if err := os.Remove(filepath.Join(work, "README.md")); err != nil {
		t.Fatalf("remove retained file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "rebuilt.txt"), []byte("unrelated candidate\n"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	requireGit(t, work, "add", "rebuilt.txt")
	requireGit(t, work, "commit", "-m", "unrelated main candidate")
	candidate = requireGit(t, work, "rev-parse", "HEAD")
	requireGit(t, work, "push", "--force", "origin", "HEAD:refs/heads/main")
	return retained, expectedBase, candidate
}

// hostGitRunner is the native command seam for keep refs.
type hostGitRunner struct{}

func (hostGitRunner) Run(ctx context.Context, command gitpkg.Command) (gitpkg.CommandResult, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := gitpkg.CommandResult{Stdout: []byte(stdout.String()), Stderr: []byte(stderr.String())}
	if err != nil {
		var exit *exec.ExitError
		if ok := asExitError(err, &exit); ok {
			result.ExitCode = exit.ExitCode()
			return result, nil
		}
		return result, err
	}
	return result, nil
}

// gitRun executes one host Git command without a shell.
func gitRun(t *testing.T, directory string, args ...string) (string, int) {
	t.Helper()
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return strings.TrimSpace(stdout.String()), 0
	}
	var exit *exec.ExitError
	if asExitError(err, &exit) {
		return strings.TrimSpace(stderr.String()), exit.ExitCode()
	}
	t.Fatalf("git %v: %v", args, err)
	return "", -1
}

func requireGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	out, code := gitRun(t, directory, args...)
	if code != 0 {
		t.Fatalf("git %v exited %d: %s", args, code, out)
	}
	return out
}

func asExitError(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if ok {
		*target = exit
	}
	return ok
}

// fileCheckpointStorage is a service-owned file store. It is test fixture code
// for the checkpoint owner's backend boundary.
type fileCheckpointStorage struct {
	dir string
}

func checkpointStorage(t *testing.T, dir string) fileCheckpointStorage {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create checkpoint storage: %v", err)
	}
	return fileCheckpointStorage{dir: dir}
}

func (storage fileCheckpointStorage) Read(_ context.Context, ref sessionpkg.Ref) (sessionpkg.Record, error) {
	path := storage.path(ref)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionpkg.Record{}, fmt.Errorf("%w: %s", sessionpkg.ErrCheckpointMissing, ref)
		}
		return sessionpkg.Record{}, err
	}
	var record sessionpkg.Record
	if err := json.Unmarshal(data, &record); err != nil {
		return sessionpkg.Record{}, err
	}
	return record, nil
}

func (storage fileCheckpointStorage) CompareAndSwap(_ context.Context, record sessionpkg.Record, expectedRevision uint64) (sessionpkg.Ref, error) {
	if record.Revision < expectedRevision {
		return "", fmt.Errorf("checkpoint revision %d is behind expected %d", record.Revision, expectedRevision)
	}
	payload := strings.ToLower(fmt.Sprintf("ctx-%s-%d", record.SessionID, record.Revision+1))
	ref, err := sessionpkg.ParseRef(sessionpkg.RefPrefix + sessionpkg.RefVersion + "/" + payload)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(storage.dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(storage.path(ref), data, 0o600); err != nil {
		return "", err
	}
	return ref, nil
}

func (storage fileCheckpointStorage) path(ref sessionpkg.Ref) string {
	payload := strings.TrimPrefix(string(ref), sessionpkg.RefPrefix+sessionpkg.RefVersion+"/")
	return filepath.Join(storage.dir, payload+".json")
}

// fixedClock is an injected session clock.
type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

// countingDispatch records how many times the restore opened dispatch.
type countingDispatch struct{ count atomic.Int64 }

func (dispatch *countingDispatch) EnableDispatch(context.Context) error {
	dispatch.count.Add(1)
	return nil
}

func (dispatch *countingDispatch) Count() int { return int(dispatch.count.Load()) }

// fetchManifest reads one artifact manifest by digest from the real registry.
func fetchManifest(t *testing.T, registry *messaging.Registry, digest string) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, registry.BaseURL+"/v2/kodama/w45/manifests/"+digest, nil)
	if err != nil {
		t.Fatalf("manifest request: %v", err)
	}
	request.Header.Set("Accept", messaging.OCIManifestMediaType)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("read restored artifact: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("artifact %s resolved %d after the restore; want 200", digest, response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read artifact body: %v", err)
	}
	return body
}
