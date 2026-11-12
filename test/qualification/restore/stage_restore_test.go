//go:build qualification

package restorequal

// Q03 stage RDB restore drill. The live Scaleway restore runs only under the
// exclusive stage RDB lock (scripts/q03-stage-drill). This hermetic test
// exercises the exact restore/drop argv, command acceptance versus verified
// confirmation, and the ambiguous-cleanup policy without contacting Scaleway.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestStageRDBRestore proves the exact restore/drop argv, the distinction
// between command acceptance and post-command confirmation, the 30-day
// PostgreSQL retention check and the ambiguous-cleanup policy without
// contacting Scaleway.
func TestStageRDBRestore(t *testing.T) {
	runID, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	destination := validDestinationRequest()
	destination.Destination.DatabaseName = "kodama-restore-" + runID
	destination.ExistingDatabases = []string{"kodama-managed", "kodama-source", "postgres"}

	// The empty database-name default is the dangerous case: the CLI would
	// restore into the source database.
	empty := destination
	empty.Destination.DatabaseName = ""
	if _, err := PlanStageRestore(StageRestoreRequest{BackupID: "backup-stage", Destination: empty}); !errors.Is(err, ErrUnsafeDestination) {
		t.Fatalf("empty database-name plan error = %v; want ErrUnsafeDestination", err)
	}

	restore, err := PlanStageRestore(StageRestoreRequest{BackupID: "backup-stage", Destination: destination})
	if err != nil {
		t.Fatalf("PlanStageRestore: %v", err)
	}
	if restore.Kind != CommandRestore || restore.Name != "scw" {
		t.Fatalf("restore command = %+v", restore)
	}
	for _, value := range restore.Argv {
		if value == "database-name=" || value == "instance-id=" {
			t.Fatalf("restore argv has an empty flag: %v", restore.Argv)
		}
	}
	if !reflect.DeepEqual(restore.EnvNames, []string{"SCW_ACCESS_KEY", "SCW_SECRET_KEY"}) {
		t.Fatalf("restore env names = %v; want credential names only", restore.EnvNames)
	}

	// The live drill's execution boundary: an ambiguous restore outcome is
	// unknown and is never retried.
	t.Setenv("SCW_ACCESS_KEY", "STAGE-ACCESS-SENTINEL")
	t.Setenv("SCW_SECRET_KEY", "STAGE-SECRET-SENTINEL")
	runner := &fakeRunner{result: CommandResult{ExitCode: -1}, err: context.DeadlineExceeded}
	outcome, err := ExecuteStageRestore(context.Background(), runner, restore)
	if !errors.Is(err, ErrUnknownOutcome) || outcome != OutcomeUnknown {
		t.Fatalf("ambiguous stage restore = %v, %v; want unknown, ErrUnknownOutcome", outcome, err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("ambiguous stage restore calls = %d; want exactly 1", len(runner.calls))
	}

	// A zero exit from the CLI is only command acceptance; OutcomeConfirmed
	// requires readiness, database listing and SQL verification.
	acceptedRunner := &fakeRunner{result: CommandResult{ExitCode: 0}}
	accepted, err := ExecuteStageRestore(context.Background(), acceptedRunner, restore)
	if err != nil || accepted != OutcomeAccepted {
		t.Fatalf("stage restore acceptance = %v, %v; want accepted, nil", accepted, err)
	}
	verifier := &fakeStageVerifier{
		instance:  StageInstanceStatus{Status: "ready", Ready: true, BackupRetention: 30 * 24 * time.Hour},
		databases: []string{"kodama-managed", "kodama-source", "postgres", destination.Destination.DatabaseName},
	}
	verified, err := VerifyStageRestore(context.Background(), verifier, destination.Destination, DefaultObjectives().PostgreSQLRecovery)
	if err != nil || verified.Outcome != OutcomeConfirmed || !verified.MeetsPostgreSQLRecovery {
		t.Fatalf("stage restore verification = %+v, %v; want confirmed with 30d met", verified, err)
	}

	created := CreatedDatabase{
		Name:       destination.Destination.DatabaseName,
		InstanceID: destination.Destination.InstanceID,
		RunID:      runID,
	}
	drop, err := PlanStageDrop(StageDropRequest{
		Created:          created,
		SourceDatabase:   destination.SourceDatabase,
		ManagedDatabase:  destination.ManagedDatabase,
		SourceInstanceID: destination.SourceInstanceID,
	})
	if err != nil {
		t.Fatalf("PlanStageDrop: %v", err)
	}
	if want := []string{"rdb", "database", "delete", "name=" + destination.Destination.DatabaseName, "instance-id=" + testInstanceID}; !reflect.DeepEqual(drop.Argv, want) {
		t.Fatalf("drop argv = %v; want %v", drop.Argv, want)
	}
	dropAccepted, err := ExecuteStageDrop(context.Background(), &fakeRunner{result: CommandResult{ExitCode: 0}}, drop)
	if err != nil || dropAccepted != OutcomeAccepted {
		t.Fatalf("stage drop acceptance = %v, %v; want accepted, nil", dropAccepted, err)
	}
	verifier.databases = []string{"kodama-managed", "kodama-source", "postgres"}
	if dropConfirmed, err := VerifyStageDrop(context.Background(), verifier, created); err != nil || dropConfirmed != OutcomeConfirmed {
		t.Fatalf("stage drop verification = %v, %v; want confirmed, nil", dropConfirmed, err)
	}

	evidence, err := NewTimeline(func() time.Time { return time.Now().UTC() }, DefaultObjectives())
	if err != nil {
		t.Fatalf("NewTimeline: %v", err)
	}
	evidence.RecordPostgreSQLRetention(verified.PostgreSQLRetention)
	started, err := evidence.Mark(MilestoneRestoreStarted)
	if err != nil {
		t.Fatalf("Mark(restore.started): %v", err)
	}
	completed, err := evidence.Mark(MilestoneRestoreCompleted)
	if err != nil {
		t.Fatalf("Mark(restore.completed): %v", err)
	}
	if !completed.At.After(started.At) && !completed.At.Equal(started.At) {
		t.Fatalf("restore completed %v before it started %v", completed.At, started.At)
	}
	t.Logf("stage RDB plan: restore %v", restore.Argv)
	t.Logf("stage RDB plan: drop %v (live execution coordinator-owned)", drop.Argv)
}
