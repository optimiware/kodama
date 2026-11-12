package installation

// Backup and restore orchestration tests. The fakes prove the runner drives
// fence -> restore -> reconcile -> dispatch from observed outcomes rather than
// caller-supplied booleans, blocks dispatch on every failure and uncertainty,
// and never replays or recreates work. No real backup, restore, database,
// broker, container or cloud client is involved.

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

func TestRestoreOrderFrozen(t *testing.T) {
	want := []RestoreStep{
		RestoreFence,
		RestoreState,
		RestoreReconcile,
		RestoreEnableDispatch,
	}
	got := RestoreOrder()
	if len(got) != len(want) {
		t.Fatalf("RestoreOrder() = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("RestoreOrder() = %v, want %v", got, want)
		}
	}
}

func TestRunBackup(t *testing.T) {
	identity := BackupIdentity{ID: "backup-2026-11-04"}
	scope := DefaultBackupScope()
	log, backup, restore, fence, dispatch := newFakes()
	backup.result = BackupResult{Identity: identity, Scope: scope, Detail: "stored"}

	runner := recovery(log, backup, restore, fence, dispatch)
	result, err := runner.RunBackup(context.Background(), BackupPlan{Scope: scope})
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	if result.Identity != identity {
		t.Errorf("identity = %+v, want %+v", result.Identity, identity)
	}
	if result.Scope != scope {
		t.Errorf("scope = %+v, want %+v", result.Scope, scope)
	}
	if result.Detail != "stored" {
		t.Errorf("detail = %q", result.Detail)
	}
	assertCalls(t, log, "backup")
}

func TestRunBackupRefusesInvalidScope(t *testing.T) {
	scope := DefaultBackupScope()
	scope.Workspaces = true
	scope.JetStream = false
	log, backup, restore, fence, dispatch := newFakes()
	backup.result = BackupResult{Identity: BackupIdentity{ID: "backup-1"}, Scope: DefaultBackupScope()}

	runner := recovery(log, backup, restore, fence, dispatch)
	if _, err := runner.RunBackup(context.Background(), BackupPlan{Scope: scope}); err == nil {
		t.Fatal("RunBackup accepted a scope with workspaces and no JetStream")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	assertCalls(t, log)
}

func TestRunBackupPropagatesFailureWithoutRetry(t *testing.T) {
	sentinel := errors.New("storage unavailable")
	log, backup, restore, fence, dispatch := newFakes()
	backup.err = fault.Wrap(fault.DependencyUnavailable, "backup storage unavailable", sentinel)

	runner := recovery(log, backup, restore, fence, dispatch)
	_, err := runner.RunBackup(context.Background(), BackupPlan{Scope: DefaultBackupScope()})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunBackup error = %v, want the port error", err)
	}
	assertCalls(t, log, "backup")
}

func TestRunBackupRefusesUnidentifiedOrIncompleteResult(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		result BackupResult
	}{
		{name: "missing identity", result: BackupResult{Scope: DefaultBackupScope()}},
		{
			name: "workspaces included",
			result: BackupResult{
				Identity: BackupIdentity{ID: "backup-1"},
				Scope: func() BackupScope {
					scope := DefaultBackupScope()
					scope.Workspaces = true
					return scope
				}(),
			},
		},
		{
			name: "missing durable family",
			result: BackupResult{
				Identity: BackupIdentity{ID: "backup-1"},
				Scope: func() BackupScope {
					scope := DefaultBackupScope()
					scope.EncryptionKeys = false
					return scope
				}(),
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			log, backup, restore, fence, dispatch := newFakes()
			backup.result = testCase.result
			runner := recovery(log, backup, restore, fence, dispatch)
			if _, err := runner.RunBackup(context.Background(), BackupPlan{Scope: DefaultBackupScope()}); err == nil {
				t.Fatal("RunBackup accepted an unusable backup result")
			} else {
				assertFaultClass(t, err, fault.InternalFailure)
			}
			assertCalls(t, log, "backup")
		})
	}
}

