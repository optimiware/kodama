package recovery

// Recovery driver conformance (unit boundary).
//
// This file pins the small, consumer-owned API of this package:
//
//	FenceDriver / NewFenceDriver(FenceConfig)
//	    FenceConfig{Libpod runtimepodman.Libpod; Inventory []Actor}
//	    Actor{Kind ActorKind; Name string}; ActorKind is controller|service|consumer.
//	    Fence inspects exactly the inventoried containers through Libpod, never
//	    lists/discoveries host containers, stops a live actor once and requires
//	    affirmative whole-container stop evidence. A missing actor or an
//	    ambiguous/unconfirmed stop is refused.
//
//	BackupDriver / NewBackupDriver(BackupConfig)
//	    BackupConfig{
//	        Executor Executor; Files Files; Container, Database, Root string
//	        Sources []BackupSource; Workspaces []Workspace
//	        Preserved func(context.Context) (installation.PreservedState, error)
//	        NewIdentity func() string
//	    }
//	    BackupSource{Name, Root string; Ready func(context.Context) error}
//	    Workspace{ID, Root string}
//	    Executor{Exec(context.Context, ExecRequest) (ExecResult, error)}
//	    ExecRequest{Container string; Argv []string; Stdin []byte}
//	    ExecResult{Stdout, Stderr []byte; ExitCode int}
//	    Files{EnsureEmpty, Snapshot, Restore, Stat, ReadFile, WriteFile, RemoveAll}
//	    FileInfo{Kind FileKind; Size int64}; FileKind is directory|regular.
//	    A nil Files uses the operating-system boundary. EnsureEmpty creates the
//	    destination when absent and refuses a non-empty one; WriteFile creates
//	    parent directories and replaces atomically.
//	    Backup runs exactly one pg_dump in Container:
//	        pg_dump --format=custom --dbname <Database>
//	    The dump keeps object ownership and table/column ACLs, so the target
//	    must already hold the application roles the migrations create.
//	    It writes <Root>/<identity>/database.dump, one directory per source under
//	    <Root>/<identity>/<source.Name>, then manifest.json last:
//	        {"format":1,"identity":"...","scope":{"database":true,
//	         "retained_logs":true,"encryption_keys":true,"jetstream":true,
//	         "workspaces":false},"sources":{"retained-logs":N,"keys":N,
//	         "jetstream":N},"database_bytes":N,
//	         "preserved":{"receipts":N,"decisions":N,"queued_executions":N},
//	         "workspaces":["..."]}
//	    No identity is returned unless the whole set is complete. A source whose
//	    Ready probe fails is not captured. Workspace roots are never captured and
//	    are reported as unrecoverable workspaces.
//
//	RestoreDriver / NewRestoreDriver(RestoreConfig)
//	    RestoreConfig{
//	        Executor Executor; Files Files; Container, Database, Root string
//	        Targets []RestoreTarget; Workloads []WorkloadIdentity
//	        Observer Observer; Services []string
//	    }
//	    RestoreTarget{Name, Root string}
//	    WorkloadIdentity{ID string}
//	    WorkloadObservation{Identity WorkloadIdentity; State ObservationState;
//	        WorkspaceID string; WorkspacePresent bool; Detail string}
//	    ObservationState is absent|running|stopped|unknown.
//	    Observer{Observe(context.Context, WorkloadIdentity) (WorkloadObservation, error)}
//	    A nil Files uses the operating-system boundary. Observer is required only
//	    when Workloads is non-empty.
//	    Preflight reads and validates the actual artifacts and binds exactly one
//	    complete identity whose keys are present. Concurrent/different preflights
//	    cannot retarget the armed restore. Restore consumes the binding before any
//	    effect, refuses when the target lacks a required database role (a typed
//	    refusal before pg_restore), checks every target is empty, then runs exactly
//	    one pg_restore in Container:
//	        pg_restore --clean --if-exists --dbname <Database>
//	    with the dump on stdin, then copies each source into its target. It never
//	    retries, never trusts caller booleans and never implements DispatchGate.
//	    Reconcile observes only the explicit workload identities; an unknown or
//	    unobservable effect is unresolved and blocks dispatch. Reconcile may be
//	    called independently; installation.Recovery enforces the restore order.
//
// The package's exported implementation types are asserted to implement the
// installation ports below.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/internal/shell/store"
)

// The installation ports are the only contract the drivers implement.
var (
	_ installation.FencePort   = (*FenceDriver)(nil)
	_ installation.BackupPort  = (*BackupDriver)(nil)
	_ installation.RestorePort = (*RestoreDriver)(nil)
)

// The restore path must never own dispatch; DispatchGate belongs to the
// composition root.
func TestRestoreDriverDoesNotImplementDispatchGate(t *testing.T) {
	if _, ok := any((*RestoreDriver)(nil)).(installation.DispatchGate); ok {
		t.Fatal("RestoreDriver implements DispatchGate; dispatch belongs to the composition root")
	}
}

