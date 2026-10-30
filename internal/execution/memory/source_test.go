package memory_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// The T-148 acceptance scenarios on the in-memory store: run origins and source
// sets in execution (ADR 0011, PX-23-PX-25). The PostgreSQL store runs the same
// scenarios in the `test` environment (postgres/source_contract_test.go).

func openSource(tb testing.TB) executiontest.SourceSubject {
	_, s := executiontest.NewSourceMem(tb)
	return s
}

func Test_unit_admission_pins_source_set(t *testing.T) {
	executiontest.SourceAdmissionPinsSourceSet(t, openSource)
}

func Test_unit_admission_push_origin_without_work_item(t *testing.T) {
	executiontest.SourceAdmissionPushOrigin(t, openSource)
}

func Test_unit_admission_provisioning_run(t *testing.T) {
	executiontest.SourceAdmissionProvisioning(t, openSource)
}

func Test_unit_admission_manual_origin(t *testing.T) {
	executiontest.SourceAdmissionManualOrigin(t, openSource)
}

func Test_unit_admission_cleanup_purpose_unchanged(t *testing.T) {
	executiontest.SourceCleanupPurposeUnchanged(t, openSource)
}

func Test_unit_context_carries_source_set(t *testing.T) {
	executiontest.SourceContextCarriesSourceSet(t, openSource)
}

func Test_unit_attempt_grant_requested_and_revoked(t *testing.T) {
	executiontest.SourceAttemptGrantRequestedAndRevoked(t, openSource)
}

func Test_unit_old_run_models_unchanged(t *testing.T) {
	executiontest.SourceOldRunModelsUnchanged(t)
}

// Beyond the named acceptance scenarios: one attempt per run and a work item's
// internally hosted writer.

func Test_unit_one_attempt_per_run(t *testing.T) {
	executiontest.SourceOneAttemptPerRun(t, openSource)
}

func Test_unit_work_item_writer_checkpoints(t *testing.T) {
	executiontest.SourceWorkItemWriter(t, openSource)
}

func TestUnitSourceContract(t *testing.T) { executiontest.SourceContract(t, openSource) }

func Test_unit_admission_run_id_race(t *testing.T) {
	executiontest.SourceAdmissionRunIDRace(t, openSource)
}

func Test_unit_redrive_keeps_the_credential(t *testing.T) {
	executiontest.SourceRedriveKeepsCredential(t, openSource)
}

func Test_unit_grant_issuance_race(t *testing.T) {
	executiontest.SourceGrantIssuanceRace(t, openSource)
}
