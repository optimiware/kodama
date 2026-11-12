// Command stagedrill drives the Q03 Scaleway RDB restore drill through the
// checked-in destination guards and argv planners.
//
// It is invoked by scripts/q03-stage-drill and never by the regular test suite.
// Modes:
//
//	plan    - observe the instance and print the exact planned argv; no effect
//	restore - restore one backup into a new kodama-restore-<run-id> database and
//	          confirm instance readiness, backup retention, database listing and
//	          SQL state
//	drop    - drop only the exact database a recorded restore run created and
//	          confirm its removal from the instance listing
//
// Credentials are read from the environment by the scw CLI and the SQL check.
// This program records only environment variable names, never a value, and its
// errors never propagate child stderr or connection strings.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	restorequal "github.com/optimiware/kodama/test/qualification/restore"
)

const (
	stageWaitTimeout  = 5 * time.Minute
	stagePollInterval = 2 * time.Second
	sqlVerifyTimeout  = 20 * time.Second
)

// restoreRecord is the non-secret evidence one restore mode writes. Drop mode
// requires it so a database can only be dropped by the run that created it.
type restoreRecord struct {
	RunID                    string    `json:"run_id"`
	Database                 string    `json:"database"`
	InstanceID               string    `json:"instance_id"`
	BackupID                 string    `json:"backup_id"`
	SourceRevision           string    `json:"source_revision,omitempty"`
	RestoreOutcome           string    `json:"restore_outcome"`
	DropOutcome              string    `json:"drop_outcome,omitempty"`
	PostgreSQLRetentionHours *float64  `json:"postgresql_retention_hours,omitempty"`
	MeetsPostgreSQLRecovery  *bool     `json:"meets_postgresql_recovery,omitempty"`
	At                       time.Time `json:"at"`
}