func TestDriversSuccess(t *testing.T) {
	t.Run("fence stops only inventoried live actors", func(t *testing.T) {
		libpod := newTestFakeLibpod()
		libpod.states["ctl-1"] = runningState("ctl-1")
		libpod.states["svc-1"] = runningState("svc-1")
		libpod.states["con-1"] = createdState("con-1")
		libpod.states["con-exited"] = exitedState("con-exited")

		driver, err := NewFenceDriver(FenceConfig{
			Libpod: libpod,
			Inventory: []Actor{
				{Kind: ActorController, Name: "ctl-1"},
				{Kind: ActorService, Name: "svc-1"},
				{Kind: ActorConsumer, Name: "con-1"},
				{Kind: ActorConsumer, Name: "con-exited"},
			},
		})
		if err != nil {
			t.Fatalf("NewFenceDriver: %v", err)
		}
		if err := driver.Fence(t.Context()); err != nil {
			t.Fatalf("Fence: %v", err)
		}

		wantInspect := []string{"ctl-1", "ctl-1", "svc-1", "svc-1", "con-1", "con-exited"}
		if got := libpod.inspectNames(); !reflect.DeepEqual(got, wantInspect) {
			t.Fatalf("inspect order = %v; want %v", got, wantInspect)
		}
		wantStop := []string{"ctl-1", "svc-1"}
		if got := libpod.stopNames(); !reflect.DeepEqual(got, wantStop) {
			t.Fatalf("stop order = %v; want %v", got, wantStop)
		}
		if libpod.listCalls != 0 {
			t.Fatalf("ListContainers calls = %d; want 0 (no host-wide discovery)", libpod.listCalls)
		}
		if libpod.otherCalls != 0 {
			t.Fatalf("unexpected libpod calls = %d; want 0", libpod.otherCalls)
		}
	})

	t.Run("backup preflight restore reconcile", func(t *testing.T) {
		fixture := newTestDriverFixture(t)
		ctx := t.Context()

		backup, err := NewBackupDriver(BackupConfig{
			Executor:  fixture.executor,
			Files:     fixture.files,
			Container: "pg-container",
			Database:  "kodama",
			Root:      fixture.backupRoot,
			Sources: []BackupSource{
				{Name: "retained-logs", Root: fixture.retainedLogs},
				{Name: "keys", Root: fixture.keys},
				{Name: "jetstream", Root: fixture.jetstream, Ready: fixture.jetStreamReady},
			},
			Workspaces:  []Workspace{{ID: "workspace-1", Root: fixture.workspaceRoot}},
			Preserved:   fixture.preserved,
			NewIdentity: func() string { return "backup-1" },
		})
		if err != nil {
			t.Fatalf("NewBackupDriver: %v", err)
		}

		result, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		if result.Identity != (installation.BackupIdentity{ID: "backup-1"}) {
			t.Fatalf("backup identity = %+v; want backup-1", result.Identity)
		}
		if result.Scope != installation.DefaultBackupScope() {
			t.Fatalf("backup scope = %+v; want the default scope", result.Scope)
		}
		if result.Detail == "" {
			t.Fatal("backup detail is empty; want human-readable capture evidence")
		}
		if fixture.jetStreamReadyCalls != 1 {
			t.Fatalf("JetStream Ready calls = %d; want 1", fixture.jetStreamReadyCalls)
		}

		if got := fixture.executor.requests(); len(got) != 1 {
			t.Fatalf("executor calls = %d; want 1", len(got))
		}
		dumpRequest := fixture.executor.requests()[0]
		wantArgv := []string{"pg_dump", "--format=custom", "--dbname", "kodama"}
		if dumpRequest.Container != "pg-container" || !reflect.DeepEqual(dumpRequest.Argv, wantArgv) {
			t.Fatalf("pg_dump request = %+v; want container pg-container argv %v", dumpRequest, wantArgv)
		}
		if len(dumpRequest.Stdin) != 0 {
			t.Fatalf("pg_dump stdin = %q; want empty", dumpRequest.Stdin)
		}

		setDir := filepath.Join(fixture.backupRoot, "backup-1")
		if got := string(fixture.files.file(filepath.Join(setDir, "database.dump"))); got != "dump-1" {
			t.Fatalf("database dump = %q; want dump-1", got)
		}
		manifest := fixture.manifest(t, setDir)
		if manifest.Identity != "backup-1" || manifest.Format != 1 {
			t.Fatalf("manifest identity/format = %q/%d; want backup-1/1", manifest.Identity, manifest.Format)
		}
		if got := manifest.Scope.installationScope(); got != installation.DefaultBackupScope() {
			t.Fatalf("manifest scope = %+v; want the default scope", got)
		}
		if manifest.DatabaseBytes != len("dump-1") {
			t.Fatalf("manifest database_bytes = %d; want %d", manifest.DatabaseBytes, len("dump-1"))
		}
		wantSources := map[string]int{"retained-logs": 2, "keys": 1, "jetstream": 4}
		if !reflect.DeepEqual(manifest.Sources, wantSources) {
			t.Fatalf("manifest sources = %v; want %v", manifest.Sources, wantSources)
		}
		wantPreserved := installation.PreservedState{Receipts: 1, Decisions: 2, QueuedExecutions: 3}
		if got := manifest.Preserved.installationState(); got != wantPreserved {
			t.Fatalf("manifest preserved = %+v; want %+v", got, wantPreserved)
		}
		if !reflect.DeepEqual(manifest.Workspaces, []string{"workspace-1"}) {
			t.Fatalf("manifest workspaces = %v; want [workspace-1]", manifest.Workspaces)
		}

		// The workspace root must never be a snapshot source or destination.
		for _, call := range fixture.files.testSnapshotCalls() {
			if pathWithin(call.Source, fixture.workspaceRoot) || pathWithin(call.Destination, fixture.workspaceRoot) {
				t.Fatalf("workspace root was captured: %+v", call)
			}
		}
		wantSnapshots := []testSnapshotCall{
			{Source: fixture.retainedLogs, Destination: filepath.Join(setDir, "retained-logs")},
			{Source: fixture.keys, Destination: filepath.Join(setDir, "keys")},
			{Source: fixture.jetstream, Destination: filepath.Join(setDir, "jetstream")},
		}
		if got := fixture.files.testSnapshotCalls(); !reflect.DeepEqual(got, wantSnapshots) {
			t.Fatalf("snapshot calls = %+v; want %+v", got, wantSnapshots)
		}

		restore, err := NewRestoreDriver(RestoreConfig{
			Executor:  fixture.executor,
			Files:     fixture.files,
			Container: "restore-pg",
			Database:  "kodama",
			Root:      fixture.backupRoot,
			Targets: []RestoreTarget{
				{Name: "retained-logs", Root: fixture.restoreLogs},
				{Name: "keys", Root: fixture.restoreKeys},
				{Name: "jetstream", Root: fixture.restoreJetStream},
			},
			Workloads: []WorkloadIdentity{{ID: "workload-1"}, {ID: "workload-2"}},
			Observer:  fixture.observer,
			Services:  []string{"controller", "publisher"},
		})
		if err != nil {
			t.Fatalf("NewRestoreDriver: %v", err)
		}

		evidence, err := restore.Preflight(ctx, result.Identity)
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		wantEvidence := installation.BackupEvidence{
			Identity:          installation.BackupIdentity{ID: "backup-1"},
			Scope:             installation.DefaultBackupScope(),
			KeysPresent:       true,
			Preserved:         wantPreserved,
			MissingWorkspaces: []string{"workspace-1"},
		}
		if !reflect.DeepEqual(evidence, wantEvidence) {
			t.Fatalf("preflight evidence = %+v; want %+v", evidence, wantEvidence)
		}

		restored, err := restore.Restore(ctx, installation.RestorePlan{
			Scope:          installation.DefaultBackupScope(),
			Fenced:         true,
			Reconciled:     true,
			EnableDispatch: false,
		})
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if restored.Fenced || restored.Reconciled {
			t.Fatalf("restore reported fenced=%v reconciled=%v; the driver observed neither", restored.Fenced, restored.Reconciled)
		}
		if restored.Preserved != wantPreserved {
			t.Fatalf("restored preserved = %+v; want %+v", restored.Preserved, wantPreserved)
		}
		if !reflect.DeepEqual(restored.UnrecoverableWorkspaces, []string{"workspace-1"}) {
			t.Fatalf("unrecoverable workspaces = %v; want [workspace-1]", restored.UnrecoverableWorkspaces)
		}
		if !reflect.DeepEqual(restored.Services, []string{"controller", "publisher"}) {
			t.Fatalf("restored services = %v; want [controller publisher]", restored.Services)
		}

		requests := fixture.executor.requests()
		if len(requests) != 2 {
			t.Fatalf("effect calls after restore = %d; want 2 (one dump, one restore)", len(requests))
		}
		probes := fixture.executor.probes()
		if len(probes) != 1 {
			t.Fatalf("target precondition probes = %d; want 1 (required database roles)", len(probes))
		}
		wantProbeArgv := []string{
			"psql", "--no-psqlrc", "--no-align", "--tuples-only", "--dbname", "kodama", "--command",
			"SELECT rolname FROM pg_roles WHERE rolname IN ('kodama_owner', 'kodama_runtime', 'kodama_controller', 'kodama_publisher', 'kodama_auth') ORDER BY rolname",
		}
		if probes[0].Container != "restore-pg" || !reflect.DeepEqual(probes[0].Argv, wantProbeArgv) {
			t.Fatalf("role probe request = %+v; want container restore-pg argv %v", probes[0], wantProbeArgv)
		}
		if len(probes[0].Stdin) != 0 {
			t.Fatalf("role probe stdin = %q; want empty", probes[0].Stdin)
		}
		restoreRequest := requests[1]
		wantRestoreArgv := []string{"pg_restore", "--clean", "--if-exists", "--dbname", "kodama"}
		if restoreRequest.Container != "restore-pg" || !reflect.DeepEqual(restoreRequest.Argv, wantRestoreArgv) {
			t.Fatalf("pg_restore request = %+v; want container restore-pg argv %v", restoreRequest, wantRestoreArgv)
		}
		if string(restoreRequest.Stdin) != "dump-1" {
			t.Fatalf("pg_restore stdin = %q; want dump-1", restoreRequest.Stdin)
		}

		wantRestores := []testRestoreCall{
			{Source: filepath.Join(setDir, "retained-logs"), Destination: fixture.restoreLogs},
			{Source: filepath.Join(setDir, "keys"), Destination: fixture.restoreKeys},
			{Source: filepath.Join(setDir, "jetstream"), Destination: fixture.restoreJetStream},
		}
		if got := fixture.files.testRestoreCalls(); !reflect.DeepEqual(got, wantRestores) {
			t.Fatalf("restore calls = %+v; want %+v", got, wantRestores)
		}
		for _, target := range []string{fixture.restoreLogs, fixture.restoreKeys, fixture.restoreJetStream} {
			if !fixture.files.ensureEmptyCalled(target) {
				t.Fatalf("target %s was not checked empty before restore", target)
			}
		}

		reconcile, err := restore.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if !reconcile.Reconciled || len(reconcile.Unresolved) != 0 {
			t.Fatalf("reconcile = %+v; want reconciled with no unresolved effects", reconcile)
		}
		if !reflect.DeepEqual(reconcile.Services, []string{"controller", "publisher"}) {
			t.Fatalf("reconcile services = %v; want [controller publisher]", reconcile.Services)
		}
		if got := fixture.observer.observed(); !reflect.DeepEqual(got, []string{"workload-1", "workload-2"}) {
			t.Fatalf("observed workloads = %v; want [workload-1 workload-2]", got)
		}
		if got := len(fixture.executor.requests()); got != 2 {
			t.Fatalf("Reconcile made %d executor calls; want 0 (observation only)", got-2)
		}
	})
}

