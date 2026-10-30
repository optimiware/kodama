package app

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Phase 2 changes persistence; Phase 3 replaces the old module queries. Refuse
// this concrete incompatibility before starting broker consumers or workers.
func requireLegacySchema(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var simplified bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('public.tasks') IS NOT NULL AND to_regclass('public.work_items') IS NULL`).Scan(&simplified); err != nil {
		return err
	}
	if simplified {
		return fmt.Errorf("%w: ADR 0013 schema is installed; Phase 3 server adapter wiring is required", errNotComposed)
	}
	return nil
}
