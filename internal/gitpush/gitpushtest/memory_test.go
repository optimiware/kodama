package gitpushtest_test

import (
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/gitpush/gitpushtest"
)

// The acceptance scenarios of T-130 (GR-07, GR-09-GR-11, GR-18, GR-19, GR-A04 to
// GR-A06, GR-A13 to GR-A15) on the memory adapter, which needs no service; the
// PostgreSQL adapter runs the same suite in gitpush/postgres.

func run(t *testing.T, name string) { gitpushtest.Run(t, gitpushtest.OpenMem, name) }

func Test_unit_gitpush_authorize_records_before_update(t *testing.T) {
	run(t, "authorize_records_before_update")
}

func Test_unit_gitpush_default_branch_denied_by_default(t *testing.T) {
	run(t, "default_branch_denied_by_default")
}

func Test_unit_gitpush_seed_consumed_once(t *testing.T) { run(t, "seed_consumed_once") }

func Test_unit_gitpush_seed_single_ref_only(t *testing.T) { run(t, "seed_single_ref_only") }

func Test_unit_gitpush_concurrent_seeds_one_wins(t *testing.T) { run(t, "concurrent_seeds_one_wins") }

func Test_unit_gitpush_unknown_seed_blocks_new_seed(t *testing.T) {
	run(t, "unknown_seed_blocks_new_seed")
}

func Test_unit_gitpush_current_authority(t *testing.T) { run(t, "current_authority") }

func Test_unit_gitpush_commit_intent_refused_after_settlement(t *testing.T) {
	run(t, "commit_intent_refused_after_settlement")
}

func Test_unit_gitpush_commit_intent_fenced_against_cancellation(t *testing.T) {
	run(t, "commit_intent_fenced_against_cancellation")
}

func Test_unit_gitpush_repository_change_fenced_at_authorization(t *testing.T) {
	run(t, "repository_change_fenced_at_authorization")
}

func Test_unit_gitpush_identical_tuples_attributed_by_receive(t *testing.T) {
	run(t, "identical_tuples_attributed_by_receive")
}

func Test_unit_gitpush_per_ref_settlement(t *testing.T) { run(t, "per_ref_settlement") }

func Test_unit_gitpush_reconcile_from_ref_journal(t *testing.T) {
	run(t, "reconcile_from_ref_journal")
}

func Test_unit_gitpush_reconcile_consistent_snapshots(t *testing.T) {
	run(t, "reconcile_consistent_snapshots")
}

func Test_unit_gitpush_reconcile_latches_regression(t *testing.T) {
	run(t, "reconcile_latches_regression")
}

func Test_unit_gitpush_commit_intent_deadline_rechecked_after_locks(t *testing.T) {
	run(t, "commit_intent_deadline_rechecked_after_locks")
}

func Test_unit_gitpush_aba_settles_refused(t *testing.T) { run(t, "aba_settles_refused") }

func Test_unit_gitpush_outcome_emits_one_event(t *testing.T) { run(t, "outcome_emits_one_event") }

func Test_unit_gitpush_unknown_blocks_writer(t *testing.T) { run(t, "unknown_blocks_writer") }

func Test_unit_gitpush_list_and_tenant_isolation(t *testing.T) {
	run(t, "list_and_tenant_isolation")
}

func Test_unit_gitpush_operator_resolves_unknown(t *testing.T) {
	run(t, "operator_resolves_unknown")
}

func Test_unit_gitpush_acknowledge_restore(t *testing.T) { run(t, "acknowledge_restore") }

func Test_unit_gitpush_memory_contract(t *testing.T) {
	gitpushtest.Contract(t, gitpushtest.OpenMem)
}

func Test_unit_gitpush_memory_is_development_only(t *testing.T) {
	e := gitpushtest.NewMemEnv(t)
	if d, ok := any(e.Mem).(interface{ DevelopmentOnly() bool }); !ok || !d.DevelopmentOnly() {
		t.Fatal("the memory store is not flagged development-only")
	}
	if _, err := gitpushtest.NewMemStore(nil, e.Bus, gitpushtest.MemOptions{}); !errors.Is(err, gitpush.ErrNotConfigured) {
		t.Fatalf("a store without a kernel: %v", err)
	}
	if _, err := gitpushtest.NewMemStore(e.DB, nil, gitpushtest.MemOptions{}); !errors.Is(err, gitpush.ErrNotConfigured) {
		t.Fatalf("a store without an event appender: %v", err)
	}
}

func Test_unit_gitpush_unsettled_on_ref(t *testing.T) { run(t, "unsettled_on_ref") }

func Test_unit_gitpush_applied_on_ref(t *testing.T) { run(t, "applied_on_ref") }

func Test_unit_gitpush_authorize_rechecks_writer_after_commit(t *testing.T) {
	run(t, "authorize_rechecks_writer_after_commit")
}
