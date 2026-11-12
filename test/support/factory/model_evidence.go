package factory

// Model gateway evidence reads for the deterministic fixture.
//
// These helpers read only the durable rows the real gateway committed, through
// the same runtime role and transaction-local scope the composition writes with.
// They never synthesize a usage or content row and never fabricate attribution:
// a missing row means the composition did not commit one.

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
)

// ModelUsageRows returns the committed model_usage rows of the fixture project.
func (h *Harness) ModelUsageRows(ctx context.Context) ([]store.Row, error) {
	h.T.Helper()
	return h.modelRows(ctx, gateway.UsageTable)
}

// ModelContentRows returns the committed content_records rows of the fixture
// project. The model fixture never opts into raw capture, so a default harness
// configured with it must observe none.
func (h *Harness) ModelContentRows(ctx context.Context) ([]store.Row, error) {
	h.T.Helper()
	return h.modelRows(ctx, gateway.ContentTable)
}

func (h *Harness) modelRows(ctx context.Context, table string) ([]store.Row, error) {
	h.T.Helper()
	scanner := storepg.New(h.runtimeDB)
	page, err := scanner.Query(ctx, h.Scope, store.ScanQuery{Table: table, Limit: 64})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", table, err)
	}
	return page.Rows, nil
}
