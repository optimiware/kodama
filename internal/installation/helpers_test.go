package installation

// Deterministic fakes for the recovery tests. Every fake records the exact
// call it served, returns only caller-supplied values and never touches a
// database, broker, container runtime, cloud client, clock or filesystem. A
// hook may cancel the context to prove the runner stops at that step.

import (
	"context"
	"sync"
	"testing"
)

// callLog records the ordered calls one fake observed.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (log *callLog) add(name string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.calls = append(log.calls, name)
}

func (log *callLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.calls...)
}

// assertCalls fails unless the observed calls equal want in order.
func assertCalls(t *testing.T, log *callLog, want ...string) {
	t.Helper()
	got := log.snapshot()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}

// fakeBackup is a BackupPort that returns one configured result.
type fakeBackup struct {
	log    *callLog
	result BackupResult
	err    error
	hook   func(context.Context)
}

func (fake *fakeBackup) Backup(ctx context.Context, plan BackupPlan) (BackupResult, error) {
	fake.log.add("backup")
	if fake.hook != nil {
		fake.hook(ctx)
	}
	return fake.result, fake.err
}

// fakeRestore is a RestorePort that records preflight, restore and reconcile
// calls and captures the RestorePlan it received.
type fakeRestore struct {
	log *callLog

	evidence     BackupEvidence
	preflightErr error
	preflight    func(context.Context)

	restoreResult RestoreResult
	restoreErr    error
	restore       func(context.Context)
	plans         []RestorePlan

	reconcileResult ReconcileResult
	reconcileErr    error
	reconcile       func(context.Context)
}

func (fake *fakeRestore) Preflight(ctx context.Context, identity BackupIdentity) (BackupEvidence, error) {
	fake.log.add("preflight")
	if fake.preflight != nil {
		fake.preflight(ctx)
	}
	return fake.evidence, fake.preflightErr
}

func (fake *fakeRestore) Restore(ctx context.Context, plan RestorePlan) (RestoreResult, error) {
	fake.log.add("restore")
	fake.plans = append(fake.plans, plan)
	if fake.restore != nil {
		fake.restore(ctx)
	}
	return fake.restoreResult, fake.restoreErr
}

func (fake *fakeRestore) Reconcile(ctx context.Context) (ReconcileResult, error) {
	fake.log.add("reconcile")
	if fake.reconcile != nil {
		fake.reconcile(ctx)
	}
	return fake.reconcileResult, fake.reconcileErr
}

// fakeFence is a FencePort with one configurable outcome.
type fakeFence struct {
	log  *callLog
	err  error
	hook func(context.Context)
}

func (fake *fakeFence) Fence(ctx context.Context) error {
	fake.log.add("fence")
	if fake.hook != nil {
		fake.hook(ctx)
	}
	return fake.err
}

// fakeDispatch is a DispatchGate that counts enable attempts.
type fakeDispatch struct {
	log   *callLog
	err   error
	hook  func(context.Context)
	calls int
}

func (fake *fakeDispatch) EnableDispatch(ctx context.Context) error {
	fake.log.add("dispatch")
	fake.calls++
	if fake.hook != nil {
		fake.hook(ctx)
	}
	return fake.err
}

// evidence builds preflight evidence for identity with the default scope.
func evidence(identity BackupIdentity) BackupEvidence {
	return BackupEvidence{
		Identity:    identity,
		Scope:       DefaultBackupScope(),
		KeysPresent: true,
		Preserved: PreservedState{
			Receipts:         3,
			Decisions:        2,
			QueuedExecutions: 1,
		},
	}
}

// recovery assembles one Recovery over the fakes.
func recovery(log *callLog, backup *fakeBackup, restore *fakeRestore, fence *fakeFence, dispatch *fakeDispatch) Recovery {
	return Recovery{
		Backup:   backup,
		Restore:  restore,
		Fence:    fence,
		Dispatch: dispatch,
	}
}

// newFakes returns a call log and every fake wired to it.
func newFakes() (*callLog, *fakeBackup, *fakeRestore, *fakeFence, *fakeDispatch) {
	log := &callLog{}
	return log, &fakeBackup{log: log}, &fakeRestore{log: log}, &fakeFence{log: log}, &fakeDispatch{log: log}
}

// cancelledContext returns a context cancelled before the call starts.
func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
