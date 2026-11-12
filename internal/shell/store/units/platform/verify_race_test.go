package platform_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// readHook wraps the memory store and applies one concurrent commit after a row
// of the named tables is read. It reproduces, deterministically, the window
// between an authority read and the adapter's commit transaction: the engine's
// running-observation commit (which advances the execution and action versions
// once per start) or a credential/state change lands after the read but before
// the verification commit.
type readHook struct {
	*memoryStoreAlias
	after      map[string]func(store.Row) error
	afterQuery map[string]func(store.QueryPage) error
}

func (hook *readHook) Read(ctx context.Context, scope tenancy.Scope, table, identity string) (store.Row, error) {
	row, err := hook.memoryStoreAlias.Read(ctx, scope, table, identity)
	if err != nil {
		return row, err
	}
	if mutate := hook.after[table]; mutate != nil {
		if err := mutate(row); err != nil {
			return store.Row{}, err
		}
	}
	return row, nil
}

func (hook *readHook) Query(ctx context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	page, err := hook.memoryStoreAlias.Query(ctx, scope, query)
	if err != nil {
		return page, err
	}
	if mutate := hook.afterQuery[query.Table]; mutate != nil {
		if err := mutate(page); err != nil {
			return store.QueryPage{}, err
		}
	}
	return page, nil
}

func hookedAdapter(t *testing.T, s *memoryStoreAlias, after map[string]func(store.Row) error) *platform.RecordAdapter {
	t.Helper()
	hook := &readHook{memoryStoreAlias: s, after: after}
	adapter, err := platform.NewRecordAdapter(hook, hook)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	return adapter
}

// advanceRow commits one guarded write against the row the hook just read. It
// is the observation-commit / credential-change / state-change stand-in; the
// expected version comes from the row snapshot the authority read observed.
func advanceRow(s *memoryStoreAlias, scope tenancy.Scope, table, identity string, row store.Row, values map[string]any) error {
	var version uint64
	switch observed := row.Values["version"].(type) {
	case uint64:
		version = observed
	case int64:
		if observed < 0 {
			return fmt.Errorf("%s row has a negative version", table)
		}
		version = uint64(observed)
	default:
		return fmt.Errorf("%s row has no usable version: %T", table, row.Values["version"])
	}
	_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         table,
		ID:            identity,
		ExpectVersion: version,
		Values:        values,
	}}})
	return err
}

func seedExecutionCredential(t *testing.T, s *memoryStoreAlias, nonce []byte) {
	t.Helper()
	_, err := s.Execute(context.Background(), testScope(), store.Unit{Writes: []store.Write{{
		Table:         "executions",
		ID:            string(testExecution),
		ExpectVersion: 1,
		Values: map[string]any{
			"token_nonce":      nonce,
			"token_expires_at": fixedNow.Add(time.Hour),
		},
	}}})
	if err != nil {
		t.Fatalf("seed execution credential: %v", err)
	}
}

func observationBump(s *memoryStoreAlias) map[string]func(store.Row) error {
	return map[string]func(store.Row) error{
		"executions": func(row store.Row) error {
			return advanceRow(s, testScope(), "executions", string(testExecution), row,
				map[string]any{"state": "running"})
		},
		"actions": func(row store.Row) error {
			return advanceRow(s, testScope(), "actions", string(testAction), row,
				map[string]any{"name": "approve"})
		},
	}
}

// TestRecordSurvivesObservationVersionBump is the defect regression: the
// engine's running-observation commit advances the execution and action rows
// between the verifier's current-authority read and the record commit. The
// execution is still running and the action deadline is still live, so the
// verification must commit instead of refusing version_conflict.
func TestRecordSurvivesObservationVersionBump(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter := hookedAdapter(t, s, observationBump(s))

	result, err := adapter.Record(context.Background(), recordUnit(t))
	if err != nil {
		t.Fatalf("Record after the observation version bump: %v", err)
	}
	if result.Replayed || string(result.ID) != testVerification {
		t.Errorf("result = %#v, want a fresh verification id", result)
	}
	if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err != nil {
		t.Fatalf("verification row was not committed: %v", err)
	}
}

