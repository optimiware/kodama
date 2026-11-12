package restorequal

// Q03 stage-destination guard and command-planner tests. The stage drill may
// restore only into a new database explicitly named kodama-restore-<run-id> on
// the same instance as the managed database, and may drop only that exact
// run-created database (docs/tasks/finish-plan.md).
//
// Credentials are read only from environment variable names; the planner never
// carries a value, so neither argv nor an error can leak one.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeStageVerifier struct {
	instance  StageInstanceStatus
	instErr   error
	databases []string
	listErr   error
	sqlErr    error
}

func (verifier *fakeStageVerifier) ObserveInstance(context.Context, string) (StageInstanceStatus, error) {
	return verifier.instance, verifier.instErr
}

func (verifier *fakeStageVerifier) ListDatabases(context.Context, string) ([]string, error) {
	return verifier.databases, verifier.listErr
}

func (verifier *fakeStageVerifier) VerifySQL(context.Context, string) error {
	return verifier.sqlErr
}

// recordingRunner is the fake command runner. It records every planned command
// and returns one canned outcome.
type fakeRunner struct {
	calls  []Command
	result CommandResult
	err    error
}

func (runner *fakeRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	runner.calls = append(runner.calls, command)
	return runner.result, runner.err
}

// testInstanceID is a syntactically valid Scaleway instance UUID. The CLI
// identifies an instance by this value, not by a display name.
const testInstanceID = "11111111-1111-4111-8111-111111111111"

func validDestinationRequest() DestinationRequest {
	return DestinationRequest{
		Destination: Destination{
			DatabaseName: "kodama-restore-abc123",
			InstanceID:   testInstanceID,
		},
		SourceDatabase:    "kodama-source",
		ManagedDatabase:   "kodama-managed",
		SourceInstanceID:  testInstanceID,
		ExistingDatabases: []string{"kodama-managed", "kodama-source", "postgres"},
	}
}