func main() {
	ctx := context.Background()
	mode := strings.TrimSpace(os.Getenv("KODAMA_Q03_STAGE_MODE"))
	if mode == "" {
		mode = "plan"
	}
	if err := run(ctx, mode); err != nil {
		// Never print a child stderr: it may echo an environment value.
		fmt.Fprintf(os.Stderr, "q03 stage drill: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, mode string) error {
	instanceID := os.Getenv("KODAMA_Q03_STAGE_INSTANCE_ID")
	if strings.TrimSpace(instanceID) == "" {
		return errors.New("KODAMA_Q03_STAGE_INSTANCE_ID is required")
	}
	sourceDatabase := os.Getenv("KODAMA_Q03_STAGE_SOURCE_DATABASE")
	managedDatabase := os.Getenv("KODAMA_Q03_STAGE_MANAGED_DATABASE")
	evidenceDir := os.Getenv("KODAMA_Q03_EVIDENCE_DIR")
	if mode != "plan" && evidenceDir == "" {
		return errors.New("KODAMA_Q03_EVIDENCE_DIR is required for restore and drop")
	}

	switch mode {
	case "plan", "restore", "drop":
	default:
		return fmt.Errorf("unknown mode %q (want plan, restore or drop)", mode)
	}

	existing, err := listDatabases(ctx, instanceID)
	if err != nil {
		return err
	}

	runID := strings.TrimSpace(os.Getenv("KODAMA_Q03_STAGE_RUN_ID"))
	if runID == "" {
		if mode == "drop" {
			return errors.New("KODAMA_Q03_STAGE_RUN_ID is required for drop")
		}
		runID, err = restorequal.NewRunID()
		if err != nil {
			return err
		}
	}
	if mode == "drop" {
		return dropRecordedRun(ctx, instanceID, sourceDatabase, managedDatabase, runID, existing, evidenceDir)
	}

	backupID := os.Getenv("KODAMA_Q03_STAGE_BACKUP_ID")
	if backupID == "" {
		return errors.New("KODAMA_Q03_STAGE_BACKUP_ID is required")
	}
	destination := restorequal.DestinationRequest{
		Destination: restorequal.Destination{
			DatabaseName: restorequal.RestoreDatabasePrefix + runID,
			InstanceID:   instanceID,
		},
		SourceDatabase:    sourceDatabase,
		ManagedDatabase:   managedDatabase,
		SourceInstanceID:  instanceID,
		ExistingDatabases: existing,
	}
	command, err := restorequal.PlanStageRestore(restorequal.StageRestoreRequest{
		BackupID:    backupID,
		Destination: destination,
	})
	if err != nil {
		return err
	}
	fmt.Printf("q03 stage drill: %s %s\n", command.Name, strings.Join(command.Argv, " "))
	if mode == "plan" {
		return nil
	}
	if os.Getenv("KODAMA_Q03_STAGE_APPLY") != "1" {
		return errors.New("restore requires KODAMA_Q03_STAGE_APPLY=1")
	}
	if !credentialsPresent() {
		return errors.New("SCW_ACCESS_KEY and SCW_SECRET_KEY must be set for restore")
	}

	outcome, err := restorequal.ExecuteStageRestore(ctx, stageRunner{}, command)
	if err != nil {
		return fmt.Errorf("restore outcome %s: %w", outcome, err)
	}
	if outcome != restorequal.OutcomeAccepted {
		return fmt.Errorf("restore outcome = %s; the database may or may not exist, reconcile before any retry", outcome)
	}
	record := restoreRecord{
		RunID:          runID,
		Database:       destination.Destination.DatabaseName,
		InstanceID:     instanceID,
		BackupID:       backupID,
		SourceRevision: os.Getenv("KODAMA_Q03_SOURCE_REVISION"),
		RestoreOutcome: string(restorequal.OutcomeAccepted),
		At:             time.Now().UTC(),
	}
	if err := writeRecord(evidenceDir, record); err != nil {
		return err
	}

	verifier := stageVerifier{}
	if err := waitForDatabasePresent(ctx, verifier, destination.Destination); err != nil {
		return fmt.Errorf("restore accepted (%s) but readiness/listing did not converge: %w", record.Database, err)
	}
	verification, verifyErr := restorequal.VerifyStageRestore(
		ctx, verifier, destination.Destination, restorequal.DefaultObjectives().PostgreSQLRecovery,
	)
	hours := verification.PostgreSQLRetention.Hours()
	meetsPG := verification.MeetsPostgreSQLRecovery
	record.PostgreSQLRetentionHours = &hours
	record.MeetsPostgreSQLRecovery = &meetsPG
	if verifyErr != nil || verification.Outcome != restorequal.OutcomeConfirmed {
		_ = writeRecord(evidenceDir, record)
		return fmt.Errorf("restore accepted (%s) and not confirmed: %w", record.Database, verifyErr)
	}
	record.RestoreOutcome = string(restorequal.OutcomeConfirmed)
	record.At = time.Now().UTC()
	if err := writeRecord(evidenceDir, record); err != nil {
		return err
	}
	fmt.Printf("q03 stage drill: restored and confirmed %s (backup retention %.0fh, 30d met=%v); record written under %s\n",
		record.Database, hours, meetsPG, evidenceDir)
	return nil
}

func dropRecordedRun(ctx context.Context, instanceID, sourceDatabase, managedDatabase, runID string, existing []string, evidenceDir string) error {
	record, err := readRecord(evidenceDir, runID)
	if err != nil {
		return err
	}
	if record.InstanceID != instanceID {
		return fmt.Errorf("recorded instance %q is not the requested instance", record.InstanceID)
	}
	if record.Database != restorequal.RestoreDatabasePrefix+runID {
		return fmt.Errorf("recorded database %q does not belong to run %q", record.Database, runID)
	}
	found := slices.Contains(existing, record.Database)
	if !found {
		return fmt.Errorf("database %q is not present; refusing a drop that has no observed target", record.Database)
	}
	created := restorequal.CreatedDatabase{
		Name:       record.Database,
		InstanceID: record.InstanceID,
		RunID:      record.RunID,
	}
	command, err := restorequal.PlanStageDrop(restorequal.StageDropRequest{
		Created:          created,
		SourceDatabase:   sourceDatabase,
		ManagedDatabase:  managedDatabase,
		SourceInstanceID: instanceID,
	})
	if err != nil {
		return err
	}
	fmt.Printf("q03 stage drill: %s %s\n", command.Name, strings.Join(command.Argv, " "))
	if os.Getenv("KODAMA_Q03_STAGE_APPLY") != "1" {
		return errors.New("drop requires KODAMA_Q03_STAGE_APPLY=1")
	}
	if !credentialsPresent() {
		return errors.New("SCW_ACCESS_KEY and SCW_SECRET_KEY must be set for drop")
	}
	outcome, err := restorequal.ExecuteStageDrop(ctx, stageRunner{}, command)
	if err != nil {
		return fmt.Errorf("drop outcome %s: %w", outcome, err)
	}
	if outcome != restorequal.OutcomeAccepted {
		return fmt.Errorf("drop outcome = %s; observe the database before any retry", outcome)
	}
	record.DropOutcome = string(restorequal.OutcomeAccepted)
	if err := writeRecord(evidenceDir, record); err != nil {
		return err
	}

	verifier := stageVerifier{}
	confirmed, err := waitForDropConfirmed(ctx, verifier, created)
	if err != nil || confirmed != restorequal.OutcomeConfirmed {
		return fmt.Errorf("drop accepted (%s) and not confirmed: %w", record.Database, err)
	}
	record.DropOutcome = string(restorequal.OutcomeConfirmed)
	if err := writeRecord(evidenceDir, record); err != nil {
		return err
	}
	fmt.Printf("q03 stage drill: dropped and confirmed %s\n", record.Database)
	return nil
}

func waitForDatabasePresent(ctx context.Context, verifier restorequal.StageVerifier, destination restorequal.Destination) error {
	waitCtx, cancel := context.WithTimeout(ctx, stageWaitTimeout)
	defer cancel()
	var lastErr error
	for {
		instance, err := verifier.ObserveInstance(waitCtx, destination.InstanceID)
		if err != nil {
			lastErr = err
		} else if !instance.Ready {
			lastErr = fmt.Errorf("instance status is %q", instance.Status)
		} else {
			databases, listErr := verifier.ListDatabases(waitCtx, destination.InstanceID)
			if listErr != nil {
				lastErr = listErr
			} else if slices.Contains(databases, destination.DatabaseName) {
				return nil
			} else {
				lastErr = fmt.Errorf("database %q is not yet listed", destination.DatabaseName)
			}
		}
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return lastErr
			}
			return waitCtx.Err()
		case <-time.After(stagePollInterval):
		}
	}
}