// TestRecordRefusesStoppedExecutionBetweenReadAndCommit asserts the running
// state guard still refuses when the execution stopped after the read, even
// though the version pin no longer fires on an observation bump.
func TestRecordRefusesStoppedExecutionBetweenReadAndCommit(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter := hookedAdapter(t, s, map[string]func(store.Row) error{
		"executions": func(row store.Row) error {
			return advanceRow(s, testScope(), "executions", string(testExecution), row,
				map[string]any{"state": "succeeded"})
		},
	})

	_, err := adapter.Record(context.Background(), recordUnit(t))
	requireClass(t, err, fault.VersionConflict)
	requireNoVerificationState(t, s)
}

// TestRecordRefusesExpiredDeadlineBetweenReadAndCommit asserts the deadline
// guard still refuses when the action deadline passes after the read.
func TestRecordRefusesExpiredDeadlineBetweenReadAndCommit(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter := hookedAdapter(t, s, map[string]func(store.Row) error{
		"actions": func(row store.Row) error {
			return advanceRow(s, testScope(), "actions", string(testAction), row,
				map[string]any{"deadline": fixedNow})
		},
	})

	_, err := adapter.Record(context.Background(), recordUnit(t))
	requireClass(t, err, fault.DeadlineExpired)
	requireNoVerificationState(t, s)
}

// TestRecordRefusesClearedCredentialBetweenReadAndCommit asserts the execution
// credential is still re-checked at commit. Clearing the nonce/expiry pair no
// longer changes the row version the verification reads, so the commit must
// guard the credential columns themselves.
func TestRecordRefusesClearedCredentialBetweenReadAndCommit(t *testing.T) {
	s := seedVerificationFixtures(t)
	seedExecutionCredential(t, s, make([]byte, 32))
	adapter := hookedAdapter(t, s, map[string]func(store.Row) error{
		"executions": func(row store.Row) error {
			return advanceRow(s, testScope(), "executions", string(testExecution), row,
				map[string]any{"token_nonce": nil, "token_expires_at": nil})
		},
	})

	_, err := adapter.Record(context.Background(), recordUnit(t))
	requireClass(t, err, fault.VersionConflict)
	requireNoVerificationState(t, s)
}

// TestRecordRefusesRotatedCredentialBetweenReadAndCommit asserts a rotated
// nonce refuses the commit: the old credential is no longer current authority.
func TestRecordRefusesRotatedCredentialBetweenReadAndCommit(t *testing.T) {
	s := seedVerificationFixtures(t)
	rotated := make([]byte, 32)
	for index := range rotated {
		rotated[index] = 9
	}
	seedExecutionCredential(t, s, make([]byte, 32))
	adapter := hookedAdapter(t, s, map[string]func(store.Row) error{
		"executions": func(row store.Row) error {
			return advanceRow(s, testScope(), "executions", string(testExecution), row,
				map[string]any{"token_nonce": rotated, "token_expires_at": fixedNow.Add(2 * time.Hour)})
		},
	})

	_, err := adapter.Record(context.Background(), recordUnit(t))
	requireClass(t, err, fault.VersionConflict)
	requireNoVerificationState(t, s)
}