// TestStageDestinationGuard covers the pure destination guard and the argv
// planners, including the empty database-name refusal that otherwise makes the
// Scaleway CLI default to the source database.
func TestStageDestinationGuard(t *testing.T) {
	t.Run("accepts a generated restore database on the source instance", func(t *testing.T) {
		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID: %v", err)
		}
		request := validDestinationRequest()
		request.Destination.DatabaseName = "kodama-restore-" + runID
		if err := CheckStageDestination(request); err != nil {
			t.Fatalf("CheckStageDestination: %v", err)
		}
	})

	t.Run("refuses an empty database-name", func(t *testing.T) {
		request := validDestinationRequest()
		request.Destination.DatabaseName = ""
		err := CheckStageDestination(request)
		if !errors.Is(err, ErrUnsafeDestination) {
			t.Fatalf("empty database-name error = %v; want ErrUnsafeDestination", err)
		}
		if !strings.Contains(err.Error(), "must not be empty") {
			t.Fatalf("empty database-name error = %q; want it to name the empty database-name hazard", err)
		}
	})

	t.Run("destination name boundaries", func(t *testing.T) {
		for _, testCase := range []struct {
			name string
			ok   bool
		}{
			{name: "kodama-restore-abc123", ok: true},
			{name: "kodama-restore-" + strings.Repeat("a", 32), ok: true},
			{name: "xkodama-restore-abc123", ok: false},
			{name: " kodama-restore-abc123", ok: false},
			{name: "\nkodama-restore-abc123", ok: false},
			{name: "kodama-restore-" + strings.Repeat("a", 5), ok: false},
			{name: "kodama-restore-" + strings.Repeat("a", 33), ok: false},
			{name: "kodama-restore-ABC123", ok: false},
			{name: "kodama-restore-abc_123", ok: false},
			{name: "restore-abc123", ok: false},
			{name: "kodama-restore-", ok: false},
			{name: "kodama-restore-abc123;drop", ok: false},
			{name: "kodama-restore-abc123 --force", ok: false},
		} {
			request := validDestinationRequest()
			request.Destination.DatabaseName = testCase.name
			err := CheckStageDestination(request)
			if testCase.ok && err != nil {
				t.Errorf("%q refused: %v", testCase.name, err)
			}
			if !testCase.ok && !errors.Is(err, ErrUnsafeDestination) {
				t.Errorf("%q error = %v; want ErrUnsafeDestination", testCase.name, err)
			}
		}
	})

	t.Run("refuses collisions and an existing database", func(t *testing.T) {
		for _, testCase := range []struct {
			name   string
			mutate func(*DestinationRequest)
		}{
			{name: "source database", mutate: func(request *DestinationRequest) {
				request.SourceDatabase = request.Destination.DatabaseName
			}},
			{name: "managed database", mutate: func(request *DestinationRequest) {
				request.ManagedDatabase = request.Destination.DatabaseName
			}},
			{name: "source and managed database are the same", mutate: func(request *DestinationRequest) {
				request.ManagedDatabase = request.Destination.DatabaseName
				request.SourceDatabase = request.Destination.DatabaseName
			}},
			{name: "pre-existing database", mutate: func(request *DestinationRequest) {
				request.ExistingDatabases = append(request.ExistingDatabases, request.Destination.DatabaseName)
			}},
			{name: "different instance", mutate: func(request *DestinationRequest) {
				request.Destination.InstanceID = "22222222-2222-4222-8222-222222222222"
			}},
			{name: "empty destination instance", mutate: func(request *DestinationRequest) {
				request.Destination.InstanceID = ""
			}},
			{name: "destination instance with a space", mutate: func(request *DestinationRequest) {
				request.Destination.InstanceID = "11111111 1111-4111-8111-111111111111"
			}},
			{name: "destination instance with an equals sign", mutate: func(request *DestinationRequest) {
				request.Destination.InstanceID = "11111111-1111-4111-8111-111111111111=x"
			}},
			{name: "empty source instance", mutate: func(request *DestinationRequest) {
				request.SourceInstanceID = ""
			}},
			{name: "nil existing database listing", mutate: func(request *DestinationRequest) {
				request.ExistingDatabases = nil
			}},
			{name: "empty source database", mutate: func(request *DestinationRequest) {
				request.SourceDatabase = ""
			}},
			{name: "empty managed database", mutate: func(request *DestinationRequest) {
				request.ManagedDatabase = ""
			}},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				request := validDestinationRequest()
				testCase.mutate(&request)
				if err := CheckStageDestination(request); !errors.Is(err, ErrUnsafeDestination) {
					t.Fatalf("CheckStageDestination error = %v; want ErrUnsafeDestination", err)
				}
			})
		}
	})

	t.Run("refuses malformed instance IDs with a UUID reason", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*DestinationRequest)
			reason []string
		}{
			{name: "destination with a space", mutate: func(request *DestinationRequest) {
				request.Destination.InstanceID = "11111111 1111-4111-8111-111111111111"
			}, reason: []string{"destination instance", "is not a UUID"}},
			{name: "destination with an equals sign", mutate: func(request *DestinationRequest) {
				request.Destination.InstanceID = testInstanceID + "=x"
			}, reason: []string{"destination instance", "is not a UUID"}},
			{name: "empty destination", mutate: func(request *DestinationRequest) {
				request.Destination.InstanceID = ""
			}, reason: []string{"destination instance", "is not a UUID"}},
			{name: "source is not a UUID", mutate: func(request *DestinationRequest) {
				request.SourceInstanceID = "instance-1"
			}, reason: []string{"source instance", "is not a UUID"}},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				request := validDestinationRequest()
				testCase.mutate(&request)
				err := CheckStageDestination(request)
				if !errors.Is(err, ErrUnsafeDestination) {
					t.Fatalf("error = %v; want ErrUnsafeDestination", err)
				}
				for _, reason := range testCase.reason {
					if !strings.Contains(err.Error(), reason) {
						t.Fatalf("error = %q; want it to contain %q", err, reason)
					}
				}
			})
		}
	})

	t.Run("plans the restore argv as an array", func(t *testing.T) {
		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID: %v", err)
		}
		request := validDestinationRequest()
		request.Destination.DatabaseName = "kodama-restore-" + runID
		command, err := PlanStageRestore(StageRestoreRequest{BackupID: "backup-123", Destination: request})
		if err != nil {
			t.Fatalf("PlanStageRestore: %v", err)
		}
		want := Command{
			Kind: CommandRestore,
			Name: "scw",
			Argv: []string{
				"rdb", "backup", "restore", "backup-123",
				"instance-id=" + testInstanceID,
				"database-name=kodama-restore-" + runID,
			},
			EnvNames: []string{"SCW_ACCESS_KEY", "SCW_SECRET_KEY"},
		}
		if !reflect.DeepEqual(command, want) {
			t.Fatalf("restore command = %+v; want %+v", command, want)
		}
	})

	t.Run("restore planner refuses an injected backup ID", func(t *testing.T) {
		request := validDestinationRequest()
		for _, backupID := range []string{
			"",
			"--wait",
			"-backup-123",
			"database-name=kodama-source",
			"a b",
			"a=b",
			"backup/../x",
		} {
			_, err := PlanStageRestore(StageRestoreRequest{BackupID: backupID, Destination: request})
			if !errors.Is(err, ErrInvalidPlan) {
				t.Errorf("backup ID %q error = %v; want ErrInvalidPlan", backupID, err)
			}
		}
		request.Destination.DatabaseName = ""
		if _, err := PlanStageRestore(StageRestoreRequest{BackupID: "backup-123", Destination: request}); !errors.Is(err, ErrUnsafeDestination) {
			t.Fatalf("empty database-name plan error = %v; want ErrUnsafeDestination", err)
		}
	})

	t.Run("credentials exist only as environment variable names", func(t *testing.T) {
		t.Setenv("SCW_ACCESS_KEY", "SCW-ACCESS-KEY-SENTINEL")
		t.Setenv("SCW_SECRET_KEY", "SCW-SECRET-KEY-SENTINEL")
		t.Setenv(DatabaseCredentialEnvName, "postgres://user:DB-PASSWORD-SENTINEL@example/kodama")
		for _, name := range []string{"SCW_ACCESS_KEY", "SCW_SECRET_KEY", DatabaseCredentialEnvName} {
			if got := os.Getenv(name); got == "" {
				t.Fatalf("test environment %s is not set; the credential test would be vacuous", name)
			}
		}

		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID: %v", err)
		}
		request := validDestinationRequest()
		request.Destination.DatabaseName = "kodama-restore-" + runID
		command, err := PlanStageRestore(StageRestoreRequest{BackupID: "backup-123", Destination: request})
		if err != nil {
			t.Fatalf("PlanStageRestore: %v", err)
		}
		if !reflect.DeepEqual(command.EnvNames, []string{"SCW_ACCESS_KEY", "SCW_SECRET_KEY"}) {
			t.Fatalf("env names = %v; want the SCW names only", command.EnvNames)
		}
		for _, value := range command.Argv {
			for _, sentinel := range []string{"SENTINEL", "DB-PASSWORD-SENTINEL", "SCW-ACCESS-KEY-SENTINEL", "SCW-SECRET-KEY-SENTINEL"} {
				if strings.Contains(value, sentinel) {
					t.Fatalf("argv leaked a credential sentinel %q in %q", sentinel, value)
				}
			}
		}

		invalid := request
		invalid.Destination.DatabaseName = ""
		_, err = PlanStageRestore(StageRestoreRequest{BackupID: "backup-123", Destination: invalid})
		if err == nil {
			t.Fatal("unsafe destination was accepted")
		}
		for _, sentinel := range []string{"SENTINEL", "DB-PASSWORD-SENTINEL", "SCW-SECRET-KEY-SENTINEL"} {
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("error leaked a credential sentinel %q: %v", sentinel, err)
			}
		}
	})

	t.Run("drop planner can drop only the exact run-created database", func(t *testing.T) {
		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID: %v", err)
		}
		created := CreatedDatabase{
			Name:       "kodama-restore-" + runID,
			InstanceID: testInstanceID,
			RunID:      runID,
		}
		command, err := PlanStageDrop(StageDropRequest{
			Created:          created,
			SourceDatabase:   "kodama-source",
			ManagedDatabase:  "kodama-managed",
			SourceInstanceID: testInstanceID,
		})
		if err != nil {
			t.Fatalf("PlanStageDrop: %v", err)
		}
		want := Command{
			Kind:     CommandDrop,
			Name:     "scw",
			Argv:     []string{"rdb", "database", "delete", "name=kodama-restore-" + runID, "instance-id=" + testInstanceID},
			EnvNames: []string{"SCW_ACCESS_KEY", "SCW_SECRET_KEY"},
		}
		if !reflect.DeepEqual(command, want) {
			t.Fatalf("drop command = %+v; want %+v", command, want)
		}

		for _, testCase := range []struct {
			name   string
			mutate func(*StageDropRequest)
		}{
			{name: "name does not match the run", mutate: func(request *StageDropRequest) {
				request.Created.Name = "kodama-restore-other1"
			}},
			{name: "run id is too short", mutate: func(request *StageDropRequest) {
				request.Created.RunID = "abc"
				request.Created.Name = "kodama-restore-abc"
			}},
			{name: "empty run id", mutate: func(request *StageDropRequest) {
				request.Created.RunID = ""
			}},
			{name: "flag-shaped run id", mutate: func(request *StageDropRequest) {
				request.Created.RunID = "--force"
				request.Created.Name = "kodama-restore---force"
			}},
			{name: "run id with an equals sign", mutate: func(request *StageDropRequest) {
				request.Created.RunID = "abc=def"
				request.Created.Name = "kodama-restore-abc=def"
			}},
			{name: "run id with whitespace", mutate: func(request *StageDropRequest) {
				request.Created.RunID = "abc def"
				request.Created.Name = "kodama-restore-abc def"
			}},
			{name: "different instance", mutate: func(request *StageDropRequest) {
				request.Created.InstanceID = "22222222-2222-4222-8222-222222222222"
			}},
			{name: "empty instance", mutate: func(request *StageDropRequest) {
				request.Created.InstanceID = ""
			}},
			{name: "instance with an equals sign", mutate: func(request *StageDropRequest) {
				request.Created.InstanceID = testInstanceID + "=x"
			}},
			{name: "created record is the source database", mutate: func(request *StageDropRequest) {
				request.SourceDatabase = request.Created.Name
			}},
			{name: "created record is the managed database", mutate: func(request *StageDropRequest) {
				request.ManagedDatabase = request.Created.Name
			}},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				request := StageDropRequest{
					Created:          created,
					SourceDatabase:   "kodama-source",
					ManagedDatabase:  "kodama-managed",
					SourceInstanceID: testInstanceID,
				}
				testCase.mutate(&request)
				if _, err := PlanStageDrop(request); !errors.Is(err, ErrUnsafeDrop) {
					t.Fatalf("PlanStageDrop error = %v; want ErrUnsafeDrop", err)
				}
			})
		}
	})

	t.Run("execution distinguishes accepted refused and ambiguous outcomes", func(t *testing.T) {
		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID: %v", err)
		}
		request := validDestinationRequest()
		request.Destination.DatabaseName = "kodama-restore-" + runID
		restore, err := PlanStageRestore(StageRestoreRequest{BackupID: "backup-123", Destination: request})
		if err != nil {
			t.Fatalf("PlanStageRestore: %v", err)
		}

		accepted := &fakeRunner{result: CommandResult{ExitCode: 0}}
		outcome, err := ExecuteStageRestore(context.Background(), accepted, restore)
		if err != nil || outcome != OutcomeAccepted {
			t.Fatalf("accepted outcome = %v, %v; want accepted, nil", outcome, err)
		}
		if len(accepted.calls) != 1 {
			t.Fatalf("accepted runner calls = %d; want 1", len(accepted.calls))
		}

		refused := &fakeRunner{err: ErrCommandRefused}
		outcome, err = ExecuteStageRestore(context.Background(), refused, restore)
		if err != nil || outcome != OutcomeRefused {
			t.Fatalf("refused outcome = %v, %v; want refused, nil", outcome, err)
		}

		ambiguous := &fakeRunner{
			result: CommandResult{ExitCode: 1, Stderr: []byte("SCW-SECRET-KEY-SENTINEL")},
			err:    errors.New("connection dropped after the request was sent: SCW-SECRET-KEY-SENTINEL"),
		}
		outcome, err = ExecuteStageRestore(context.Background(), ambiguous, restore)
		if !errors.Is(err, ErrUnknownOutcome) {
			t.Fatalf("ambiguous error = %v; want ErrUnknownOutcome", err)
		}
		if outcome != OutcomeUnknown {
			t.Fatalf("ambiguous outcome = %v; want unknown", outcome)
		}
		if strings.Contains(err.Error(), "SENTINEL") {
			t.Fatalf("ambiguous error leaked the runner error: %v", err)
		}
		if len(ambiguous.calls) != 1 {
			t.Fatalf("ambiguous runner calls = %d; want exactly 1 (never retried)", len(ambiguous.calls))
		}

		nonZero := &fakeRunner{result: CommandResult{ExitCode: 2}}
		outcome, err = ExecuteStageRestore(context.Background(), nonZero, restore)
		if !errors.Is(err, ErrUnknownOutcome) || outcome != OutcomeUnknown {
			t.Fatalf("non-zero outcome = %v, %v; want unknown, ErrUnknownOutcome", outcome, err)
		}

		cancelled := &fakeRunner{err: context.Canceled}
		outcome, err = ExecuteStageRestore(context.Background(), cancelled, restore)
		if !errors.Is(err, ErrUnknownOutcome) || outcome != OutcomeUnknown {
			t.Fatalf("cancelled outcome = %v, %v; want unknown, ErrUnknownOutcome", outcome, err)
		}
	})

	t.Run("drop execution reports an ambiguous cleanup without retrying", func(t *testing.T) {
		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID: %v", err)
		}
		drop, err := PlanStageDrop(StageDropRequest{
			Created: CreatedDatabase{
				Name:       "kodama-restore-" + runID,
				InstanceID: testInstanceID,
				RunID:      runID,
			},
			SourceDatabase:   "kodama-source",
			ManagedDatabase:  "kodama-managed",
			SourceInstanceID: testInstanceID,
		})
		if err != nil {
			t.Fatalf("PlanStageDrop: %v", err)
		}

		ambiguous := &fakeRunner{result: CommandResult{ExitCode: -1}, err: context.DeadlineExceeded}
		outcome, err := ExecuteStageDrop(context.Background(), ambiguous, drop)
		if !errors.Is(err, ErrUnknownOutcome) || outcome != OutcomeUnknown {
			t.Fatalf("ambiguous drop outcome = %v, %v; want unknown, ErrUnknownOutcome", outcome, err)
		}
		if len(ambiguous.calls) != 1 {
			t.Fatalf("ambiguous drop calls = %d; want exactly 1", len(ambiguous.calls))
		}
		if want := drop.Argv; !reflect.DeepEqual(ambiguous.calls[0].Argv, want) {
			t.Fatalf("drop argv = %v; want %v", ambiguous.calls[0].Argv, want)
		}
		for _, value := range ambiguous.calls[0].Argv {
			if strings.Contains(value, "kodama-managed") || strings.Contains(value, "kodama-source") {
				t.Fatalf("drop argv targets a forbidden database: %v", ambiguous.calls[0].Argv)
			}
		}

		accepted := &fakeRunner{result: CommandResult{ExitCode: 0}}
		outcome, err = ExecuteStageDrop(context.Background(), accepted, drop)
		if err != nil || outcome != OutcomeAccepted {
			t.Fatalf("accepted drop outcome = %v, %v; want accepted, nil", outcome, err)
		}
	})

	t.Run("confirmed outcome requires post-command readiness listing and SQL verification", func(t *testing.T) {
		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID: %v", err)
		}
		dbName := "kodama-restore-" + runID
		destination := Destination{DatabaseName: dbName, InstanceID: testInstanceID}
		created := CreatedDatabase{Name: dbName, InstanceID: testInstanceID, RunID: runID}

		instanceJSON := []byte(`{"status":"ready","backup_schedule":{"retention":30,"disabled":false}}`)
		parsed, err := ParseStageInstanceJSON(instanceJSON)
		if err != nil || !parsed.Ready || parsed.BackupRetention != 30*24*time.Hour {
			t.Fatalf("ParseStageInstanceJSON = %+v, %v; want ready with 30d retention", parsed, err)
		}

		notReady := &fakeStageVerifier{
			instance:  StageInstanceStatus{Status: "Snapshot", Ready: false, BackupRetention: 7 * 24 * time.Hour},
			databases: []string{"kodama", dbName},
		}
		got, err := VerifyStageRestore(context.Background(), notReady, destination, DefaultObjectives().PostgreSQLRecovery)
		if err == nil || got.Outcome != OutcomeAccepted || got.MeetsPostgreSQLRecovery {
			t.Fatalf("unready restore verification = %+v, %v; want accepted error and missed 30d objective", got, err)
		}

		sqlFailed := &fakeStageVerifier{
			instance:  parsed,
			databases: []string{"kodama", dbName},
			sqlErr:    errors.New("permission denied"),
		}
		got, err = VerifyStageRestore(context.Background(), sqlFailed, destination, DefaultObjectives().PostgreSQLRecovery)
		if err == nil || got.Outcome != OutcomeAccepted || !got.MeetsPostgreSQLRecovery {
			t.Fatalf("SQL-failed restore verification = %+v, %v; want accepted error with 30d met", got, err)
		}

		verified := &fakeStageVerifier{
			instance:  parsed,
			databases: []string{"kodama", dbName},
		}
		got, err = VerifyStageRestore(context.Background(), verified, destination, DefaultObjectives().PostgreSQLRecovery)
		if err != nil || got.Outcome != OutcomeConfirmed || !got.MeetsPostgreSQLRecovery {
			t.Fatalf("verified restore = %+v, %v; want confirmed with 30d met", got, err)
		}

		if dropOutcome, err := VerifyStageDrop(context.Background(), verified, created); err == nil || dropOutcome != OutcomeAccepted {
			t.Fatalf("drop while still listed = %v, %v; want accepted error", dropOutcome, err)
		}
		verified.databases = []string{"kodama"}
		if dropOutcome, err := VerifyStageDrop(context.Background(), verified, created); err != nil || dropOutcome != OutcomeConfirmed {
			t.Fatalf("verified drop = %v, %v; want confirmed, nil", dropOutcome, err)
		}
	})

	t.Run("execution re-validates the command shape", func(t *testing.T) {
		runner := &fakeRunner{result: CommandResult{ExitCode: 0}}
		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID: %v", err)
		}
		validDrop := Command{
			Kind:     CommandDrop,
			Name:     "scw",
			Argv:     []string{"rdb", "database", "delete", "name=kodama-restore-" + runID, "instance-id=" + testInstanceID},
			EnvNames: []string{"SCW_ACCESS_KEY", "SCW_SECRET_KEY"},
		}
		validRestore := Command{
			Kind:     CommandRestore,
			Name:     "scw",
			Argv:     []string{"rdb", "backup", "restore", "backup-123", "instance-id=" + testInstanceID, "database-name=kodama-restore-" + runID},
			EnvNames: []string{"SCW_ACCESS_KEY", "SCW_SECRET_KEY"},
		}

		for _, testCase := range []struct {
			name    string
			execute func(Command) (Outcome, error)
			command Command
		}{
			{name: "restore of a drop command", execute: func(command Command) (Outcome, error) {
				return ExecuteStageRestore(context.Background(), runner, command)
			}, command: validDrop},
			{name: "drop of a restore command", execute: func(command Command) (Outcome, error) {
				return ExecuteStageDrop(context.Background(), runner, command)
			}, command: validRestore},
			{name: "hand-built drop with a shell", execute: func(command Command) (Outcome, error) {
				return ExecuteStageDrop(context.Background(), runner, command)
			}, command: Command{Kind: CommandDrop, Name: "sh", Argv: validDrop.Argv}},
			{name: "hand-built drop with a positional database", execute: func(command Command) (Outcome, error) {
				return ExecuteStageDrop(context.Background(), runner, command)
			}, command: Command{Kind: CommandDrop, Name: "scw", Argv: []string{"rdb", "database", "delete", "kodama-managed", "instance-id=" + testInstanceID}}},
			{name: "hand-built drop with a managed database name flag", execute: func(command Command) (Outcome, error) {
				return ExecuteStageDrop(context.Background(), runner, command)
			}, command: Command{Kind: CommandDrop, Name: "scw", Argv: []string{"rdb", "database", "delete", "name=kodama-managed", "instance-id=" + testInstanceID}}},
			{name: "hand-built restore with an injected backup ID", execute: func(command Command) (Outcome, error) {
				return ExecuteStageRestore(context.Background(), runner, command)
			}, command: Command{Kind: CommandRestore, Name: "scw", Argv: []string{"rdb", "backup", "restore", "--wait", "instance-id=" + testInstanceID, "database-name=kodama-restore-" + runID}}},
			{name: "hand-built restore with a malformed instance", execute: func(command Command) (Outcome, error) {
				return ExecuteStageRestore(context.Background(), runner, command)
			}, command: Command{Kind: CommandRestore, Name: "scw", Argv: []string{"rdb", "backup", "restore", "backup-123", "instance-id=instance-1", "database-name=kodama-restore-" + runID}}},
			{name: "hand-built drop with an unplanned environment name", execute: func(command Command) (Outcome, error) {
				return ExecuteStageDrop(context.Background(), runner, command)
			}, command: Command{Kind: CommandDrop, Name: "scw", Argv: validDrop.Argv, EnvNames: []string{"AWS_SECRET_ACCESS_KEY"}}},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				if _, err := testCase.execute(testCase.command); !errors.Is(err, ErrInvalidPlan) {
					t.Fatalf("error = %v; want ErrInvalidPlan", err)
				}
			})
		}
		if len(runner.calls) != 0 {
			t.Fatalf("runner calls = %d; want 0 for a refused hand-built command", len(runner.calls))
		}
		if _, err := ExecuteStageRestore(context.Background(), nil, validRestore); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("nil runner error = %v; want ErrInvalidPlan", err)
		}
	})
}

func TestNewRunID(t *testing.T) {
	first, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	second, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	if first == second {
		t.Fatalf("two run IDs are equal: %q", first)
	}
	for _, id := range []string{first, second} {
		request := validDestinationRequest()
		request.Destination.DatabaseName = "kodama-restore-" + id
		if err := CheckStageDestination(request); err != nil {
			t.Fatalf("generated run ID %q does not form a valid destination: %v", id, err)
		}
	}
}