func TestInvalidScopeKeysIdentity(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()

	backup := fixture.newBackup(t)
	invalidScopes := []installation.BackupScope{
		{Database: false, RetainedLogs: true, EncryptionKeys: true, JetStream: true},
		{Database: true, RetainedLogs: false, EncryptionKeys: true, JetStream: true},
		{Database: true, RetainedLogs: true, EncryptionKeys: false, JetStream: true},
		{Database: true, RetainedLogs: true, EncryptionKeys: true, JetStream: false},
		{Database: true, RetainedLogs: true, EncryptionKeys: true, JetStream: true, Workspaces: true},
	}
	for _, scope := range invalidScopes {
		result, err := backup.Backup(ctx, installation.BackupPlan{Scope: scope})
		if err == nil {
			t.Fatalf("Backup accepted invalid scope %+v", scope)
		}
		if result.Identity.ID != "" {
			t.Fatalf("invalid scope returned identity %q; want none", result.Identity.ID)
		}
	}
	if got := len(fixture.executor.requests()); got != 0 {
		t.Fatalf("invalid scope made %d executor calls; want 0", got)
	}

	// A backup that captures an empty key source is not a complete set: it must
	// return no identity and write no manifest, so the caller has nothing it can
	// preflight or restore.
	fixture.files.setSnapshotCount("keys", 0)
	fixture.identity = "backup-no-keys"
	fixture.dump = "dump-no-keys"
	noKeysBackup := fixture.newBackup(t)
	noKeysResult, err := noKeysBackup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err == nil {
		t.Fatal("Backup reported success without capturing encryption key material")
	}
	if noKeysResult.Identity.ID != "" {
		t.Fatalf("empty key capture returned identity %q; want none", noKeysResult.Identity.ID)
	}
	noKeysManifest := filepath.Join(fixture.backupRoot, "backup-no-keys", "manifest.json")
	if data := fixture.files.file(noKeysManifest); len(data) != 0 {
		t.Fatalf("empty key capture wrote a manifest: %s", data)
	}
	restore := fixture.newRestore(t)
	if _, err := restore.Preflight(ctx, installation.BackupIdentity{ID: "backup-no-keys"}); err == nil {
		t.Fatal("Preflight accepted a backup set with no key material")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("missing-manifest error class = %v; want not_found (%v)", class, err)
	}

	// A foreign identity is not found. An empty identity is invalid input.
	if _, err := restore.Preflight(ctx, installation.BackupIdentity{ID: "backup-foreign"}); err == nil {
		t.Fatal("Preflight accepted a foreign backup identity")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("foreign identity error class = %v; want not_found (%v)", class, err)
	}
	if _, err := restore.Preflight(ctx, installation.BackupIdentity{}); err == nil {
		t.Fatal("Preflight accepted an empty backup identity")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("empty identity error class = %v; want invalid_input (%v)", class, err)
	}

	fence := &testRecordingFence{}
	dispatch := &testRecordingDispatch{}
	runner := installation.Recovery{Restore: restore, Fence: fence, Dispatch: dispatch}
	if _, err := runner.RunRestore(ctx, installation.BackupIdentity{ID: "backup-no-keys"}, installation.RestorePlan{Scope: installation.DefaultBackupScope()}); err == nil {
		t.Fatal("RunRestore accepted a backup without encryption keys")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("missing-key error class = %v; want not_found (%v)", class, err)
	}
	if fence.calls != 0 || dispatch.calls != 0 {
		t.Fatalf("missing keys fenced %d and dispatched %d; want both 0", fence.calls, dispatch.calls)
	}

	// Restore without an armed preflight is refused before any effect. A set
	// without key material never arms the driver.
	before := len(fixture.executor.requests())
	if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: installation.DefaultBackupScope()}); err == nil {
		t.Fatal("Restore ran without a preflight")
	}
	if got := len(fixture.executor.requests()); got != before {
		t.Fatalf("unarmed Restore made %d executor calls; want 0", got-before)
	}

	// A complete set is required to exercise the dispatch refusal. The driver
	// must reject EnableDispatch regardless of caller booleans.
	fixture.files.setSnapshotCount("keys", 1)
	fixture.identity = "backup-complete"
	completeBackup := fixture.newBackup(t)
	complete, err := completeBackup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("complete Backup: %v", err)
	}
	armedDriver := fixture.newRestore(t)
	armed, err := armedDriver.Preflight(ctx, complete.Identity)
	if err != nil {
		t.Fatalf("complete Preflight: %v", err)
	}
	if !armed.KeysPresent {
		t.Fatal("complete backup reported keys absent")
	}
	before = len(fixture.executor.requests())
	if _, err := armedDriver.Restore(ctx, installation.RestorePlan{
		Scope:          armed.Scope,
		Fenced:         true,
		Reconciled:     true,
		EnableDispatch: true,
	}); err == nil {
		t.Fatal("Restore accepted EnableDispatch; dispatch belongs to the composition root")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("EnableDispatch error class = %v; want invalid_input (%v)", class, err)
	}
	if got := len(fixture.executor.requests()); got != before {
		t.Fatalf("refused Restore made %d executor calls; want 0", got-before)
	}

	// An incomplete backup returns no identity: a failing source capture leaves
	// the caller with nothing it can preflight.
	fixture.identity = "backup-incomplete"
	fixture.files.setSnapshotError("retained-logs", errors.New("source unavailable"))
	incomplete := fixture.newBackup(t)
	failed, err := incomplete.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err == nil {
		t.Fatal("Backup reported success for an incomplete capture")
	}
	if failed.Identity.ID != "" {
		t.Fatalf("incomplete capture returned identity %q; want none", failed.Identity.ID)
	}
	if _, err := fixture.newRestore(t).Preflight(ctx, installation.BackupIdentity{ID: "backup-incomplete"}); err == nil {
		t.Fatal("Preflight accepted an incomplete backup set")
	}
}

// TestBackupDriverRequiresDurableSources proves construction refuses a source
// configuration that is not exactly the three durable families Preflight and
// Restore resolve.
func TestBackupDriverRequiresDurableSources(t *testing.T) {
	fixture := newTestDriverFixture(t)
	all := []BackupSource{
		{Name: "retained-logs", Root: fixture.retainedLogs},
		{Name: "keys", Root: fixture.keys},
		{Name: "jetstream", Root: fixture.jetstream},
	}
	cases := []struct {
		name    string
		sources []BackupSource
	}{
		{name: "nil sources"},
		{name: "missing retained-logs", sources: all[1:]},
		{name: "missing keys", sources: []BackupSource{all[0], all[2]}},
		{name: "missing jetstream", sources: all[:2]},
		{name: "extra source", sources: append(append([]BackupSource(nil), all...),
			BackupSource{Name: "extra", Root: fixture.workspaceRoot})},
		{name: "duplicate source", sources: []BackupSource{all[0], all[1], all[1]}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewBackupDriver(BackupConfig{
				Executor:    fixture.executor,
				Files:       fixture.files,
				Container:   "pg-container",
				Database:    "kodama",
				Root:        fixture.backupRoot,
				Sources:     testCase.sources,
				NewIdentity: func() string { return "backup-1" },
			})
			if err == nil {
				t.Fatal("NewBackupDriver accepted an incomplete source set")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("source set error class = %v; want invalid_input (%v)", class, err)
			}
		})
	}
}

// TestFenceDriverRequiresInventory proves a fence with nothing to inspect is a
// configuration defect, not a successful no-op.
func TestFenceDriverRequiresInventory(t *testing.T) {
	libpod := newTestFakeLibpod()
	for _, inventory := range [][]Actor{nil, {}} {
		if _, err := NewFenceDriver(FenceConfig{Libpod: libpod, Inventory: inventory}); err == nil {
			t.Fatal("NewFenceDriver accepted an empty inventory")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("empty inventory error class = %v; want invalid_input (%v)", class, err)
		}
	}
	if len(libpod.inspectNames()) != 0 || len(libpod.stopNames()) != 0 {
		t.Fatalf("construction touched libpod: inspect=%v stop=%v", libpod.inspectNames(), libpod.stopNames())
	}
}

