package restorequal

// Stage restore destination guard and argv planners.
//
// The live-stage rules in docs/tasks/finish-plan.md allow exactly one
// destructive restore shape: an RDB backup restored into a new database
// explicitly named kodama-restore-<run-id> on the existing instance, never into
// the managed database, and dropping only that exact run-created database
// afterwards.
//
// The planners build argv arrays for a fake runner. They never build a shell
// string and never carry a credential value: a command names the environment
// variables an executor may read, and the executor resolves them.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	// ErrUnsafeDestination reports a restore destination that the live-stage
	// rules do not allow.
	ErrUnsafeDestination = errors.New("unsafe stage restore destination")
	// ErrUnsafeDrop reports a cleanup that could touch a database this run did
	// not create.
	ErrUnsafeDrop = errors.New("unsafe stage database drop")
	// ErrInvalidPlan reports a malformed or misused command plan.
	ErrInvalidPlan = errors.New("invalid stage command plan")
	// ErrUnknownOutcome reports an ambiguous effect. The caller must not retry;
	// it must reconcile the observed state first.
	ErrUnknownOutcome = errors.New("stage command outcome is unknown; do not retry")
	// ErrCommandRefused reports that the runner proved the command had no
	// effect. It is the only failure that is safe to treat as not created.
	ErrCommandRefused = errors.New("stage command refused before any effect")
)

// RestoreDatabasePrefix prefixes every generated drill database name.
const RestoreDatabasePrefix = "kodama-restore-"

// DatabaseCredentialEnvName names the environment variable an executor reads
// for the restored database connection. Only the name is ever planned or
// recorded.
const DatabaseCredentialEnvName = "KODAMA_Q03_RESTORE_DATABASE_URL"

// SCWEnvNames are the environment variable names the Scaleway CLI reads its
// credentials from. The planner records names only; it never carries a value.
var SCWEnvNames = []string{"SCW_ACCESS_KEY", "SCW_SECRET_KEY"}