func waitForDropConfirmed(ctx context.Context, verifier restorequal.StageVerifier, created restorequal.CreatedDatabase) (restorequal.Outcome, error) {
	waitCtx, cancel := context.WithTimeout(ctx, stageWaitTimeout)
	defer cancel()
	var lastErr error
	for {
		outcome, err := restorequal.VerifyStageDrop(waitCtx, verifier, created)
		if err == nil && outcome == restorequal.OutcomeConfirmed {
			return outcome, nil
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return restorequal.OutcomeAccepted, lastErr
			}
			return restorequal.OutcomeAccepted, waitCtx.Err()
		case <-time.After(stagePollInterval):
		}
	}
}

type stageVerifier struct{}

func (stageVerifier) ObserveInstance(ctx context.Context, instanceID string) (restorequal.StageInstanceStatus, error) {
	command := exec.CommandContext(ctx, "scw",
		"rdb", "instance", "get",
		instanceID,
		"--output", "json",
	)
	var stdout strings.Builder
	command.Stdout = &stdout
	command.Stderr = nil
	if err := command.Run(); err != nil {
		return restorequal.StageInstanceStatus{}, errors.New("observe stage RDB instance failed")
	}
	return restorequal.ParseStageInstanceJSON([]byte(stdout.String()))
}

func (stageVerifier) ListDatabases(ctx context.Context, instanceID string) ([]string, error) {
	return listDatabases(ctx, instanceID)
}