// TestRestoreTargetsMustMatchManifestSources proves preflight refuses a target
// set that differs from the captured sources before arming or any effect.
func TestRestoreTargetsMustMatchManifestSources(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()
	backup := fixture.newBackup(t)
	result, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	t.Run("manifest source without target", func(t *testing.T) {
		restore := fixture.newRestoreWithTargets(t, []RestoreTarget{
			{Name: "retained-logs", Root: fixture.restoreLogs},
			{Name: "keys", Root: fixture.restoreKeys},
		})
		before := len(fixture.executor.requests())
		if _, err := restore.Preflight(ctx, result.Identity); err == nil {
			t.Fatal("Preflight accepted a target list missing jetstream")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("missing target error class = %v; want invalid_input (%v)", class, err)
		}
		if got := len(fixture.executor.requests()); got != before {
			t.Fatalf("mismatched preflight made %d executor calls; want 0", got-before)
		}
		if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
			t.Fatal("Restore ran after a refused preflight")
		}
		if got := len(fixture.executor.requests()); got != before {
			t.Fatalf("refused Restore made %d pg_restore calls; want 0", got-before)
		}
	})

	t.Run("target without manifest source", func(t *testing.T) {
		restore := fixture.newRestoreWithTargets(t, []RestoreTarget{
			{Name: "retained-logs", Root: fixture.restoreLogs},
			{Name: "keys", Root: fixture.restoreKeys},
			{Name: "jetstream", Root: fixture.restoreJetStream},
			{Name: "extra", Root: filepath.Join(t.TempDir(), "extra")},
		})
		before := len(fixture.executor.requests())
		if _, err := restore.Preflight(ctx, result.Identity); err == nil {
			t.Fatal("Preflight accepted a target with no captured source")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("extra target error class = %v; want invalid_input (%v)", class, err)
		}
		if got := len(fixture.executor.requests()); got != before {
			t.Fatalf("mismatched preflight made %d executor calls; want 0", got-before)
		}
		if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
			t.Fatal("Restore ran after a refused preflight")
		}
		if got := len(fixture.executor.requests()); got != before {
			t.Fatalf("refused Restore made %d pg_restore calls; want 0", got-before)
		}
	})

	t.Run("captured source directory must exist", func(t *testing.T) {
		fixture.files.removeTree(filepath.Join(fixture.backupRoot, result.Identity.ID, "jetstream"))
		restore := fixture.newRestore(t)
		before := len(fixture.executor.requests())
		if _, err := restore.Preflight(ctx, result.Identity); err == nil {
			t.Fatal("Preflight accepted a set whose source directory is missing")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("missing source dir error class = %v; want not_found (%v)", class, err)
		}
		if got := len(fixture.executor.requests()); got != before {
			t.Fatalf("missing source dir made %d executor calls; want 0", got-before)
		}
	})
}

// TestRestoreRefusesNonEmptyTarget proves the destination-empty check runs
// before pg_restore and before any source copy.
func TestRestoreRefusesNonEmptyTarget(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()
	backup := fixture.newBackup(t)
	result, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	restore := fixture.newRestore(t)
	if _, err := restore.Preflight(ctx, result.Identity); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	fixture.files.setFile(filepath.Join(fixture.restoreLogs, "stale.txt"), []byte("stale"))
	before := len(fixture.executor.requests())
	if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
		t.Fatal("Restore accepted a non-empty destination")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("non-empty destination error class = %v; want invalid_input (%v)", class, err)
	}
	if got := len(fixture.executor.requests()); got != before {
		t.Fatalf("non-empty destination made %d pg_restore calls; want 0", got-before)
	}
	if got := len(fixture.files.testRestoreCalls()); got != 0 {
		t.Fatalf("non-empty destination copied %d sources; want 0", got)
	}
}

// TestRestoreRefusesMissingDatabaseRole proves the restore refuses before any
// effect when a role the dump's ownership and ACLs reference is absent from the
// target. Every required role is exercised so dropping one from the probe cannot
// pass.
func TestRestoreRefusesMissingDatabaseRole(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()
	backup := fixture.newBackup(t)
	result, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	all := expectedDatabaseRoles()
	if got := requiredDatabaseRoles(); !reflect.DeepEqual(got, all) {
		t.Fatalf("required roles = %v; want the declaration %v", got, all)
	}
	for _, missing := range all {
		t.Run(missing, func(t *testing.T) {
			present := make([]string, 0, len(all)-1)
			for _, role := range all {
				if role != missing {
					present = append(present, role)
				}
			}
			fixture.executor.setRoles(present)
			defer fixture.executor.setRoles(all)

			restore := fixture.newRestore(t)
			if _, err := restore.Preflight(ctx, result.Identity); err != nil {
				t.Fatalf("Preflight: %v", err)
			}
			beforeEffects := len(fixture.executor.requests())
			beforeProbes := len(fixture.executor.probes())
			beforeEmpty := len(fixture.files.emptyCalls)
			beforeRestores := len(fixture.files.testRestoreCalls())

			if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
				t.Fatalf("Restore ran without target role %s", missing)
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("missing role error class = %v; want invalid_input (%v)", class, err)
			}
			if got := len(fixture.executor.probes()); got != beforeProbes+1 {
				t.Fatalf("role probes = %d; want exactly one precondition check", got-beforeProbes)
			}
			if got := len(fixture.executor.requests()); got != beforeEffects {
				t.Fatalf("missing role made %d database effects; want 0", got-beforeEffects)
			}
			if got := len(fixture.files.emptyCalls); got != beforeEmpty {
				t.Fatalf("missing role touched %d destinations before refusing; want 0", got-beforeEmpty)
			}
			if got := len(fixture.files.testRestoreCalls()); got != beforeRestores {
				t.Fatalf("missing role copied %d sources; want 0", got-beforeRestores)
			}
			// The binding was consumed, so a retry cannot replay the refusal.
			if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
				t.Fatal("Restore replayed after a missing-role refusal")
			}
			if got := len(fixture.executor.requests()); got != beforeEffects {
				t.Fatalf("replayed refusal made %d database effects; want 0", got-beforeEffects)
			}
		})
	}
}

// expectedDatabaseRoles is the independent, contract-sourced role set the
// probe must require, so a mutant that drops a role from the implementation
// list cannot silently shrink the test's expectations.
func expectedDatabaseRoles() []string {
	return []string{
		store.DefaultOwnerRole,
		store.DefaultRuntimeRole,
		store.ControllerRole,
		store.PublisherRole,
		store.AuthRole,
	}
}

// TestRestoreRefusesUnreachableRoleProbe proves an unobservable target role set
// is a dependency failure, not a successful restore, and still performs no
// effect.
func TestRestoreRefusesUnreachableRoleProbe(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()
	backup := fixture.newBackup(t)
	result, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	restore := fixture.newRestore(t)
	if _, err := restore.Preflight(ctx, result.Identity); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	fixture.executor.setProbeError(errors.New("psql unavailable"))
	before := len(fixture.executor.requests())
	if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
		t.Fatal("Restore ran without observing the target roles")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("unreachable role probe class = %v; want dependency_unavailable (%v)", class, err)
	}
	if got := len(fixture.executor.requests()); got != before {
		t.Fatalf("unreachable role probe made %d database effects; want 0", got-before)
	}
}

// TestIdentityNamesAreSingleComponents proves a caller-supplied identity can
// never escape the configured root, for backup and preflight.
func TestIdentityNamesAreSingleComponents(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()
	for _, identity := range []string{"../x", "a/b", ""} {
		t.Run("backup "+identity, func(t *testing.T) {
			fixture.identity = identity
			fixture.dump = "dump-1"
			backup := fixture.newBackup(t)
			beforeExec := len(fixture.executor.requests())
			beforeSnapshots := len(fixture.files.testSnapshotCalls())
			beforeFiles := len(fixture.files.writtenFiles())
			result, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
			if err == nil {
				t.Fatalf("Backup accepted identity %q", identity)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("invalid identity error class = %v; want invalid_input (%v)", class, err)
			}
			if result.Identity.ID != "" {
				t.Fatalf("invalid identity returned %q; want none", result.Identity.ID)
			}
			if got := len(fixture.executor.requests()); got != beforeExec {
				t.Fatalf("invalid identity made %d executor calls; want 0", got-beforeExec)
			}
			if got := len(fixture.files.testSnapshotCalls()); got != beforeSnapshots {
				t.Fatalf("invalid identity made %d snapshots; want 0", got-beforeSnapshots)
			}
			if got := len(fixture.files.writtenFiles()); got != beforeFiles {
				t.Fatalf("invalid identity wrote files: %v", fixture.files.writtenFiles())
			}
		})
	}

	restore := fixture.newRestore(t)
	for _, identity := range []string{"../x", "a/b", ""} {
		t.Run("preflight "+identity, func(t *testing.T) {
			beforeExec := len(fixture.executor.requests())
			beforeFiles := len(fixture.files.writtenFiles())
			if _, err := restore.Preflight(ctx, installation.BackupIdentity{ID: identity}); err == nil {
				t.Fatalf("Preflight accepted identity %q", identity)
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("invalid identity error class = %v; want invalid_input (%v)", class, err)
			}
			if got := len(fixture.executor.requests()); got != beforeExec {
				t.Fatalf("invalid preflight made %d executor calls; want 0", got-beforeExec)
			}
			if got := len(fixture.files.writtenFiles()); got != beforeFiles {
				t.Fatalf("invalid preflight wrote files: %v", fixture.files.writtenFiles())
			}
		})
	}
}