func TestRunBackupRefusesCancelledOrNilPorts(t *testing.T) {
	log, backup, restore, fence, dispatch := newFakes()
	runner := recovery(log, backup, restore, fence, dispatch)
	if _, err := runner.RunBackup(cancelledContext(t), BackupPlan{Scope: DefaultBackupScope()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunBackup cancelled error = %v", err)
	}
	assertCalls(t, log)

	_, err := (Recovery{}).RunBackup(context.Background(), BackupPlan{Scope: DefaultBackupScope()})
	assertFaultClass(t, err, fault.InternalFailure)
}

func TestRunRestoreOrderAndEvidence(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	log, backup, restore, fence, dispatch := newFakes()
	restore.evidence = evidence(identity)
	restore.evidence.MissingWorkspaces = []string{"workspace-1"}
	restore.restoreResult = RestoreResult{Detail: "restored"}
	restore.reconcileResult = ReconcileResult{
		Reconciled: true,
		Services:   []string{"git", "registry"},
		Detail:     "reconciled 2 services",
	}

	runner := recovery(log, backup, restore, fence, dispatch)
	result, err := runner.RunRestore(context.Background(), identity, RestorePlan{Scope: DefaultBackupScope()})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	assertCalls(t, log, "preflight", "fence", "restore", "reconcile", "dispatch")
	if dispatch.calls != 1 {
		t.Errorf("dispatch attempts = %d, want 1", dispatch.calls)
	}

	if !result.Fenced || !result.Reconciled {
		t.Errorf("result fenced/reconciled = %v/%v", result.Fenced, result.Reconciled)
	}
	if result.Preserved != restore.evidence.Preserved {
		t.Errorf("preserved = %+v, want %+v", result.Preserved, restore.evidence.Preserved)
	}
	if len(result.Services) != 2 || result.Services[0] != "git" || result.Services[1] != "registry" {
		t.Errorf("services = %v", result.Services)
	}
	if result.Detail != "reconciled 2 services" {
		t.Errorf("detail = %q", result.Detail)
	}
	if len(result.UnrecoverableWorkspaces) != 1 || result.UnrecoverableWorkspaces[0] != "workspace-1" {
		t.Errorf("unrecoverable workspaces = %v", result.UnrecoverableWorkspaces)
	}

	if len(restore.plans) != 1 {
		t.Fatalf("restore plans = %v", restore.plans)
	}
	plan := restore.plans[0]
	if plan.Scope != restore.evidence.Scope {
		t.Errorf("restore scope = %+v, want evidence scope %+v", plan.Scope, restore.evidence.Scope)
	}
	if !plan.Fenced {
		t.Error("restore plan was not marked fenced although fencing happened")
	}
	if plan.Reconciled {
		t.Error("restore plan was marked reconciled before reconciliation happened")
	}
	if plan.EnableDispatch {
		t.Error("restore plan enabled dispatch before the runner did")
	}
}

func TestRunRestoreIgnoresCallerBooleans(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	for _, testCase := range []struct {
		name string
		plan RestorePlan
	}{
		{name: "claims fenced and reconciled", plan: RestorePlan{
			Scope:          DefaultBackupScope(),
			Fenced:         true,
			Reconciled:     true,
			EnableDispatch: true,
		}},
		{name: "claims dispatch without fencing", plan: RestorePlan{
			Scope:          DefaultBackupScope(),
			EnableDispatch: true,
		}},
		{name: "claims nothing", plan: RestorePlan{Scope: DefaultBackupScope()}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			log, backup, restore, fence, dispatch := newFakes()
			restore.evidence = evidence(identity)
			restore.reconcileResult = ReconcileResult{Reconciled: true}
			runner := recovery(log, backup, restore, fence, dispatch)

			if _, err := runner.RunRestore(context.Background(), identity, testCase.plan); err != nil {
				t.Fatalf("RunRestore: %v", err)
			}
			assertCalls(t, log, "preflight", "fence", "restore", "reconcile", "dispatch")
			plan := restore.plans[0]
			if !plan.Fenced || plan.Reconciled || plan.EnableDispatch {
				t.Fatalf("restore plan = %+v, want Fenced=true Reconciled=false EnableDispatch=false", plan)
			}
		})
	}
}

