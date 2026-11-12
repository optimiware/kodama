package memory

// audit is write-only for the controller: it is an INSERT-only write table and
// is in no read set. It has no controller SELECT grant or policy, so a
// cross-scope controller read of it would return another tenant's audit rows.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
)

func TestControllerCannotReadAudit(t *testing.T) {
	s := New()
	scope := scopeFor("tenant-9", "project-9")
	mustExecute(t, s, scope, store.Unit{
		Audit: []event.Audit{defaultAudit(scope, "audit-1", "execution.engine.commit")},
	})

	// Non-vacuous: the audit row really is committed and would be visible to a
	// cross-scope reader that admitted the table.
	if _, found := s.Snapshot(scope, "audit", "audit-1"); !found {
		t.Fatal("the audit row was not committed; the refusal check would be vacuous")
	}

	controller := s.Controller()
	if _, err := controller.Read(context.Background(), "audit", "audit-1"); err == nil {
		t.Fatal("controller Read of audit succeeded; audit is write-only")
	} else {
		assertFault(t, err, fault.InvalidInput)
	}

	scan := store.ControllerScan{Table: "audit", Limit: 10}
	assertFault(t, scan.Validate(), fault.InvalidInput)
	if _, err := controller.Scan(context.Background(), scan); err == nil {
		t.Fatal("controller Scan of audit succeeded; audit is write-only")
	} else {
		assertFault(t, err, fault.InvalidInput)
	}
}

func TestControllerDeclarationKeepsAuditWriteOnly(t *testing.T) {
	if !controllerWriteTable("audit") {
		t.Error("audit must stay a controller write table")
	}
	if !controllerInsertOnlyTable("audit") {
		t.Error("audit must stay INSERT-only for the controller")
	}
	if controllerReadTable("audit") {
		t.Error("audit must not be a controller read table")
	}
	if !controllerReadTable("events") {
		t.Error("events must stay readable for the trigger EventReader")
	}
}