// TestPreflightRefusesTruncatedDump proves the dump size must match the
// manifest before a binding is armed.
func TestPreflightRefusesTruncatedDump(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()
	backup := fixture.newBackup(t)
	result, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	setDir := filepath.Join(fixture.backupRoot, result.Identity.ID)
	fixture.files.setFile(filepath.Join(setDir, "database.dump"), []byte("short"))
	restore := fixture.newRestore(t)
	before := len(fixture.executor.requests())
	if _, err := restore.Preflight(ctx, result.Identity); err == nil {
		t.Fatal("Preflight accepted a truncated database dump")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
		t.Fatalf("truncated dump error class = %v; want internal_failure (%v)", class, err)
	}
	if got := len(fixture.executor.requests()); got != before {
		t.Fatalf("truncated preflight made %d executor calls; want 0", got-before)
	}
	if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
		t.Fatal("Restore ran without a valid preflight")
	}
	if got := len(fixture.executor.requests()); got != before {
		t.Fatalf("unarmed Restore made %d pg_restore calls; want 0", got-before)
	}
}

// TestFenceRefusesUnconfirmedStopEvidence proves an actor that already exited
// without whole-container evidence, or a created actor that may have started,
// is refused without retrying a stop.
func TestFenceRefusesUnconfirmedStopEvidence(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name  string
		state runtimepodman.ContainerState
	}{
		{
			name: "exited without whole-container evidence",
			state: runtimepodman.ContainerState{
				ID:           "id-exited",
				Name:         "exited",
				Status:       runtimepodman.ContainerStatusExited,
				Started:      true,
				WholeStopped: false,
			},
		},
		{
			name: "created but started",
			state: runtimepodman.ContainerState{
				ID:      "id-created",
				Name:    "created",
				Status:  runtimepodman.ContainerStatusCreated,
				Started: true,
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			libpod := newTestFakeLibpod()
			libpod.states[testCase.state.Name] = testCase.state
			fence, err := NewFenceDriver(FenceConfig{
				Libpod:    libpod,
				Inventory: []Actor{{Kind: ActorController, Name: testCase.state.Name}},
			})
			if err != nil {
				t.Fatalf("NewFenceDriver: %v", err)
			}
			if err := fence.Fence(ctx); err == nil {
				t.Fatal("Fence reported success without affirmative stop evidence")
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
				t.Fatalf("unconfirmed stop class = %v; want uncertain_external_effect (%v)", class, err)
			}
			if got := libpod.stopNames(); len(got) != 0 {
				t.Fatalf("Fence retried a stop for an already-exited/ambiguous actor: %v", got)
			}
		})
	}
}

// TestFenceStopsCreatedStartPending proves a created actor whose start outcome
// is pending is stopped once rather than accepted as never-run.
func TestFenceStopsCreatedStartPending(t *testing.T) {
	libpod := newTestFakeLibpod()
	libpod.states["pending"] = runtimepodman.ContainerState{
		ID:           "id-pending",
		Name:         "pending",
		Status:       runtimepodman.ContainerStatusCreated,
		StartPending: true,
	}
	fence, err := NewFenceDriver(FenceConfig{
		Libpod:    libpod,
		Inventory: []Actor{{Kind: ActorController, Name: "pending"}},
	})
	if err != nil {
		t.Fatalf("NewFenceDriver: %v", err)
	}
	if err := fence.Fence(t.Context()); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if got := libpod.stopNames(); !reflect.DeepEqual(got, []string{"pending"}) {
		t.Fatalf("stop calls = %v; want exactly one attempt for the start-pending actor", got)
	}
	if got := libpod.inspectNames(); !reflect.DeepEqual(got, []string{"pending", "pending"}) {
		t.Fatalf("inspect calls = %v; want one before and one after the stop", got)
	}
}

func TestCancellationAndUncertaintyNeverReplay(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()

	t.Run("cancelled context performs no effect", func(t *testing.T) {
		libpod := newTestFakeLibpod()
		libpod.states["ctl-1"] = runningState("ctl-1")
		fence, err := NewFenceDriver(FenceConfig{Libpod: libpod, Inventory: []Actor{{Kind: ActorController, Name: "ctl-1"}}})
		if err != nil {
			t.Fatalf("NewFenceDriver: %v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := fence.Fence(cancelled); err == nil {
			t.Fatal("Fence ran with a cancelled context")
		}
		if len(libpod.inspectNames()) != 0 || len(libpod.stopNames()) != 0 {
			t.Fatalf("cancelled fence made libpod calls: inspect=%v stop=%v", libpod.inspectNames(), libpod.stopNames())
		}

		backup := fixture.newBackup(t)
		before := len(fixture.executor.requests())
		if _, err := backup.Backup(cancelled, installation.BackupPlan{Scope: installation.DefaultBackupScope()}); err == nil {
			t.Fatal("Backup ran with a cancelled context")
		}
		if got := len(fixture.executor.requests()); got != before {
			t.Fatalf("cancelled backup made %d executor calls; want 0", got-before)
		}

		restore := fixture.newRestore(t)
		if _, err := restore.Preflight(cancelled, installation.BackupIdentity{ID: "backup-1"}); err == nil {
			t.Fatal("Preflight ran with a cancelled context")
		}
	})

	t.Run("ambiguous fence stop refuses without retry", func(t *testing.T) {
		libpod := newTestFakeLibpod()
		libpod.states["ctl-1"] = runningState("ctl-1")
		libpod.stopErr["ctl-1"] = errors.New("ambiguous external effect")
		fence, err := NewFenceDriver(FenceConfig{Libpod: libpod, Inventory: []Actor{{Kind: ActorController, Name: "ctl-1"}}})
		if err != nil {
			t.Fatalf("NewFenceDriver: %v", err)
		}
		if err := fence.Fence(ctx); err == nil {
			t.Fatal("Fence reported success for an ambiguous stop")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
			t.Fatalf("ambiguous stop class = %v; want uncertain_external_effect (%v)", class, err)
		}
		if got := libpod.stopNames(); !reflect.DeepEqual(got, []string{"ctl-1"}) {
			t.Fatalf("stop calls = %v; want exactly one attempt", got)
		}
	})

	t.Run("unconfirmed fence stop refuses", func(t *testing.T) {
		libpod := newTestFakeLibpod()
		libpod.states["ctl-1"] = runningState("ctl-1")
		libpod.stopNoEffect["ctl-1"] = true
		fence, err := NewFenceDriver(FenceConfig{Libpod: libpod, Inventory: []Actor{{Kind: ActorController, Name: "ctl-1"}}})
		if err != nil {
			t.Fatalf("NewFenceDriver: %v", err)
		}
		if err := fence.Fence(ctx); err == nil {
			t.Fatal("Fence reported success without whole-container stop evidence")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
			t.Fatalf("unconfirmed stop class = %v; want uncertain_external_effect (%v)", class, err)
		}
	})

	t.Run("missing fence actor refuses", func(t *testing.T) {
		libpod := newTestFakeLibpod()
		fence, err := NewFenceDriver(FenceConfig{Libpod: libpod, Inventory: []Actor{{Kind: ActorService, Name: "missing"}}})
		if err != nil {
			t.Fatalf("NewFenceDriver: %v", err)
		}
		if err := fence.Fence(ctx); err == nil {
			t.Fatal("Fence reported success for a missing actor")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("missing actor class = %v; want not_found (%v)", class, err)
		}
	})

	t.Run("restore ambiguity is not replayed", func(t *testing.T) {
		backup := fixture.newBackup(t)
		result, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		restore := fixture.newRestore(t)
		if _, err := restore.Preflight(ctx, result.Identity); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		fixture.executor.setError(errors.New("ambiguous external effect"))
		before := len(fixture.executor.requests())
		if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
			t.Fatal("Restore reported success for an ambiguous pg_restore")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
			t.Fatalf("ambiguous restore class = %v; want uncertain_external_effect (%v)", class, err)
		}
		if got := len(fixture.executor.requests()) - before; got != 1 {
			t.Fatalf("restore attempts = %d; want exactly 1", got)
		}
		// The binding is consumed before the effect, so a second call without a
		// new preflight cannot replay the restore.
		if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: result.Scope}); err == nil {
			t.Fatal("Restore replayed after an ambiguous effect")
		}
		if got := len(fixture.executor.requests()) - before; got != 1 {
			t.Fatalf("second Restore made an effect; attempts = %d; want 1", got)
		}
	})

	t.Run("reconcile unknown blocks and never launches", func(t *testing.T) {
		fixture.observer.set("workload-1", WorkloadObservation{
			Identity: WorkloadIdentity{ID: "workload-1"},
			State:    ObservationUnknown,
			Detail:   "inspect timed out",
		})
		restore := fixture.newRestore(t)
		before := len(fixture.executor.requests())
		result, err := restore.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if result.Reconciled {
			t.Fatal("Reconcile reported success with an unknown effect")
		}
		if !reflect.DeepEqual(result.Unresolved, []string{"workload-1"}) {
			t.Fatalf("unresolved = %v; want [workload-1]", result.Unresolved)
		}
		if got := len(fixture.executor.requests()); got != before {
			t.Fatalf("Reconcile made %d executor calls; want 0 (never launch or replay)", got-before)
		}
	})

	t.Run("crash before manifest leaves no usable backup", func(t *testing.T) {
		fixture.executor.setError(nil)
		fixture.identity = "backup-crash"
		fixture.dump = "dump-crash"
		backup := fixture.newBackup(t)
		manifestPath := filepath.Join(fixture.backupRoot, "backup-crash", "manifest.json")
		fixture.files.setWriteError(manifestPath, errors.New("crash before manifest commit"))
		failed, err := backup.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()})
		if err == nil {
			t.Fatal("Backup reported success without committing its manifest")
		}
		if failed.Identity.ID != "" {
			t.Fatalf("crash-before-manifest returned identity %q; want none", failed.Identity.ID)
		}
		// The dump and source trees may exist, but without the manifest the set
		// is not a usable backup: preflight must refuse it rather than pretend a
		// partial capture is complete.
		if _, err := fixture.newRestore(t).Preflight(ctx, installation.BackupIdentity{ID: "backup-crash"}); err == nil {
			t.Fatal("Preflight accepted a backup set without a manifest")
		}
	})
}