var (
	restoreDatabasePattern = regexp.MustCompile(`^kodama-restore-[a-z0-9]{6,32}$`)
	runIDPattern           = regexp.MustCompile(`^[a-z0-9]{6,32}$`)
	backupIDPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	instanceIDPattern      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// NewRunID returns one generated drill run ID. It is lowercase hexadecimal, so
// it satisfies the restore database name grammar.
func NewRunID() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate drill run id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// Destination is the new database a restore would create.
type Destination struct {
	DatabaseName string
	InstanceID   string
}

// DestinationRequest is the observed stage state a destination guard checks.
type DestinationRequest struct {
	Destination       Destination
	SourceDatabase    string
	ManagedDatabase   string
	SourceInstanceID  string
	ExistingDatabases []string
}

// CheckStageDestination enforces the live-stage restore destination rules:
//
//   - database-name must be present; the Scaleway CLI otherwise defaults to the
//     source database;
//   - it must match ^kodama-restore-[a-z0-9]{6,32}$ (a generated run ID);
//   - it must differ from the source and managed database names;
//   - the observed database listing must be supplied (nil is not an observation);
//   - it must target the same UUID instance as the source.
func CheckStageDestination(request DestinationRequest) error {
	if request.SourceDatabase == "" {
		return fmt.Errorf("%w: source database is required", ErrUnsafeDestination)
	}
	if request.ManagedDatabase == "" {
		return fmt.Errorf("%w: managed database is required", ErrUnsafeDestination)
	}
	if !instanceIDPattern.MatchString(request.SourceInstanceID) {
		return fmt.Errorf("%w: source instance %q is not a UUID", ErrUnsafeDestination, request.SourceInstanceID)
	}
	if !instanceIDPattern.MatchString(request.Destination.InstanceID) {
		return fmt.Errorf("%w: destination instance %q is not a UUID", ErrUnsafeDestination, request.Destination.InstanceID)
	}
	name := request.Destination.DatabaseName
	if name == "" {
		return fmt.Errorf("%w: database-name must not be empty (the CLI would default to the source database)", ErrUnsafeDestination)
	}
	if !restoreDatabasePattern.MatchString(name) {
		return fmt.Errorf("%w: database name %q does not match %s<run-id>", ErrUnsafeDestination, name, RestoreDatabasePrefix)
	}
	if name == request.SourceDatabase {
		return fmt.Errorf("%w: database name %q is the source database", ErrUnsafeDestination, name)
	}
	if name == request.ManagedDatabase {
		return fmt.Errorf("%w: database name %q is the managed database", ErrUnsafeDestination, name)
	}
	if request.Destination.InstanceID != request.SourceInstanceID {
		return fmt.Errorf("%w: destination instance %q is not the source instance %q",
			ErrUnsafeDestination, request.Destination.InstanceID, request.SourceInstanceID)
	}
	if request.ExistingDatabases == nil {
		return fmt.Errorf("%w: the existing database listing must be observed", ErrUnsafeDestination)
	}
	if slices.Contains(request.ExistingDatabases, name) {
		return fmt.Errorf("%w: database %q already exists on the instance", ErrUnsafeDestination, name)
	}
	return nil
}

// CommandKind is the fixed vocabulary of planned stage commands.
type CommandKind string

const (
	// CommandRestore is one RDB backup restore.
	CommandRestore CommandKind = "scw.rdb.backup.restore"
	// CommandDrop is one database delete.
	CommandDrop CommandKind = "scw.rdb.database.delete"
)

// Command is one planned process execution. Argv is always an array and is
// never joined into a shell string. EnvNames lists the environment variables
// the child may read; their values are resolved by the executor and never
// appear here.
type Command struct {
	Kind     CommandKind
	Name     string
	Argv     []string
	EnvNames []string
}

// StageRestoreRequest is one planned RDB restore.
type StageRestoreRequest struct {
	BackupID    string
	Destination DestinationRequest
}

// PlanStageRestore builds the exact `scw rdb backup restore` argv for a fake or
// real runner. It refuses an unsafe destination before any command exists.
func PlanStageRestore(request StageRestoreRequest) (Command, error) {
	if !backupIDPattern.MatchString(request.BackupID) {
		return Command{}, fmt.Errorf("%w: backup ID %q is malformed", ErrInvalidPlan, request.BackupID)
	}
	if err := CheckStageDestination(request.Destination); err != nil {
		return Command{}, err
	}
	return Command{
		Kind: CommandRestore,
		Name: "scw",
		Argv: []string{
			"rdb",
			"backup",
			"restore",
			request.BackupID,
			"instance-id=" + request.Destination.Destination.InstanceID,
			"database-name=" + request.Destination.Destination.DatabaseName,
		},
		EnvNames: append([]string(nil), SCWEnvNames...),
	}, nil
}

// CreatedDatabase is the database this drill run created. A drop is possible
// only from such a record, never from a discovered name. The installed CLI
// identifies an RDB database by name, so the validated name is the only
// identity that reaches argv.
type CreatedDatabase struct {
	Name       string
	InstanceID string
	RunID      string
}

// StageDropRequest is one planned cleanup.
type StageDropRequest struct {
	Created          CreatedDatabase
	SourceDatabase   string
	ManagedDatabase  string
	SourceInstanceID string
}

// PlanStageDrop builds the exact `scw rdb database delete` argv for the database
// this run created. Every other database is refused.
func PlanStageDrop(request StageDropRequest) (Command, error) {
	if !runIDPattern.MatchString(request.Created.RunID) {
		return Command{}, fmt.Errorf("%w: run ID %q is malformed", ErrUnsafeDrop, request.Created.RunID)
	}
	wantName := RestoreDatabasePrefix + request.Created.RunID
	if !restoreDatabasePattern.MatchString(request.Created.Name) || request.Created.Name != wantName {
		return Command{}, fmt.Errorf("%w: created database %q does not match this run %q",
			ErrUnsafeDrop, request.Created.Name, wantName)
	}
	if request.SourceDatabase == "" || request.ManagedDatabase == "" || request.SourceInstanceID == "" {
		return Command{}, fmt.Errorf("%w: source database, managed database and instance are required", ErrUnsafeDrop)
	}
	if request.Created.Name == request.SourceDatabase {
		return Command{}, fmt.Errorf("%w: refusing to drop the source database %q", ErrUnsafeDrop, request.Created.Name)
	}
	if request.Created.Name == request.ManagedDatabase {
		return Command{}, fmt.Errorf("%w: refusing to drop the managed database %q", ErrUnsafeDrop, request.Created.Name)
	}
	if request.Created.InstanceID == "" || request.Created.InstanceID != request.SourceInstanceID {
		return Command{}, fmt.Errorf("%w: created database instance %q is not the source instance %q",
			ErrUnsafeDrop, request.Created.InstanceID, request.SourceInstanceID)
	}
	if !instanceIDPattern.MatchString(request.Created.InstanceID) {
		return Command{}, fmt.Errorf("%w: created database instance %q is not a UUID", ErrUnsafeDrop, request.Created.InstanceID)
	}
	return Command{
		Kind: CommandDrop,
		Name: "scw",
		Argv: []string{
			"rdb",
			"database",
			"delete",
			"name=" + request.Created.Name,
			"instance-id=" + request.Created.InstanceID,
		},
		EnvNames: append([]string(nil), SCWEnvNames...),
	}, nil
}

// CommandResult is one executed command outcome. Stderr is kept out of reported
// errors because a runner may include an environment value in it.
type CommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// CommandRunner executes one planned command. A runner signals a proven
// no-effect refusal with ErrCommandRefused; every other failure is ambiguous.
type CommandRunner interface {
	Run(ctx context.Context, command Command) (CommandResult, error)
}

// Outcome is the observed effect of one stage command or post-command check.
type Outcome string

const (
	// OutcomeAccepted reports that the CLI command exited 0; post-command
	// readiness and SQL/list verification are still required before recording
	// OutcomeConfirmed.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeConfirmed is a post-command verified restore or drop.
	OutcomeConfirmed Outcome = "confirmed"
	// OutcomeRefused is a proven no-effect refusal.
	OutcomeRefused Outcome = "refused"
	// OutcomeUnknown is an ambiguous effect; reconcile before any retry.
	OutcomeUnknown Outcome = "unknown"
)

// ExecuteStageRestore runs one planned restore command.
func ExecuteStageRestore(ctx context.Context, runner CommandRunner, command Command) (Outcome, error) {
	if err := validatePlannedCommand(command, CommandRestore); err != nil {
		return "", err
	}
	return runStageCommand(ctx, runner, command)
}

// ExecuteStageDrop runs one planned cleanup command.
func ExecuteStageDrop(ctx context.Context, runner CommandRunner, command Command) (Outcome, error) {
	if err := validatePlannedCommand(command, CommandDrop); err != nil {
		return "", err
	}
	return runStageCommand(ctx, runner, command)
}

// validatePlannedCommand re-validates a hand-built command before execution.
// Kind alone is not proof that the argv is the planned shape: an executor must
// never run an arbitrary program or a positional value the planner refused.
func validatePlannedCommand(command Command, kind CommandKind) error {
	if command.Kind != kind {
		return fmt.Errorf("%w: expected kind %q, got %q", ErrInvalidPlan, kind, command.Kind)
	}
	if command.Name != "scw" {
		return fmt.Errorf("%w: expected the scw executable, got %q", ErrInvalidPlan, command.Name)
	}
	if !sameEnvNames(command.EnvNames, SCWEnvNames) {
		return fmt.Errorf("%w: environment names %v are not the planned credential names", ErrInvalidPlan, command.EnvNames)
	}
	switch kind {
	case CommandRestore:
		if len(command.Argv) != 6 || command.Argv[0] != "rdb" || command.Argv[1] != "backup" || command.Argv[2] != "restore" {
			return fmt.Errorf("%w: malformed restore argv %v", ErrInvalidPlan, command.Argv)
		}
		if !backupIDPattern.MatchString(command.Argv[3]) {
			return fmt.Errorf("%w: malformed backup ID %q", ErrInvalidPlan, command.Argv[3])
		}
		if !validFlagValue(command.Argv[4], "instance-id=", instanceIDPattern) {
			return fmt.Errorf("%w: malformed instance-id flag %q", ErrInvalidPlan, command.Argv[4])
		}
		if !validFlagValue(command.Argv[5], "database-name=", restoreDatabasePattern) {
			return fmt.Errorf("%w: malformed database-name flag %q", ErrInvalidPlan, command.Argv[5])
		}
	case CommandDrop:
		if len(command.Argv) != 5 || command.Argv[0] != "rdb" || command.Argv[1] != "database" || command.Argv[2] != "delete" {
			return fmt.Errorf("%w: malformed drop argv %v", ErrInvalidPlan, command.Argv)
		}
		if !validFlagValue(command.Argv[3], "name=", restoreDatabasePattern) {
			return fmt.Errorf("%w: malformed name flag %q", ErrInvalidPlan, command.Argv[3])
		}
		if !validFlagValue(command.Argv[4], "instance-id=", instanceIDPattern) {
			return fmt.Errorf("%w: malformed instance-id flag %q", ErrInvalidPlan, command.Argv[4])
		}
	}
	return nil
}

// sameEnvNames reports whether both lists name the same environment variables
// in the same order.
func sameEnvNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// validFlagValue reports whether value is exactly prefix followed by a value
// that matches pattern.
func validFlagValue(value, prefix string, pattern *regexp.Regexp) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	return pattern.MatchString(strings.TrimPrefix(value, prefix))
}