func TestRunRestoreRefusesMissingBackupOrKeys(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	for _, testCase := range []struct {
		name      string
		configure func(*fakeRestore)
	}{
		{name: "missing backup", configure: func(fake *fakeRestore) {
			fake.preflightErr = fault.New(fault.NotFound, "backup not found")
		}},
		{name: "missing encryption keys", configure: func(fake *fakeRestore) {
			fake.evidence = evidence(identity)
			fake.evidence.KeysPresent = false
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			log, backup, restore, fence, dispatch := newFakes()
			restore.evidence = evidence(identity)
			testCase.configure(restore)
			runner := recovery(log, backup, restore, fence, dispatch)

			_, err := runner.RunRestore(context.Background(), identity, RestorePlan{Scope: DefaultBackupScope()})
			assertFaultClass(t, err, fault.NotFound)
			assertCalls(t, log, "preflight")
		})
	}
}

func TestRunRestoreRefusesForeignEvidence(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	log, backup, restore, fence, dispatch := newFakes()
	restore.evidence = evidence(BackupIdentity{ID: "backup-2"})
	runner := recovery(log, backup, restore, fence, dispatch)

	_, err := runner.RunRestore(context.Background(), identity, RestorePlan{Scope: DefaultBackupScope()})
	assertFaultClass(t, err, fault.InternalFailure)
	assertCalls(t, log, "preflight")
}

func TestRunRestoreRefusesInvalidEvidenceScope(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	log, backup, restore, fence, dispatch := newFakes()
	restore.evidence = evidence(identity)
	restore.evidence.Scope = BackupScope{}
	runner := recovery(log, backup, restore, fence, dispatch)

	_, err := runner.RunRestore(context.Background(), identity, RestorePlan{Scope: DefaultBackupScope()})
	assertFaultClass(t, err, fault.InternalFailure)
	assertCalls(t, log, "preflight")
}

func TestRunRestoreRefusesInvalidRequest(t *testing.T) {
	log, backup, restore, fence, dispatch := newFakes()
	runner := recovery(log, backup, restore, fence, dispatch)

	scope := DefaultBackupScope()
	scope.Workspaces = true
	if _, err := runner.RunRestore(context.Background(), BackupIdentity{ID: "backup-1"}, RestorePlan{Scope: scope}); err == nil {
		t.Fatal("RunRestore accepted a scope with workspaces")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}

	if _, err := runner.RunRestore(context.Background(), BackupIdentity{}, RestorePlan{Scope: DefaultBackupScope()}); err == nil {
		t.Fatal("RunRestore accepted an empty backup identity")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}

	assertCalls(t, log)
}