func TestConcurrentPreflightCannotRetarget(t *testing.T) {
	fixture := newTestDriverFixture(t)
	ctx := t.Context()

	// Two complete backup sets with distinguishable database dumps.
	fixture.identity = "backup-a"
	fixture.dump = "dump-1"
	backupA := fixture.newBackup(t)
	if _, err := backupA.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()}); err != nil {
		t.Fatalf("Backup A: %v", err)
	}
	fixture.identity = "backup-b"
	fixture.dump = "dump-b"
	backupB := fixture.newBackup(t)
	if _, err := backupB.Backup(ctx, installation.BackupPlan{Scope: installation.DefaultBackupScope()}); err != nil {
		t.Fatalf("Backup B: %v", err)
	}

	t.Run("a second identity is refused while armed", func(t *testing.T) {
		restore := fixture.newRestore(t)
		if _, err := restore.Preflight(ctx, installation.BackupIdentity{ID: "backup-a"}); err != nil {
			t.Fatalf("Preflight A: %v", err)
		}
		if _, err := restore.Preflight(ctx, installation.BackupIdentity{ID: "backup-b"}); err == nil {
			t.Fatal("a second preflight retargeted the armed restore")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("retarget error class = %v; want version_conflict (%v)", class, err)
		}
		before := len(fixture.executor.requests())
		if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: installation.DefaultBackupScope()}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		requests := fixture.executor.requests()
		if len(requests) != before+1 {
			t.Fatalf("restore made %d executor calls; want 1", len(requests)-before)
		}
		if string(requests[len(requests)-1].Stdin) != "dump-1" {
			t.Fatalf("restore used %q; want backup-a's dump", requests[len(requests)-1].Stdin)
		}
		// After the binding is consumed, a new preflight may arm B.
		if _, err := restore.Preflight(ctx, installation.BackupIdentity{ID: "backup-b"}); err != nil {
			t.Fatalf("Preflight B after restore: %v", err)
		}
	})

	t.Run("concurrent preflights have one winner and one restore identity", func(t *testing.T) {
		restore := fixture.newRestore(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]installation.BackupEvidence, 2)
		errs := make([]error, 2)
		for index, identity := range []string{"backup-a", "backup-b"} {
			wg.Add(1)
			go func(index int, identity string) {
				defer wg.Done()
				<-start
				results[index], errs[index] = restore.Preflight(ctx, installation.BackupIdentity{ID: identity})
			}(index, identity)
		}
		close(start)
		wg.Wait()

		successes := 0
		winner := ""
		for index, err := range errs {
			if err == nil {
				successes++
				winner = results[index].Identity.ID
				continue
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
				t.Fatalf("concurrent preflight error class = %v; want version_conflict (%v)", class, err)
			}
		}
		if successes != 1 {
			t.Fatalf("concurrent preflights succeeded %d times; want exactly 1", successes)
		}
		before := len(fixture.executor.requests())
		if _, err := restore.Restore(ctx, installation.RestorePlan{Scope: installation.DefaultBackupScope()}); err != nil {
			t.Fatalf("Restore after concurrent preflight: %v", err)
		}
		requests := fixture.executor.requests()
		if len(requests) != before+1 {
			t.Fatalf("restore made %d executor calls; want 1", len(requests)-before)
		}
		wantDump := "dump-1"
		if winner == "backup-b" {
			wantDump = "dump-b"
		}
		if got := string(requests[len(requests)-1].Stdin); got != wantDump {
			t.Fatalf("restore used %q; want the winning preflight %q", got, wantDump)
		}
	})

	t.Run("concurrent restores perform one effect", func(t *testing.T) {
		restore := fixture.newRestore(t)
		if _, err := restore.Preflight(ctx, installation.BackupIdentity{ID: "backup-a"}); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		before := len(fixture.executor.requests())
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for index := range errs {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				<-start
				_, errs[index] = restore.Restore(ctx, installation.RestorePlan{Scope: installation.DefaultBackupScope()})
			}(index)
		}
		close(start)
		wg.Wait()
		successes := 0
		for _, err := range errs {
			if err == nil {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("concurrent restores succeeded %d times; want exactly 1", successes)
		}
		if got := len(fixture.executor.requests()) - before; got != 1 {
			t.Fatalf("concurrent restores made %d effects; want 1", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Fixtures and fakes
// ---------------------------------------------------------------------------

type testDriverFixture struct {
	executor   *testFakeExecutor
	files      *testFakeFiles
	observer   *testScriptedObserver
	preserved  func(context.Context) (installation.PreservedState, error)
	backupRoot string
	identity   string
	dump       string

	retainedLogs  string
	keys          string
	jetstream     string
	workspaceRoot string

	restoreLogs      string
	restoreKeys      string
	restoreJetStream string

	jetStreamReadyCalls int
	jetStreamReady      func(context.Context) error
}

func newTestDriverFixture(t *testing.T) *testDriverFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &testDriverFixture{
		executor:         newTestFakeExecutor("dump-1"),
		files:            newTestFakeFiles(),
		observer:         newTestScriptedObserver(),
		backupRoot:       filepath.Join(root, "backups"),
		identity:         "backup-1",
		dump:             "dump-1",
		retainedLogs:     filepath.Join(root, "retained-logs"),
		keys:             filepath.Join(root, "keys"),
		jetstream:        filepath.Join(root, "jetstream"),
		workspaceRoot:    filepath.Join(root, "workspaces", "workspace-1"),
		restoreLogs:      filepath.Join(root, "restore", "retained-logs"),
		restoreKeys:      filepath.Join(root, "restore", "keys"),
		restoreJetStream: filepath.Join(root, "restore", "jetstream"),
	}
	for _, dir := range []string{
		fixture.backupRoot, fixture.retainedLogs, fixture.keys, fixture.jetstream,
		fixture.workspaceRoot, fixture.restoreLogs, fixture.restoreKeys, fixture.restoreJetStream,
	} {
		fixture.files.addDirectory(dir)
	}
	fixture.files.setSnapshotCount("retained-logs", 2)
	fixture.files.setSnapshotCount("keys", 1)
	fixture.files.setSnapshotCount("jetstream", 4)
	fixture.preserved = func(context.Context) (installation.PreservedState, error) {
		return installation.PreservedState{Receipts: 1, Decisions: 2, QueuedExecutions: 3}, nil
	}
	fixture.jetStreamReady = func(context.Context) error {
		fixture.jetStreamReadyCalls++
		return nil
	}
	fixture.observer.set("workload-1", WorkloadObservation{
		Identity: WorkloadIdentity{ID: "workload-1"},
		State:    ObservationStopped,
	})
	fixture.observer.set("workload-2", WorkloadObservation{
		Identity: WorkloadIdentity{ID: "workload-2"},
		State:    ObservationAbsent,
	})
	return fixture
}

func (fixture *testDriverFixture) newBackup(t *testing.T) *BackupDriver {
	t.Helper()
	identity := fixture.identity
	dump := fixture.dump
	backup, err := NewBackupDriver(BackupConfig{
		Executor:  fixture.executor,
		Files:     fixture.files,
		Container: "pg-container",
		Database:  "kodama",
		Root:      fixture.backupRoot,
		Sources: []BackupSource{
			{Name: "retained-logs", Root: fixture.retainedLogs},
			{Name: "keys", Root: fixture.keys},
			{Name: "jetstream", Root: fixture.jetstream, Ready: fixture.jetStreamReady},
		},
		Workspaces:  []Workspace{{ID: "workspace-1", Root: fixture.workspaceRoot}},
		Preserved:   fixture.preserved,
		NewIdentity: func() string { return identity },
	})
	if err != nil {
		t.Fatalf("NewBackupDriver: %v", err)
	}
	fixture.executor.setResult(ExecResult{Stdout: []byte(dump)})
	return backup
}

func (fixture *testDriverFixture) newRestore(t *testing.T) *RestoreDriver {
	t.Helper()
	restore, err := NewRestoreDriver(RestoreConfig{
		Executor:  fixture.executor,
		Files:     fixture.files,
		Container: "restore-pg",
		Database:  "kodama",
		Root:      fixture.backupRoot,
		Targets: []RestoreTarget{
			{Name: "retained-logs", Root: fixture.restoreLogs},
			{Name: "keys", Root: fixture.restoreKeys},
			{Name: "jetstream", Root: fixture.restoreJetStream},
		},
		Workloads: []WorkloadIdentity{{ID: "workload-1"}, {ID: "workload-2"}},
		Observer:  fixture.observer,
		Services:  []string{"controller", "publisher"},
	})
	if err != nil {
		t.Fatalf("NewRestoreDriver: %v", err)
	}
	return restore
}

func (fixture *testDriverFixture) newRestoreWithTargets(t *testing.T, targets []RestoreTarget) *RestoreDriver {
	t.Helper()
	restore, err := NewRestoreDriver(RestoreConfig{
		Executor:  fixture.executor,
		Files:     fixture.files,
		Container: "restore-pg",
		Database:  "kodama",
		Root:      fixture.backupRoot,
		Targets:   targets,
		Services:  []string{"controller", "publisher"},
	})
	if err != nil {
		t.Fatalf("NewRestoreDriver: %v", err)
	}
	return restore
}

func (fixture *testDriverFixture) manifest(t *testing.T, setDir string) testBackupManifest {
	t.Helper()
	data := fixture.files.file(filepath.Join(setDir, "manifest.json"))
	if len(data) == 0 {
		t.Fatal("backup manifest was not written")
	}
	var manifest testBackupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return manifest
}

// testBackupManifest is the documented on-disk evidence format.
type testBackupManifest struct {
	Format        int                   `json:"format"`
	Identity      string                `json:"identity"`
	Scope         testManifestScope     `json:"scope"`
	Sources       map[string]int        `json:"sources"`
	DatabaseBytes int                   `json:"database_bytes"`
	Preserved     testManifestPreserved `json:"preserved"`
	Workspaces    []string              `json:"workspaces"`
}

type testManifestScope struct {
	Database       bool `json:"database"`
	RetainedLogs   bool `json:"retained_logs"`
	EncryptionKeys bool `json:"encryption_keys"`
	JetStream      bool `json:"jetstream"`
	Workspaces     bool `json:"workspaces"`
}

func (scope testManifestScope) installationScope() installation.BackupScope {
	return installation.BackupScope{
		Database:       scope.Database,
		RetainedLogs:   scope.RetainedLogs,
		EncryptionKeys: scope.EncryptionKeys,
		JetStream:      scope.JetStream,
		Workspaces:     scope.Workspaces,
	}
}

type testManifestPreserved struct {
	Receipts         int `json:"receipts"`
	Decisions        int `json:"decisions"`
	QueuedExecutions int `json:"queued_executions"`
}

func (preserved testManifestPreserved) installationState() installation.PreservedState {
	return installation.PreservedState{
		Receipts:         preserved.Receipts,
		Decisions:        preserved.Decisions,
		QueuedExecutions: preserved.QueuedExecutions,
	}
}

// testFakeLibpod is a deterministic implementation of the Libpod seam.
type testFakeLibpod struct {
	mu           sync.Mutex
	states       map[string]runtimepodman.ContainerState
	inspectErr   map[string]error
	stopErr      map[string]error
	stopNoEffect map[string]bool
	inspects     []string
	stops        []string
	listCalls    int
	otherCalls   int
}

func newTestFakeLibpod() *testFakeLibpod {
	return &testFakeLibpod{
		states:       map[string]runtimepodman.ContainerState{},
		inspectErr:   map[string]error{},
		stopErr:      map[string]error{},
		stopNoEffect: map[string]bool{},
	}
}

func (f *testFakeLibpod) CreateContainer(context.Context, runtimepodman.ContainerSpec) (runtimepodman.ContainerState, error) {
	f.otherCalls++
	return runtimepodman.ContainerState{}, errors.New("testFakeLibpod: create is not expected")
}

func (f *testFakeLibpod) InspectImage(context.Context, string) error {
	f.otherCalls++
	return errors.New("testFakeLibpod: image inspect is not expected")
}

func (f *testFakeLibpod) InspectContainer(_ context.Context, name string) (runtimepodman.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects = append(f.inspects, name)
	if err := f.inspectErr[name]; err != nil {
		return runtimepodman.ContainerState{}, err
	}
	state, ok := f.states[name]
	if !ok {
		return runtimepodman.ContainerState{}, runtimepodman.ErrNotFound
	}
	return state, nil
}

func (f *testFakeLibpod) ListContainers(context.Context, map[string]string) ([]runtimepodman.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return nil, errors.New("testFakeLibpod: list is not expected")
}

func (f *testFakeLibpod) StartContainer(context.Context, string) error {
	f.otherCalls++
	return errors.New("testFakeLibpod: start is not expected")
}

func (f *testFakeLibpod) StopContainer(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, name)
	if err := f.stopErr[name]; err != nil {
		return err
	}
	if f.stopNoEffect[name] {
		return nil
	}
	state, ok := f.states[name]
	if !ok {
		return runtimepodman.ErrNotFound
	}
	state.Status = runtimepodman.ContainerStatusExited
	// Real libpod reports StartedAt for a container that ran, so Started stays
	// true after termination; WholeStopped is the affirmative stop evidence.
	state.Started = true
	state.StartPending = false
	state.WholeStopped = true
	f.states[name] = state
	return nil
}

func (f *testFakeLibpod) RemoveContainer(context.Context, string) error {
	f.otherCalls++
	return errors.New("testFakeLibpod: remove is not expected")
}

func (f *testFakeLibpod) ReadLogs(context.Context, runtimepodman.LogRequest) (runtimepodman.LogPage, error) {
	f.otherCalls++
	return runtimepodman.LogPage{}, errors.New("testFakeLibpod: logs are not expected")
}

func (f *testFakeLibpod) InspectVolume(context.Context, string) (runtimepodman.Volume, error) {
	f.otherCalls++
	return runtimepodman.Volume{}, errors.New("testFakeLibpod: volume inspect is not expected")
}

func (f *testFakeLibpod) RemoveVolume(context.Context, string) error {
	f.otherCalls++
	return errors.New("testFakeLibpod: volume remove is not expected")
}

func (f *testFakeLibpod) inspectNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inspects...)
}