func (stageVerifier) VerifySQL(ctx context.Context, databaseName string) error {
	rawURL := strings.TrimSpace(os.Getenv(restorequal.DatabaseCredentialEnvName))
	if rawURL == "" {
		return fmt.Errorf("%s is required to confirm restored SQL state", restorequal.DatabaseCredentialEnvName)
	}
	cfg, err := pgx.ParseConfig(rawURL)
	if err != nil {
		return fmt.Errorf("parse %s: invalid connection configuration", restorequal.DatabaseCredentialEnvName)
	}
	cfg.Database = databaseName

	sqlCtx, cancel := context.WithTimeout(ctx, sqlVerifyTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(sqlCtx, cfg)
	if err != nil {
		return safePGError("connect to restored database", databaseName, err)
	}
	defer func() { _ = conn.Close(sqlCtx) }()

	var migrations int64
	if err := conn.QueryRow(sqlCtx, `SELECT count(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		return safePGError("query schema_migrations", databaseName, err)
	}
	if migrations < 1 {
		return fmt.Errorf("restored database %q has %d applied migrations; want at least 1", databaseName, migrations)
	}
	var tenants int64
	if err := conn.QueryRow(sqlCtx, `SELECT count(*) FROM tenants`).Scan(&tenants); err != nil {
		return safePGError("query tenants", databaseName, err)
	}
	return nil
}

func safePGError(action, databaseName string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code != "" {
		return fmt.Errorf("%s on %q failed (SQLSTATE %s)", action, databaseName, pgErr.Code)
	}
	return fmt.Errorf("%s on %q failed", action, databaseName)
}

// listDatabases observes the instance's databases. A nil list is never
// treated as an observation: a decode failure refuses the drill.
func listDatabases(ctx context.Context, instanceID string) ([]string, error) {
	command := exec.CommandContext(ctx, "scw",
		"rdb", "database", "list",
		"instance-id="+instanceID,
		"--output", "json",
	)
	var stdout strings.Builder
	command.Stdout = &stdout
	command.Stderr = nil
	if err := command.Run(); err != nil {
		return nil, errors.New("observe existing databases failed")
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &rows); err != nil {
		return nil, errors.New("decode existing database listing")
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Name != "" {
			names = append(names, row.Name)
		}
	}
	return names, nil
}

// stageRunner executes one planned argv without a shell and never returns
// stderr.
type stageRunner struct{}

func (stageRunner) Run(ctx context.Context, command restorequal.Command) (restorequal.CommandResult, error) {
	process := exec.CommandContext(ctx, command.Name, command.Argv...)
	var stdout strings.Builder
	process.Stdout = &stdout
	process.Stderr = nil
	err := process.Run()
	result := restorequal.CommandResult{Stdout: []byte(stdout.String())}
	if err == nil {
		return result, nil
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func credentialsPresent() bool {
	return strings.TrimSpace(os.Getenv("SCW_ACCESS_KEY")) != "" &&
		strings.TrimSpace(os.Getenv("SCW_SECRET_KEY")) != ""
}

func recordPath(directory, runID string) string {
	return filepath.Join(directory, "q03-stage-restore-"+runID+".json")
}

func writeRecord(directory string, record restoreRecord) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create evidence directory: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode restore record: %w", err)
	}
	path := recordPath(directory, record.RunID)
	temp, err := os.CreateTemp(directory, ".q03-stage-*")
	if err != nil {
		return fmt.Errorf("stage restore record: %w", err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	data = append(data, '\n')
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write restore record: %w", err)
	}
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set restore record mode: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close restore record: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publish restore record: %w", err)
	}
	return nil
}

func readRecord(directory, runID string) (restoreRecord, error) {
	data, err := os.ReadFile(recordPath(directory, runID))
	if err != nil {
		return restoreRecord{}, fmt.Errorf("read restore record for run %q: %w", runID, err)
	}
	var record restoreRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return restoreRecord{}, fmt.Errorf("decode restore record for run %q: %w", runID, err)
	}
	if record.RunID != runID {
		return restoreRecord{}, fmt.Errorf("restore record names run %q", record.RunID)
	}
	if record.RestoreOutcome != string(restorequal.OutcomeAccepted) &&
		record.RestoreOutcome != string(restorequal.OutcomeConfirmed) {
		return restoreRecord{}, errors.New("restore record has no accepted or confirmed outcome")
	}
	return record, nil
}