func runStageCommand(ctx context.Context, runner CommandRunner, command Command) (Outcome, error) {
	if runner == nil {
		return "", fmt.Errorf("%w: runner is required", ErrInvalidPlan)
	}
	result, err := runner.Run(ctx, command)
	switch {
	case errors.Is(err, ErrCommandRefused):
		return OutcomeRefused, nil
	case err == nil && result.ExitCode == 0:
		return OutcomeAccepted, nil
	default:
		// The runner's own error and stderr are deliberately not propagated:
		// they may echo an environment value. The exit code is enough to
		// reconcile the observed state.
		return OutcomeUnknown, fmt.Errorf("%w: exit code %d", ErrUnknownOutcome, result.ExitCode)
	}
}

// StageInstanceStatus is the non-secret instance observation parsed from
// `scw rdb instance get <instance-id> --output json`.
type StageInstanceStatus struct {
	Status          string
	Ready           bool
	BackupRetention time.Duration
	BackupDisabled  bool
}

// ParseStageInstanceJSON decodes the Scaleway RDB instance JSON document and
// extracts readiness and automated backup retention.
func ParseStageInstanceJSON(data []byte) (StageInstanceStatus, error) {
	var wire struct {
		Status         string `json:"status"`
		BackupSchedule *struct {
			Retention int  `json:"retention"`
			Disabled  bool `json:"disabled"`
		} `json:"backup_schedule"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return StageInstanceStatus{}, errors.New("decode stage RDB instance JSON")
	}
	if wire.Status == "" {
		return StageInstanceStatus{}, errors.New("stage RDB instance JSON has no status")
	}
	if wire.BackupSchedule == nil {
		return StageInstanceStatus{}, errors.New("stage RDB instance JSON has no backup_schedule")
	}
	if wire.BackupSchedule.Retention < 0 {
		return StageInstanceStatus{}, errors.New("stage RDB instance backup retention is negative")
	}
	retention := time.Duration(wire.BackupSchedule.Retention) * 24 * time.Hour
	if wire.BackupSchedule.Disabled {
		retention = 0
	}
	return StageInstanceStatus{
		Status:          wire.Status,
		Ready:           wire.Status == "ready",
		BackupRetention: retention,
		BackupDisabled:  wire.BackupSchedule.Disabled,
	}, nil
}

// StageVerifier performs the post-command readiness, listing and SQL checks
// required to promote an accepted stage command to OutcomeConfirmed.
type StageVerifier interface {
	ObserveInstance(ctx context.Context, instanceID string) (StageInstanceStatus, error)
	ListDatabases(ctx context.Context, instanceID string) ([]string, error)
	VerifySQL(ctx context.Context, databaseName string) error
}

// StageRestoreVerification is the post-command verification result of one
// stage restore.
type StageRestoreVerification struct {
	Outcome                 Outcome
	PostgreSQLRetention     time.Duration
	MeetsPostgreSQLRecovery bool
}

// VerifyStageRestore promotes an accepted restore to OutcomeConfirmed only
// after the RDB instance is ready, the destination database appears in the
// instance listing, and SQL verification against the restored database
// succeeds. It also evaluates the instance's backup retention against
// objective.
func VerifyStageRestore(ctx context.Context, verifier StageVerifier, destination Destination, objective time.Duration) (StageRestoreVerification, error) {
	if verifier == nil {
		return StageRestoreVerification{}, fmt.Errorf("%w: verifier is required", ErrInvalidPlan)
	}
	if !instanceIDPattern.MatchString(destination.InstanceID) {
		return StageRestoreVerification{}, fmt.Errorf("%w: destination instance %q is not a UUID", ErrUnsafeDestination, destination.InstanceID)
	}
	if !restoreDatabasePattern.MatchString(destination.DatabaseName) {
		return StageRestoreVerification{}, fmt.Errorf("%w: database name %q does not match %s<run-id>", ErrUnsafeDestination, destination.DatabaseName, RestoreDatabasePrefix)
	}
	if objective <= 0 {
		return StageRestoreVerification{}, fmt.Errorf("%w: PostgreSQL recovery objective must be positive", ErrInvalidTimeline)
	}
	instance, err := verifier.ObserveInstance(ctx, destination.InstanceID)
	if err != nil {
		return StageRestoreVerification{Outcome: OutcomeAccepted}, err
	}
	result := StageRestoreVerification{
		Outcome:                 OutcomeAccepted,
		PostgreSQLRetention:     instance.BackupRetention,
		MeetsPostgreSQLRecovery: !instance.BackupDisabled && instance.BackupRetention >= objective,
	}
	if !instance.Ready {
		return result, fmt.Errorf("stage RDB instance is %q; want ready", instance.Status)
	}
	databases, err := verifier.ListDatabases(ctx, destination.InstanceID)
	if err != nil {
		return result, err
	}
	if databases == nil {
		return result, errors.New("stage RDB database listing was not observed")
	}
	if !slices.Contains(databases, destination.DatabaseName) {
		return result, fmt.Errorf("restored database %q is not present in the instance listing", destination.DatabaseName)
	}
	if err := verifier.VerifySQL(ctx, destination.DatabaseName); err != nil {
		return result, err
	}
	result.Outcome = OutcomeConfirmed
	return result, nil
}

// VerifyStageDrop promotes an accepted drop to OutcomeConfirmed only after
// the RDB instance is ready and the run-created database is absent from the
// observed database listing.
func VerifyStageDrop(ctx context.Context, verifier StageVerifier, created CreatedDatabase) (Outcome, error) {
	if verifier == nil {
		return "", fmt.Errorf("%w: verifier is required", ErrInvalidPlan)
	}
	if !runIDPattern.MatchString(created.RunID) || created.Name != RestoreDatabasePrefix+created.RunID || !restoreDatabasePattern.MatchString(created.Name) {
		return "", fmt.Errorf("%w: created database %q does not match run %q", ErrUnsafeDrop, created.Name, created.RunID)
	}
	if !instanceIDPattern.MatchString(created.InstanceID) {
		return "", fmt.Errorf("%w: created database instance %q is not a UUID", ErrUnsafeDrop, created.InstanceID)
	}
	instance, err := verifier.ObserveInstance(ctx, created.InstanceID)
	if err != nil {
		return OutcomeAccepted, err
	}
	if !instance.Ready {
		return OutcomeAccepted, fmt.Errorf("stage RDB instance is %q; want ready", instance.Status)
	}
	databases, err := verifier.ListDatabases(ctx, created.InstanceID)
	if err != nil {
		return OutcomeAccepted, err
	}
	if databases == nil {
		return OutcomeAccepted, errors.New("stage RDB database listing was not observed")
	}
	if slices.Contains(databases, created.Name) {
		return OutcomeAccepted, fmt.Errorf("dropped database %q is still present in the instance listing", created.Name)
	}
	return OutcomeConfirmed, nil
}