func (f *testFakeLibpod) stopNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stops...)
}

func runningState(name string) runtimepodman.ContainerState {
	return runtimepodman.ContainerState{
		ID:      "id-" + name,
		Name:    name,
		Status:  runtimepodman.ContainerStatusRunning,
		Started: true,
	}
}

func createdState(name string) runtimepodman.ContainerState {
	return runtimepodman.ContainerState{
		ID:     "id-" + name,
		Name:   name,
		Status: runtimepodman.ContainerStatusCreated,
	}
}

func exitedState(name string) runtimepodman.ContainerState {
	return runtimepodman.ContainerState{
		ID:           "id-" + name,
		Name:         name,
		Status:       runtimepodman.ContainerStatusExited,
		Started:      true,
		WholeStopped: true,
	}
}

type testFakeExecutor struct {
	mu           sync.Mutex
	requestsList []ExecRequest
	probesList   []ExecRequest
	result       ExecResult
	err          error
	roleNames    []string
	probeErr     error
}

func newTestFakeExecutor(dump string) *testFakeExecutor {
	return &testFakeExecutor{
		result:    ExecResult{Stdout: []byte(dump)},
		roleNames: expectedDatabaseRoles(),
	}
}

func (f *testFakeExecutor) Exec(_ context.Context, request ExecRequest) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request.Stdin = append([]byte(nil), request.Stdin...)
	request.Argv = append([]string(nil), request.Argv...)
	// The role probe is a read-only precondition check, not a backup/restore
	// effect, so it is recorded separately from the effect requests.
	if len(request.Argv) > 0 && request.Argv[0] == "psql" {
		f.probesList = append(f.probesList, request)
		if f.probeErr != nil {
			return ExecResult{}, f.probeErr
		}
		return ExecResult{Stdout: []byte(strings.Join(f.roleNames, "\n") + "\n")}, nil
	}
	f.requestsList = append(f.requestsList, request)
	return f.result, f.err
}

