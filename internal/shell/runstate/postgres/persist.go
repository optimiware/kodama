package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// persist writes the difference between the stored record and the record the
// transition produced, inside the caller's transaction. The step rows are
// written with their waits and outputs as one row each, so the table's checks
// see the step status and the wait state together.
func persist(ctx context.Context, tx pgx.Tx, stored, next runtime.RunRecord, t runtime.Transition) error {
	if err := writeRun(ctx, tx, stored.Run, next.Run, t.Now); err != nil {
		return err
	}
	if t.RenewTTL > 0 && next.Claim != stored.Claim {
		if _, err := tx.Exec(ctx, stmtSetLeaseExpiry, t.Scope.TenantID, t.Scope.ProjectID, t.RunID, next.Claim.Expires); err != nil {
			return fmt.Errorf("runstate/postgres: renew the lease: %w", err)
		}
	}
	// Outputs CheckOutputs accepted are appended after the recorded ones.
	added := next.Outputs[len(stored.Outputs):]
	for i, s := range next.Run.Steps {
		sw, nw := waitOf(stored.Waits, s.Key), waitOf(next.Waits, s.Key)
		var outputs *runtime.StepOutputs
		if j := slices.IndexFunc(added, func(o runtime.StepOutputs) bool { return o.StepKey == s.Key }); j >= 0 {
			outputs = &added[j]
		}
		if outputs == nil && same(stored.Run.Steps[i], s) && sameWait(sw, nw) {
			continue
		}
		if err := writeStep(ctx, tx, t.Scope, t.RunID, s, nw, outputs, t.Now); err != nil {
			return err
		}
	}
	for _, c := range t.Commands {
		switch i := slices.IndexFunc(stored.Commands, func(x runtime.Command) bool { return x.ID == c.ID }); {
		case i < 0:
			if err := insertCommand(ctx, tx, c); err != nil {
				return err
			}
		case !same(stored.Commands[i], c):
			if err := updateCommand(ctx, tx, c); err != nil {
				return err
			}
		}
	}
	for _, o := range t.Workloads {
		b := o.Binding
		switch i := slices.IndexFunc(stored.Workloads, func(x runtime.WorkloadObservation) bool {
			return x.Binding.StepKey == b.StepKey && x.Binding.Attempt == b.Attempt
		}); {
		case i < 0:
			if err := insertWorkload(ctx, tx, o); err != nil {
				return err
			}
		case !same(stored.Workloads[i], o):
			if err := updateWorkload(ctx, tx, o); err != nil {
				return err
			}
		}
	}
	for _, o := range t.Resources {
		id := resourceIdentity(o.Handle)
		switch i := slices.IndexFunc(stored.Resources, func(x runtime.ResourceObservation) bool { return resourceIdentity(x.Handle) == id }); {
		case i < 0:
			if err := insertResource(ctx, tx, t.RunID, o); err != nil {
				return err
			}
		case !same(stored.Resources[i], o):
			if err := updateResource(ctx, tx, t.RunID, o); err != nil {
				return err
			}
		}
	}
	for _, m := range t.Materializations {
		switch i := slices.IndexFunc(stored.Materializations, func(x runtime.MaterializationRecord) bool { return x.Ref.ID == m.Ref.ID }); {
		case i < 0:
			if err := insertMaterialization(ctx, tx, m); err != nil {
				return err
			}
		case stored.Materializations[i].State != m.State:
			if err := revokeMaterialization(ctx, tx, m, t.Now); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameWait(a, b *runtime.Wait) bool {
	if a == nil || b == nil {
		return a == b
	}
	return same(*a, *b)
}

// eventRowID is the audit_events identity of an event. The event's own id is a
// deduplication key that is not a database identifier (it has dots and colons)
// and is only unique inside its project, so the row id is a digest of both; the
// event itself, id included, is the payload.
func eventRowID(project domain.ProjectID, id string) string {
	sum := sha256.Sum256([]byte(string(project) + "\x00" + id))
	return "ev-" + hex.EncodeToString(sum[:])
}

// emit appends events to the outbox in the caller's transaction, so they commit
// with the state they describe or not at all. An event already recorded with the
// same identity and content is not added again; the same identity with other
// content is a conflict.
func emit(ctx context.Context, tx pgx.Tx, events []runtime.Event) error {
	for _, e := range events {
		row := eventRowID(e.Scope.ProjectID, e.ID)
		tag, err := tx.Exec(ctx, stmtInsertEvent, e.Scope.TenantID, row, e.Scope.ProjectID, string(e.Kind), e.RunID, jsonOf(e))
		if err != nil {
			return fmt.Errorf("runstate/postgres: record event %s: %w", e.ID, err)
		}
		if tag.RowsAffected() == 1 {
			continue
		}
		var payload []byte
		if err := tx.QueryRow(ctx, stmtGetEvent, e.Scope.TenantID, row).Scan(&payload); err != nil {
			return fmt.Errorf("runstate/postgres: read event %s: %w", e.ID, err)
		}
		var prev runtime.Event
		if err := json.Unmarshal(payload, &prev); err != nil {
			return fmt.Errorf("runstate/postgres: decode event %s: %w", e.ID, err)
		}
		if !sameEvent(prev, e) {
			return fmt.Errorf("%w: event %s was recorded with other content", domain.ErrConflict, e.ID)
		}
	}
	return nil
}

func sameEvent(a, b runtime.Event) bool {
	at := a.At.Equal(b.At)
	a.At = b.At
	return at && a == b
}

// Outbox returns the events committed for one run, in commit order. A run that
// is not in the scope is not found. It is the bounded read the publisher and the
// shared contract suite use; it never changes the outbox.
func (s *Store) Outbox(ctx context.Context, scope domain.Scope, id domain.RunID) ([]runtime.Event, error) {
	if err := checkRef(scope, id); err != nil {
		return nil, err
	}
	var out []runtime.Event
	err := s.view(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := loadRecord(ctx, tx, scope, id, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, stmtRunEvents, scope.TenantID, scope.ProjectID, id)
		if err != nil {
			return fmt.Errorf("runstate/postgres: read the outbox: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				return fmt.Errorf("runstate/postgres: scan event: %w", err)
			}
			var e runtime.Event
			if err := json.Unmarshal(payload, &e); err != nil {
				return fmt.Errorf("runstate/postgres: decode event: %w", err)
			}
			e.At = e.At.UTC()
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}