func TestRunRestoreRefusesCancelledOrNilPorts(t *testing.T) {
	log, backup, restore, fence, dispatch := newFakes()
	runner := recovery(log, backup, restore, fence, dispatch)
	if _, err := runner.RunRestore(cancelledContext(t), BackupIdentity{ID: "backup-1"}, RestorePlan{Scope: DefaultBackupScope()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunRestore cancelled error = %v", err)
	}
	assertCalls(t, log)

	for _, testCase := range []struct {
		name   string
		mutate func(*Recovery)
	}{
		{name: "nil restore port", mutate: func(candidate *Recovery) { candidate.Restore = nil }},
		{name: "nil fence port", mutate: func(candidate *Recovery) { candidate.Fence = nil }},
		{name: "nil dispatch gate", mutate: func(candidate *Recovery) { candidate.Dispatch = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := runner
			testCase.mutate(&candidate)
			_, err := candidate.RunRestore(context.Background(), BackupIdentity{ID: "backup-1"}, RestorePlan{Scope: DefaultBackupScope()})
			assertFaultClass(t, err, fault.InternalFailure)
		})
	}
}

func TestRunRestoreFailureBlocksLaterSteps(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	for _, testCase := range []struct {
		name string
		// configure installs the failure; wantCalls lists the calls that may
		// happen before the sequence stops.
		configure func(*fakeRestore, *fakeFence, *fakeDispatch, error)
		wantCalls []string
	}{
		{
			name: "fence failure",
			configure: func(_ *fakeRestore, fence *fakeFence, _ *fakeDispatch, err error) {
				fence.err = fault.Wrap(fault.DependencyUnavailable, "fence unavailable", err)
			},
			wantCalls: []string{"preflight", "fence"},
		},
		{
			name: "restore failure",
			configure: func(restore *fakeRestore, _ *fakeFence, _ *fakeDispatch, err error) {
				restore.restoreErr = fault.Wrap(fault.DependencyUnavailable, "restore unavailable", err)
			},
			wantCalls: []string{"preflight", "fence", "restore"},
		},
		{
			name: "reconcile failure",
			configure: func(restore *fakeRestore, _ *fakeFence, _ *fakeDispatch, err error) {
				restore.reconcileErr = fault.Wrap(fault.DependencyUnavailable, "reconcile unavailable", err)
			},
			wantCalls: []string{"preflight", "fence", "restore", "reconcile"},
		},
		{
			name: "dispatch failure",
			configure: func(_ *fakeRestore, _ *fakeFence, dispatch *fakeDispatch, err error) {
				dispatch.err = fault.Wrap(fault.DependencyUnavailable, "dispatch unavailable", err)
			},
			wantCalls: []string{"preflight", "fence", "restore", "reconcile", "dispatch"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sentinel := errors.New(testCase.name)
			log, backup, restore, fence, dispatch := newFakes()
			restore.evidence = evidence(identity)
			restore.reconcileResult = ReconcileResult{Reconciled: true}
			testCase.configure(restore, fence, dispatch, sentinel)
			runner := recovery(log, backup, restore, fence, dispatch)

			result, err := runner.RunRestore(context.Background(), identity, RestorePlan{Scope: DefaultBackupScope()})
			if !errors.Is(err, sentinel) {
				t.Fatalf("RunRestore error = %v, want the port error", err)
			}
			assertCalls(t, log, testCase.wantCalls...)
			if testCase.name == "dispatch failure" {
				if result.Fenced || result.Reconciled || result.Detail != "" ||
					len(result.Services) != 0 || len(result.UnrecoverableWorkspaces) != 0 ||
					result.Preserved != (PreservedState{}) {
					t.Errorf("dispatch failure returned partial result %+v", result)
				}
				if dispatch.calls != 1 {
					t.Errorf("dispatch attempts = %d, want 1", dispatch.calls)
				}
			}
			if len(restore.plans) > 1 {
				t.Errorf("restore called %d times, want at most 1", len(restore.plans))
			}
		})
	}
}

func TestRunRestoreUncertainRestoreNeverReplays(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	log, backup, restore, fence, dispatch := newFakes()
	restore.evidence = evidence(identity)
	restore.restoreErr = fault.New(fault.UncertainExternalEffect, "restore outcome unknown after lost response")
	runner := recovery(log, backup, restore, fence, dispatch)

	_, err := runner.RunRestore(context.Background(), identity, RestorePlan{Scope: DefaultBackupScope()})
	assertFaultClass(t, err, fault.UncertainExternalEffect)
	assertCalls(t, log, "preflight", "fence", "restore")
	if len(restore.plans) != 1 {
		t.Fatalf("restore called %d times, want exactly 1", len(restore.plans))
	}
	if dispatch.calls != 0 {
		t.Fatalf("dispatch attempted %d times after an uncertain restore", dispatch.calls)
	}
}

func TestRunRestoreUnresolvedEffectsBlockDispatch(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	for _, testCase := range []struct {
		name      string
		reconcile ReconcileResult
	}{
		{name: "not reconciled", reconcile: ReconcileResult{Unresolved: []string{"job-1"}}},
		{name: "unresolved effects", reconcile: ReconcileResult{Reconciled: true, Unresolved: []string{"job-2", "registry-push-1"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			log, backup, restore, fence, dispatch := newFakes()
			restore.evidence = evidence(identity)
			restore.reconcileResult = testCase.reconcile
			runner := recovery(log, backup, restore, fence, dispatch)

			_, err := runner.RunRestore(context.Background(), identity, RestorePlan{Scope: DefaultBackupScope()})
			assertFaultClass(t, err, fault.UncertainExternalEffect)
			assertCalls(t, log, "preflight", "fence", "restore", "reconcile")
			if dispatch.calls != 0 {
				t.Fatalf("dispatch attempted %d times with unresolved effects", dispatch.calls)
			}
		})
	}
}

func TestRunRestoreCancellationStopsBeforeNextStep(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	for _, testCase := range []struct {
		name      string
		configure func(context.CancelFunc, *fakeRestore, *fakeFence)
		wantCalls []string
	}{
		{
			name: "after fence",
			configure: func(cancel context.CancelFunc, _ *fakeRestore, fence *fakeFence) {
				fence.hook = func(context.Context) { cancel() }
			},
			wantCalls: []string{"preflight", "fence"},
		},
		{
			name: "after restore",
			configure: func(cancel context.CancelFunc, restore *fakeRestore, _ *fakeFence) {
				restore.restore = func(context.Context) { cancel() }
			},
			wantCalls: []string{"preflight", "fence", "restore"},
		},
		{
			name: "after reconcile",
			configure: func(cancel context.CancelFunc, restore *fakeRestore, _ *fakeFence) {
				restore.reconcile = func(context.Context) { cancel() }
			},
			wantCalls: []string{"preflight", "fence", "restore", "reconcile"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			log, backup, restore, fence, dispatch := newFakes()
			restore.evidence = evidence(identity)
			restore.reconcileResult = ReconcileResult{Reconciled: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			testCase.configure(cancel, restore, fence)
			runner := recovery(log, backup, restore, fence, dispatch)

			_, err := runner.RunRestore(ctx, identity, RestorePlan{Scope: DefaultBackupScope()})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("RunRestore error = %v, want context.Canceled", err)
			}
			assertCalls(t, log, testCase.wantCalls...)
			if dispatch.calls != 0 {
				t.Fatalf("dispatch attempted %d times after cancellation", dispatch.calls)
			}
			if len(restore.plans) > 1 {
				t.Fatalf("restore called %d times, want at most 1", len(restore.plans))
			}
		})
	}
}

func TestRunRestoreMissingWorkspacesAreVisibleNotRecreated(t *testing.T) {
	identity := BackupIdentity{ID: "backup-1"}
	log, backup, restore, fence, dispatch := newFakes()
	restore.evidence = evidence(identity)
	restore.evidence.MissingWorkspaces = []string{"workspace-1", "workspace-2"}
	restore.reconcileResult = ReconcileResult{Reconciled: true}
	runner := recovery(log, backup, restore, fence, dispatch)

	result, err := runner.RunRestore(context.Background(), identity, RestorePlan{Scope: DefaultBackupScope()})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	assertCalls(t, log, "preflight", "fence", "restore", "reconcile", "dispatch")
	if len(result.UnrecoverableWorkspaces) != 2 {
		t.Fatalf("unrecoverable workspaces = %v, want the two missing workspaces", result.UnrecoverableWorkspaces)
	}
	if len(restore.plans) != 1 {
		t.Fatalf("restore called %d times, want exactly 1", len(restore.plans))
	}
	if dispatch.calls != 1 {
		t.Fatalf("dispatch attempts = %d, want 1", dispatch.calls)
	}
}