func (f *testFakeExecutor) setResult(result ExecResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result = result
}

func (f *testFakeExecutor) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *testFakeExecutor) setRoles(present []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roleNames = append([]string(nil), present...)
}

func (f *testFakeExecutor) setProbeError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeErr = err
}

func (f *testFakeExecutor) requests() []ExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ExecRequest(nil), f.requestsList...)
}

func (f *testFakeExecutor) probes() []ExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ExecRequest(nil), f.probesList...)
}

type testSnapshotCall struct {
	Source      string
	Destination string
}

type testRestoreCall struct {
	Source      string
	Destination string
}

type testFakeFiles struct {
	mu          sync.Mutex
	files       map[string][]byte
	dirs        map[string]bool
	stats       map[string]FileInfo
	statErr     map[string]error
	readErr     map[string]error
	writeErr    map[string]error
	snapshots   []testSnapshotCall
	snapshotN   map[string]int
	snapshotErr map[string]error
	restores    []testRestoreCall
	restoreErr  map[string]error
	emptyCalls  []string
	emptyErr    map[string]error
	removed     []string
}

func newTestFakeFiles() *testFakeFiles {
	return &testFakeFiles{
		files:       map[string][]byte{},
		dirs:        map[string]bool{},
		stats:       map[string]FileInfo{},
		statErr:     map[string]error{},
		readErr:     map[string]error{},
		writeErr:    map[string]error{},
		snapshotN:   map[string]int{},
		snapshotErr: map[string]error{},
		restoreErr:  map[string]error{},
		emptyErr:    map[string]error{},
	}
}

func (f *testFakeFiles) addDirectory(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[path] = true
	f.stats[path] = FileInfo{Kind: FileDirectory}
}

func (f *testFakeFiles) setSnapshotCount(name string, count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotN[name] = count
}

func (f *testFakeFiles) setSnapshotError(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotErr[name] = err
}

func (f *testFakeFiles) setWriteError(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErr[path] = err
}

// setFile records path as a regular file, creating its parents as directories
// so Stat and EnsureEmpty observe the same tree an operating-system boundary
// would.
func (f *testFakeFiles) setFile(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addDirectoryLocked(filepath.Dir(path))
	f.files[path] = append([]byte(nil), data...)
}

// writtenFiles returns the recorded file paths in sorted order.
func (f *testFakeFiles) writtenFiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, 0, len(f.files))
	for path := range f.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// removeTree deletes path and everything below it, including stat entries.
func (f *testFakeFiles) removeTree(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := path + string(os.PathSeparator)
	for file := range f.files {
		if file == path || strings.HasPrefix(file, prefix) {
			delete(f.files, file)
		}
	}
	for dir := range f.dirs {
		if dir == path || strings.HasPrefix(dir, prefix) {
			delete(f.dirs, dir)
		}
	}
	for stat := range f.stats {
		if stat == path || strings.HasPrefix(stat, prefix) {
			delete(f.stats, stat)
		}
	}
}

func (f *testFakeFiles) EnsureEmpty(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emptyCalls = append(f.emptyCalls, path)
	if err := f.emptyErr[path]; err != nil {
		return err
	}
	prefix := path + string(os.PathSeparator)
	for file := range f.files {
		if file == path || strings.HasPrefix(file, prefix) {
			return fault.New(fault.InvalidInput, "destination is not empty")
		}
	}
	for dir := range f.dirs {
		if dir != path && strings.HasPrefix(dir, prefix) {
			return fault.New(fault.InvalidInput, "destination is not empty")
		}
	}
	return nil
}

func (f *testFakeFiles) Snapshot(_ context.Context, source, destination string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots = append(f.snapshots, testSnapshotCall{Source: source, Destination: destination})
	name := filepath.Base(source)
	if err := f.snapshotErr[name]; err != nil {
		return 0, err
	}
	f.addDirectoryLocked(destination)
	return f.snapshotN[name], nil
}

func (f *testFakeFiles) Restore(_ context.Context, source, destination string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, testRestoreCall{Source: source, Destination: destination})
	return f.restoreErr[source]
}

func (f *testFakeFiles) Stat(_ context.Context, path string) (FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.statErr[path]; err != nil {
		return FileInfo{}, err
	}
	if info, ok := f.stats[path]; ok {
		return info, nil
	}
	if data, ok := f.files[path]; ok {
		return FileInfo{Kind: FileRegular, Size: int64(len(data))}, nil
	}
	if f.dirs[path] {
		return FileInfo{Kind: FileDirectory}, nil
	}
	return FileInfo{}, os.ErrNotExist
}

func (f *testFakeFiles) ReadFile(_ context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.readErr[path]; err != nil {
		return nil, err
	}
	data, ok := f.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (f *testFakeFiles) WriteFile(_ context.Context, path string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.writeErr[path]; err != nil {
		return err
	}
	f.addDirectoryLocked(filepath.Dir(path))
	f.files[path] = append([]byte(nil), data...)
	return nil
}

// addDirectoryLocked records path and its parents as directories so a Stat
// after a write observes the same tree an operating-system boundary would.
func (f *testFakeFiles) addDirectoryLocked(path string) {
	for {
		if path == "" || path == "." || path == string(os.PathSeparator) {
			return
		}
		f.dirs[path] = true
		if _, ok := f.stats[path]; !ok {
			f.stats[path] = FileInfo{Kind: FileDirectory}
		}
		parent := filepath.Dir(path)
		if parent == path {
			return
		}
		path = parent
	}
}

func (f *testFakeFiles) RemoveAll(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, path)
	prefix := path + string(os.PathSeparator)
	for file := range f.files {
		if file == path || strings.HasPrefix(file, prefix) {
			delete(f.files, file)
		}
	}
	for dir := range f.dirs {
		if dir == path || strings.HasPrefix(dir, prefix) {
			delete(f.dirs, dir)
		}
	}
	return nil
}

func (f *testFakeFiles) file(path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.files[path]...)
}

func (f *testFakeFiles) testSnapshotCalls() []testSnapshotCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]testSnapshotCall(nil), f.snapshots...)
}

func (f *testFakeFiles) testRestoreCalls() []testRestoreCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]testRestoreCall(nil), f.restores...)
}

func (f *testFakeFiles) ensureEmptyCalled(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.emptyCalls, path)
}

type testScriptedObserver struct {
	mu   sync.Mutex
	seen []string
	byID map[string]WorkloadObservation
}

func newTestScriptedObserver() *testScriptedObserver {
	return &testScriptedObserver{byID: map[string]WorkloadObservation{}}
}

func (o *testScriptedObserver) set(id string, observation WorkloadObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.byID[id] = observation
}

func (o *testScriptedObserver) Observe(_ context.Context, identity WorkloadIdentity) (WorkloadObservation, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, identity.ID)
	if observation, ok := o.byID[identity.ID]; ok {
		return observation, nil
	}
	return WorkloadObservation{Identity: identity, State: ObservationAbsent}, nil
}

func (o *testScriptedObserver) observed() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := append([]string(nil), o.seen...)
	sort.Strings(out)
	return out
}

type testRecordingFence struct {
	calls int
}

func (f *testRecordingFence) Fence(context.Context) error {
	f.calls++
	return nil
}

type testRecordingDispatch struct {
	calls int
}

func (d *testRecordingDispatch) EnableDispatch(context.Context) error {
	d.calls++
	return nil
}

func pathWithin(path, root string) bool {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	return cleanPath == cleanRoot || strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator))
}