// TestRecordRefusesStoppedExecutionPrincipalBetweenReadAndCommit isolates the
// principal-enablement guard: the unit carries only the principal_enabled
// execution read, so the running-state re-check inside that lock is what must
// refuse an execution principal that stopped between its resolution and the
// commit.
func TestRecordRefusesStoppedExecutionPrincipalBetweenReadAndCommit(t *testing.T) {
	s := seedVerificationFixtures(t)
	hook := &readHook{
		memoryStoreAlias: s,
		afterQuery: map[string]func(store.QueryPage) error{
			"executions": func(page store.QueryPage) error {
				if len(page.Rows) != 1 {
					return fmt.Errorf("execution principal query returned %d rows", len(page.Rows))
				}
				return advanceRow(s, testScope(), "executions", string(testExecution), page.Rows[0],
					map[string]any{"state": "succeeded"})
			},
		},
	}
	adapter, err := platform.NewRecordAdapter(hook, hook)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	authority := make([]authorize.AuthorityRead, 0, len(unit.Authority))
	for _, read := range unit.Authority {
		if read.Check == authorize.CheckExecutionRunning || read.Check == authorize.CheckActionDeadline {
			continue
		}
		authority = append(authority, read)
	}
	unit.Authority = authority

	_, err = adapter.Record(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	requireNoVerificationState(t, s)
}

// TestRecordKeepsExplicitExecutionVersionPin asserts a caller that observed and
// supplied an execution version still gets the version guard: an observation
// bump is then a legitimate version_conflict, not silently accepted.
func TestRecordKeepsExplicitExecutionVersionPin(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter := hookedAdapter(t, s, observationBump(s))
	unit := recordUnit(t)
	for index := range unit.Authority {
		if unit.Authority[index].Check == authorize.CheckExecutionRunning {
			unit.Authority[index].ExpectedVersion = 1
		}
	}

	_, err := adapter.Record(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	requireNoVerificationState(t, s)
}

// TestRecordKeepsExplicitActionVersionPin asserts a caller-supplied action
// version still pins the action row.
func TestRecordKeepsExplicitActionVersionPin(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter := hookedAdapter(t, s, observationBump(s))
	unit := recordUnit(t)
	for index := range unit.Authority {
		if unit.Authority[index].Check == authorize.CheckActionDeadline {
			unit.Authority[index].ExpectedVersion = 1
		}
	}

	_, err := adapter.Record(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	requireNoVerificationState(t, s)
}

// TestRecordRefusesRealRotatedCredentialBetweenReadAndCommit is the nonce
// guard pin with an actual credential pair: the seeded nonce belongs to a real
// derived kodex execution token, and the rotation that lands between the read
// and the commit is the next real token. A guard that only compared synthetic
// byte slices, or that treated an all-zero nonce as "absent", would not protect
// the live token a Git hook presents.
func TestRecordRefusesRealRotatedCredentialBetweenReadAndCommit(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	current := token.ExecutionClaims{
		TenantID:    testTenant,
		ExecutionID: testExecution,
		Nonce:       bytes.Repeat([]byte{0x11}, 32),
		Expiry:      fixedNow.Add(time.Hour),
	}
	rotated := token.ExecutionClaims{
		TenantID:    testTenant,
		ExecutionID: testExecution,
		Nonce:       bytes.Repeat([]byte{0x22}, 32),
		Expiry:      fixedNow.Add(2 * time.Hour),
	}
	presented, err := token.DeriveExecutionToken(key, current)
	if err != nil {
		t.Fatalf("derive the current execution credential: %v", err)
	}
	if err := token.VerifyExecutionToken(key, presented, current, fixedNow); err != nil {
		t.Fatalf("the current execution credential does not verify: %v", err)
	}
	replacement, err := token.DeriveExecutionToken(key, rotated)
	if err != nil {
		t.Fatalf("derive the rotated execution credential: %v", err)
	}
	if replacement == presented {
		t.Fatal("rotation produced the same execution credential")
	}
	if err := token.VerifyExecutionToken(key, replacement, rotated, fixedNow); err != nil {
		t.Fatalf("the rotated execution credential does not verify: %v", err)
	}

	s := seedVerificationFixtures(t)
	seedExecutionCredential(t, s, current.Nonce)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	if _, err := adapter.Record(context.Background(), recordUnit(t)); err != nil {
		t.Fatalf("Record with the current real credential: %v", err)
	}
	if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err != nil {
		t.Fatalf("the verification row was not committed with the current credential: %v", err)
	}

	rotating := seedVerificationFixtures(t)
	seedExecutionCredential(t, rotating, current.Nonce)
	rotatedAdapter := hookedAdapter(t, rotating, map[string]func(store.Row) error{
		"executions": func(row store.Row) error {
			return advanceRow(rotating, testScope(), "executions", string(testExecution), row,
				map[string]any{"token_nonce": rotated.Nonce, "token_expires_at": rotated.Expiry})
		},
	})

	_, err = rotatedAdapter.Record(context.Background(), recordUnit(t))
	requireClass(t, err, fault.VersionConflict)
	requireNoVerificationState(t, rotating)
}
