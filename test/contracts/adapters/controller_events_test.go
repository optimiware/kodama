package adaptercontract

// The cross-scope controller seam may carry execution-bound lifecycle event
// rows and audit rows in the same unit as the Kernel state writes. The scope
// is derived from the guarded execution row inside the transaction, every
// event/audit row must carry exactly that scope, receipts stay refused, and
// events/audit are INSERT-only (no update or delete).
//
// The vectors below run against a deterministic reference controller. They are
// semantic conformance inputs for the store adapters; they prove no PostgreSQL
// isolation, RLS or lock behavior.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type controllerEventDocument struct {
	Version int                     `json:"version"`
	Vectors []controllerEventVector `json:"vectors"`
}

type controllerEventVector struct {
	Name  string                `json:"name"`
	Clock string                `json:"clock"`
	Seed  seamSeed              `json:"seed"`
	Steps []controllerEventStep `json:"steps"`
}

type controllerEventStep struct {
	Op            string                `json:"op"`
	Table         string                `json:"table"`
	ID            string                `json:"id"`
	ExecutionID   string                `json:"execution_id"`
	Scope         *seamScope            `json:"scope"`
	Locks         []seamLock            `json:"locks"`
	Writes        []seamWrite           `json:"writes"`
	Events        []controllerEventSpec `json:"events"`
	Audit         []controllerAuditSpec `json:"audit"`
	Receipt       *seamSeedReceipt      `json:"receipt"`
	ReceiptMode   string                `json:"receipt_mode"`
	ReceiptExpect *seamReceiptExpect    `json:"receipt_expect"`
	Equals        map[string]any        `json:"equals"`
	Range         []scannerRange        `json:"range"`
	OrderBy       []scannerOrder        `json:"order_by"`
	Limit         int                   `json:"limit"`
	WantValues    map[string]any        `json:"want_values"`
	WantVersion   *uint64               `json:"want_version"`
	WantAbsent    bool                  `json:"want_absent"`
	WantIDs       []string              `json:"want_ids"`
	WantError     string                `json:"want_error"`
}

type controllerEventSpec struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	ReportType      string     `json:"report_type"`
	SourceKind      string     `json:"source_kind"`
	SourcePrincipal string     `json:"source_principal"`
	At              string     `json:"at"`
	Scope           *seamScope `json:"scope"`
	SubjectID       string     `json:"subject_id"`
	SubjectVersion  uint64     `json:"subject_version"`
	CorrelationID   string     `json:"correlation_id"`
	CausationID     string     `json:"causation_id"`
	Data            string     `json:"data"`
}

type controllerAuditSpec struct {
	ID        string     `json:"id"`
	Scope     *seamScope `json:"scope"`
	Actor     string     `json:"actor"`
	Operation string     `json:"operation"`
	At        string     `json:"at"`
	Detail    string     `json:"detail"`
}

// TestControllerEngineEventVectors runs every controller event vector against
// a reference controller and proves the lock/scope vectors are not vacuous.
func TestControllerEngineEventVectors(t *testing.T) {
	document := loadControllerEventVectors(t)
	validateControllerEventVectors(t, document)
	for _, vector := range document.Vectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			controller := newReferenceEventController(vector.Seed, vector.Clock)
			if err := runControllerEventVector(vector, controller); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}

	// Non-vacuous: a controller that silently supplies the execution lock would
	// accept the event-without-guard vector, so at least one vector must fail.
	failures := 0
	for _, vector := range document.Vectors {
		controller := brokenEventController{referenceEventController: newReferenceEventController(vector.Seed, vector.Clock)}
		if err := runControllerEventVector(vector, controller); err != nil {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("no controller vector failed against a fake that supplies a missing execution guard")
	}
}

// TestControllerEventAllowList pins the controller table authority and the
// INSERT-only event/audit grants. It is a declaration check, not adapter
// behavior. The read set includes events but never admits audit: the audit
// trail is write-only for the controller.
func TestControllerEventAllowList(t *testing.T) {
	readTables := []string{"executions", "actions", "cleanup", "events"}
	if got := store.ControllerReadTables(); !sameStringSet(got, readTables) {
		t.Errorf("ControllerReadTables = %v; want %v", got, readTables)
	}
	writeTables := []string{"executions", "actions", "cleanup", "events", "audit"}
	if got := store.ControllerWriteTables(); !sameStringSet(got, writeTables) {
		t.Errorf("ControllerWriteTables = %v; want %v", got, writeTables)
	}
	if stringSliceContains(store.ControllerReadTables(), "audit") {
		t.Error("audit must never be in the controller read set")
	}

	wantGrants := map[string][]string{
		"executions": {"select", "update"},
		"actions":    {"select", "insert", "update"},
		"cleanup":    {"select", "insert", "update"},
		"events":     {"select", "insert"},
		"audit":      {"insert"},
	}
	grants := grantsForRole(store.ControllerGrants(), store.ControllerRole)
	if len(grants) != len(wantGrants) {
		t.Errorf("controller grants = %d; want exactly %d (%v)", len(grants), len(wantGrants), wantGrants)
	}
	for _, grant := range grants {
		operations, ok := wantGrants[grant.Table]
		if !ok {
			t.Errorf("controller grant targets unexpected table %q", grant.Table)
			continue
		}
		if !sameOperationSet(grant.Operations, operations) {
			t.Errorf("controller grant %q operations = %v; want %v", grant.Table, grant.Operations, operations)
		}
		for _, operation := range grant.Operations {
			if operation == store.OperationDelete {
				t.Errorf("controller grant %q holds DELETE", grant.Table)
			}
		}
	}
	if grantFor(store.ControllerGrants(), store.ControllerRole, "receipts") != nil {
		t.Error("the controller role must have no receipt grant")
	}

	// The events table is the outbox: the publisher role keeps its own grant.
	publisher := grantsForRole(store.PublisherGrants(), store.PublisherRole)
	if len(publisher) != 1 || publisher[0].Table != "events" {
		t.Fatalf("publisher grants = %+v; want exactly one events grant", publisher)
	}
	if !sameOperationSet(publisher[0].Operations, []string{"select", "update"}) {
		t.Errorf("publisher operations = %v; want select,update", publisher[0].Operations)
	}
	if !sameStringSet(publisher[0].WriteColumns, []string{"publish_state"}) {
		t.Errorf("publisher write columns = %v; want publish_state", publisher[0].WriteColumns)
	}
}

// TestControllerEventPolicies pins the forced-RLS policies for the controller
// event/audit inserts: the derived transaction scope is the only permitted
// write scope, and the publisher keeps its publish_state policy.
func TestControllerEventPolicies(t *testing.T) {
	policies := store.SystemPolicies()
	for _, table := range []string{"events", "audit"} {
		policy := policyFor(policies, store.ControllerRole, table, store.PolicyInsert)
		if policy == nil {
			t.Errorf("controller %s INSERT policy is missing", table)
			continue
		}
		if policy.Using != store.PredicateScopeMatchesTxnScope || policy.WithCheck != store.PredicateScopeMatchesTxnScope {
			t.Errorf("controller %s INSERT policy = using %q / with check %q; want %q",
				table, policy.Using, policy.WithCheck, store.PredicateScopeMatchesTxnScope)
		}
		for _, command := range []store.PolicyCommand{store.PolicyUpdate, store.PolicyDelete} {
			if policyFor(policies, store.ControllerRole, table, command) != nil {
				t.Errorf("controller %s must have no %s policy", table, command)
			}
		}
	}
	eventsSelect := policyFor(policies, store.ControllerRole, "events", store.PolicySelect)
	if eventsSelect == nil || eventsSelect.Using != store.PredicateAny {
		t.Errorf("controller events SELECT policy = %+v; want the cross-scope any predicate", eventsSelect)
	}

	publisherUpdate := policyFor(policies, store.PublisherRole, "events", store.PolicyUpdate)
	if publisherUpdate == nil {
		t.Fatal("publisher events UPDATE policy is missing")
	}
	if publisherUpdate.Using != store.PredicateAny || publisherUpdate.WithCheck != store.PredicateColumnInSet ||
		publisherUpdate.Column != "publish_state" || !sameStringSet(publisherUpdate.Values, []string{"pending", "published"}) {
		t.Errorf("publisher events UPDATE policy = %+v; want the publish_state column policy", publisherUpdate)
	}

	schema := store.SchemaDesign()
	for _, name := range []string{"events", "audit"} {
		table, ok := findTable(schema, name)
		if !ok {
			t.Errorf("schema is missing table %q", name)
			continue
		}
		if !table.RowLevelSecurity || !table.ForceRowLevelSecurity {
			t.Errorf("table %q must enable and force row-level security", name)
		}
	}
}

// TestControllerAuthorityIsBackedByGrantsAndPolicies is the regression for
// the controller read set: every declared controller read table
// needs both the controller SELECT grant and the controller SELECT policy that
// make the read legal, every declared grant operation needs a matching policy,
// and every controller policy needs a matching grant. audit stays write-only.
func TestControllerAuthorityIsBackedByGrantsAndPolicies(t *testing.T) {
	grants := store.ControllerGrants()
	policies := store.SystemPolicies()
	readTables := store.ControllerReadTables()
	writeTables := store.ControllerWriteTables()

	for _, table := range readTables {
		grant := grantFor(grants, store.ControllerRole, table)
		if grant == nil || !operationSetContains(grant.Operations, store.OperationSelect) {
			t.Errorf("controller read table %q has no SELECT grant", table)
		}
		if policyFor(policies, store.ControllerRole, table, store.PolicySelect) == nil {
			t.Errorf("controller read table %q has no SELECT policy", table)
		}
		if !stringSliceContains(writeTables, table) {
			t.Errorf("controller read table %q is outside the write set", table)
		}
	}

	for _, table := range writeTables {
		grant := grantFor(grants, store.ControllerRole, table)
		if grant == nil {
			t.Errorf("controller write table %q has no grant", table)
			continue
		}
		for _, operation := range grant.Operations {
			if policyFor(policies, store.ControllerRole, table, store.PolicyCommand(operation)) == nil {
				t.Errorf("controller grant %q %q has no matching policy", table, operation)
			}
		}
	}
	for _, grant := range grantsForRole(grants, store.ControllerRole) {
		if !stringSliceContains(writeTables, grant.Table) {
			t.Errorf("controller grant targets %q outside the write set", grant.Table)
		}
	}

	for _, policy := range policies {
		if policy.Role != store.ControllerRole {
			continue
		}
		grant := grantFor(grants, store.ControllerRole, policy.Table)
		if grant == nil || !operationSetContains(grant.Operations, store.Operation(policy.Command)) {
			t.Errorf("controller policy %q %q has no matching grant", policy.Table, policy.Command)
		}
		if !stringSliceContains(writeTables, policy.Table) {
			t.Errorf("controller policy %q %q targets a table outside the write set", policy.Table, policy.Command)
		}
		if policy.Command == store.PolicySelect && !stringSliceContains(readTables, policy.Table) {
			t.Errorf("controller SELECT policy on %q is outside the read set", policy.Table)
		}
	}

	insertOnly := store.ControllerInsertOnlyTables()
	if !sameStringSet(insertOnly, []string{"events", "audit"}) {
		t.Errorf("ControllerInsertOnlyTables = %v; want events,audit", insertOnly)
	}
	for _, table := range insertOnly {
		if !stringSliceContains(writeTables, table) {
			t.Errorf("insert-only table %q is not a controller write table", table)
		}
		grant := grantFor(grants, store.ControllerRole, table)
		if grant == nil || !operationSetContains(grant.Operations, store.OperationInsert) {
			t.Errorf("insert-only table %q has no INSERT grant", table)
		}
		if policyFor(policies, store.ControllerRole, table, store.PolicyInsert) == nil {
			t.Errorf("insert-only table %q has no INSERT policy", table)
		}
		for _, command := range []store.PolicyCommand{store.PolicyUpdate, store.PolicyDelete} {
			if policyFor(policies, store.ControllerRole, table, command) != nil {
				t.Errorf("insert-only table %q has a %s policy", table, command)
			}
			if grant != nil && operationSetContains(grant.Operations, store.Operation(command)) {
				t.Errorf("insert-only table %q has a %s grant", table, command)
			}
		}
	}
	if stringSliceContains(readTables, "audit") {
		t.Error("audit is write-only and must not be in the controller read set")
	}
}

func operationSetContains(operations []store.Operation, want store.Operation) bool {
	for _, operation := range operations {
		if operation == want {
			return true
		}
	}
	return false
}

func grantFor(grants []store.Grant, role, table string) *store.Grant {
	for index := range grants {
		if grants[index].Role == role && grants[index].Table == table {
			return &grants[index]
		}
	}
	return nil
}

func policyFor(policies []store.Policy, role, table string, command store.PolicyCommand) *store.Policy {
	for index := range policies {
		if policies[index].Role == role && policies[index].Table == table && policies[index].Command == command {
			return &policies[index]
		}
	}
	return nil
}

func loadControllerEventVectors(t *testing.T) controllerEventDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "controller_events.json"))
	if err != nil {
		t.Fatalf("read controller event vectors: %v", err)
	}
	var document controllerEventDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode controller event vectors: %v", err)
	}
	return document
}

func validateControllerEventVectors(t *testing.T, document controllerEventDocument) {
	t.Helper()
	if document.Version != 1 {
		t.Fatalf("controller event vector version = %d; want 1", document.Version)
	}
	if len(document.Vectors) == 0 {
		t.Fatal("controller event vector set is empty")
	}
	seen := make(map[string]bool, len(document.Vectors))
	successEvents := false
	foreignEvent := false
	foreignAudit := false
	receiptRefused := false
	eventMutationRefused := false
	staleVersion := false
	missingGuard := false
	for _, vector := range document.Vectors {
		if vector.Name == "" {
			t.Errorf("controller event vector has no name: %+v", vector)
			continue
		}
		if seen[vector.Name] {
			t.Errorf("duplicate controller event vector %q", vector.Name)
		}
		seen[vector.Name] = true
		if len(vector.Steps) == 0 {
			t.Errorf("controller event vector %q has no steps", vector.Name)
			continue
		}
		for index, step := range vector.Steps {
			switch step.Op {
			case "execute":
				if step.ExecutionID == "" {
					t.Errorf("vector %q step %d executes without an execution id", vector.Name, index)
				}
			case "inspect", "scan":
				if step.Table == "" {
					t.Errorf("vector %q step %d reads without a table", vector.Name, index)
				}
			default:
				t.Errorf("vector %q step %d has unknown op %q", vector.Name, index, step.Op)
			}
			if step.WantError != "" && !seamErrorClasses[step.WantError] {
				t.Errorf("vector %q step %d expects unknown error class %q", vector.Name, index, step.WantError)
			}
			if step.Op != "execute" {
				continue
			}
			if step.WantError == "" && len(step.Events) > 0 && len(step.Audit) > 0 {
				successEvents = true
			}
			if step.WantError == "forbidden" && len(step.Events) > 0 && len(step.Audit) == 0 {
				foreignEvent = true
			}
			if step.WantError == "forbidden" && len(step.Audit) > 0 && len(step.Events) == 0 {
				foreignAudit = true
			}
			if step.WantError == "version_conflict" && len(step.Events) > 0 {
				staleVersion = true
			}
			if step.Receipt != nil && step.WantError == "forbidden" {
				receiptRefused = true
			}
			for _, write := range step.Writes {
				if (write.Table == "events" || write.Table == "audit") && !write.Insert && step.WantError == "forbidden" {
					eventMutationRefused = true
				}
			}
			if len(step.Events) > 0 && len(step.Locks) == 0 && step.WantError == "forbidden" {
				missingGuard = true
			}
		}
	}
	required := []struct {
		ok   bool
		what string
	}{
		{successEvents, "a successful engine unit carrying lifecycle events and audit rows"},
		{foreignEvent, "a refused foreign-scope event row"},
		{foreignAudit, "a refused foreign-scope audit row"},
		{receiptRefused, "a refused controller receipt"},
		{eventMutationRefused, "a refused event/audit update or delete"},
		{staleVersion, "a stale execution version that refuses the event commit"},
		{missingGuard, "an event unit with no execution lock/guard"},
	}
	for _, want := range required {
		if !want.ok {
			t.Errorf("controller event vectors never cover %s", want.what)
		}
	}
}

func runControllerEventVector(vector controllerEventVector, controller store.Controller) error {
	for index, step := range vector.Steps {
		switch step.Op {
		case "execute":
			unit, err := controllerEventUnit(step)
			if err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
			_, err = controller.Execute(context.Background(), step.ExecutionID, unit)
			if err := checkSeamError(index, seamStep{WantError: step.WantError}, err); err != nil {
				return err
			}
		case "scan":
			scan := store.ControllerScan{ScanQuery: store.ScanQuery{
				Table: step.Table, Equals: step.Equals, Range: scannerRangeFilters(step.Range),
				OrderBy: scannerOrderTerms(step.OrderBy), Limit: step.Limit,
			}}
			page, err := controller.Scan(context.Background(), scan)
			if err := checkSeamError(index, seamStep{WantError: step.WantError}, err); err != nil {
				return err
			}
			if step.WantError != "" {
				continue
			}
			got := make([]string, 0, len(page.Rows))
			for _, row := range page.Rows {
				got = append(got, scannerString(row.Values["id"]))
			}
			if step.WantIDs != nil && !equalStrings(got, step.WantIDs) {
				return fmt.Errorf("step %d ids = %v; want %v", index, got, step.WantIDs)
			}
		case "inspect":
			if step.Scope == nil {
				return fmt.Errorf("step %d inspects without a scope", index)
			}
			inspector, ok := controller.(interface {
				inspectRow(seamScope, string, string) *seamRow
			})
			if !ok {
				return fmt.Errorf("step %d: controller cannot be inspected", index)
			}
			row := inspector.inspectRow(*step.Scope, step.Table, step.ID)
			if row == nil {
				if step.WantAbsent {
					continue
				}
				return fmt.Errorf("step %d row %s/%s is absent; want present", index, step.Table, step.ID)
			}
			if step.WantAbsent {
				return fmt.Errorf("step %d row %s/%s is present; want absent", index, step.Table, step.ID)
			}
			expected := seamStep{
				Table:       step.Table,
				ID:          step.ID,
				WantValues:  step.WantValues,
				WantVersion: step.WantVersion,
			}
			if err := checkSeamRow(expected, store.Row{Table: step.Table, Values: row.values}, true); err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
		default:
			return fmt.Errorf("step %d has unknown controller op %q", index, step.Op)
		}
	}
	return nil
}

func controllerEventUnit(step controllerEventStep) (store.Unit, error) {
	unit := store.Unit{ReceiptMode: store.ReceiptMode(step.ReceiptMode)}
	for _, lock := range step.Locks {
		converted := store.Lock{
			Table:         lock.Table,
			IDs:           append([]string(nil), lock.IDs...),
			ExpectVersion: lock.ExpectVersion,
			Expect:        lock.Expect,
			TenantWide:    lock.TenantWide,
		}
		if lock.Deadline != "" {
			converted.Deadline = &store.DeadlineGuard{Column: lock.Deadline}
		}
		unit.Locks = append(unit.Locks, converted)
	}
	for _, write := range step.Writes {
		converted := store.Write{
			Table:         write.Table,
			ID:            write.ID,
			Insert:        write.Insert,
			Delete:        write.Delete,
			ExpectVersion: write.ExpectVersion,
			Expect:        write.Expect,
			Values:        write.Values,
		}
		if write.Deadline != "" {
			converted.Deadline = &store.DeadlineGuard{Column: write.Deadline}
		}
		unit.Writes = append(unit.Writes, converted)
	}
	for _, spec := range step.Events {
		unit.Events = append(unit.Events, controllerEnvelope(spec))
	}
	for _, spec := range step.Audit {
		unit.Audit = append(unit.Audit, controllerAudit(spec))
	}
	if step.Receipt != nil {
		unit.Receipt = &receipt.Record{
			Key: receipt.Key{
				TenantID:    id.TenantID(step.Receipt.TenantID),
				ProjectID:   id.ProjectID(step.Receipt.ProjectID),
				PrincipalID: id.PrincipalID(step.Receipt.PrincipalID),
				Operation:   step.Receipt.Operation,
				CallerKey:   step.Receipt.CallerKey,
			},
			ContentHash: step.Receipt.ContentHash,
			Result:      []byte(step.Receipt.Result),
			ExecutionID: id.ExecutionID(step.Receipt.ExecutionID),
		}
	}
	if step.ReceiptExpect != nil {
		unit.ReceiptExpect = &store.ReceiptExpect{
			Completed:   step.ReceiptExpect.Completed,
			ContentHash: step.ReceiptExpect.ContentHash,
		}
	}
	return unit, nil
}

func controllerEnvelope(spec controllerEventSpec) event.Envelope {
	source := event.SystemSource
	if spec.SourceKind != "" {
		source.Kind = event.SourceKind(spec.SourceKind)
	}
	if spec.SourcePrincipal != "" {
		source.PrincipalID = id.PrincipalID(spec.SourcePrincipal)
	}
	scope := seamScope{}
	if spec.Scope != nil {
		scope = *spec.Scope
	}
	return event.Envelope{
		ID:             id.EventID(spec.ID),
		Type:           event.Type(spec.Type),
		ReportType:     event.ReportType(spec.ReportType),
		Source:         source,
		At:             seamClock(spec.At),
		Scope:          seamScopeOf(scope),
		SubjectID:      spec.SubjectID,
		SubjectVersion: spec.SubjectVersion,
		CorrelationID:  spec.CorrelationID,
		CausationID:    spec.CausationID,
		Data:           []byte(spec.Data),
	}
}

func controllerAudit(spec controllerAuditSpec) event.Audit {
	scope := seamScope{}
	if spec.Scope != nil {
		scope = *spec.Scope
	}
	return event.Audit{
		ID:        id.EventID(spec.ID),
		Scope:     seamScopeOf(scope),
		Actor:     id.PrincipalID(spec.Actor),
		Operation: spec.Operation,
		At:        seamClock(spec.At),
		Detail:    spec.Detail,
	}
}

// referenceEventController is the deterministic reference controller for the
// controller event vectors. It derives the owning scope from the execution
// row, requires a guarded execution lock for event/audit rows, refuses a
// foreign scope before staging anything, and inserts event/audit rows only
// through the unit's Events/Audit slices.
type referenceEventController struct {
	*referenceSeams
	eventSequence int64
}

func newReferenceEventController(seed seamSeed, clock string) *referenceEventController {
	return &referenceEventController{referenceSeams: newReferenceSeams(seed, clock)}
}

func (controller *referenceEventController) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	if err := ctx.Err(); err != nil {
		return store.UnitResult{}, err
	}
	if unit.Receipt != nil || unit.ReceiptExpect != nil || unit.ReceiptMode != store.ReceiptInsert {
		return store.UnitResult{}, fault.New(fault.Forbidden, "the controller cannot commit a receipt")
	}
	execution := controller.findRow("executions", executionID)
	if execution == nil {
		return store.UnitResult{}, fault.New(fault.NotFound, "controller execution is missing")
	}
	scope := tenancy.Scope{TenantID: id.TenantID(execution.tenant), ProjectID: id.ProjectID(execution.project)}
	if err := unit.Validate(); err != nil {
		return store.UnitResult{}, err
	}
	for _, lock := range unit.Locks {
		if !seamControllerTable(lock.Table, true) {
			return store.UnitResult{}, fault.New(fault.Forbidden, fmt.Sprintf("controller lock table %q is outside the controller authority", lock.Table))
		}
	}
	for _, write := range unit.Writes {
		if !seamControllerTable(write.Table, true) {
			return store.UnitResult{}, fault.New(fault.Forbidden, fmt.Sprintf("controller write table %q is outside the controller authority", write.Table))
		}
		if controllerInsertOnly(write.Table) && !write.Insert {
			return store.UnitResult{}, fault.New(fault.Forbidden, fmt.Sprintf("controller table %q is insert-only", write.Table))
		}
	}
	if len(unit.Events) > 0 || len(unit.Audit) > 0 {
		if !controllerExecutionGuarded(unit, executionID) {
			return store.UnitResult{}, fault.New(fault.Forbidden, "an event or audit unit requires a guarded execution lock")
		}
		if len(unit.Events) > 0 && !seamControllerTable("events", true) {
			return store.UnitResult{}, fault.New(fault.Forbidden, "controller table \"events\" is outside the controller authority")
		}
		if len(unit.Audit) > 0 && !seamControllerTable("audit", true) {
			return store.UnitResult{}, fault.New(fault.Forbidden, "controller table \"audit\" is outside the controller authority")
		}
		for _, envelope := range unit.Events {
			if err := envelope.Validate(); err != nil {
				return store.UnitResult{}, err
			}
			if !sameTenancyScope(envelope.Scope, scope) {
				return store.UnitResult{}, fault.New(fault.Forbidden, "an event row is outside the derived execution scope")
			}
		}
		for _, audit := range unit.Audit {
			if !sameTenancyScope(audit.Scope, scope) {
				return store.UnitResult{}, fault.New(fault.Forbidden, "an audit row is outside the derived execution scope")
			}
		}
	}

	for _, lock := range unit.Locks {
		for _, rowID := range lock.IDs {
			key := seamRowKey(lock.Table, string(scope.TenantID), string(scope.ProjectID), rowID)
			row := controller.rows[key]
			if row == nil {
				if controller.crossScopeRow(lock.Table, scope, rowID) {
					return store.UnitResult{}, fault.New(fault.Forbidden, "a locked row is outside the owning scope")
				}
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a locked row is missing or out of scope")
			}
			if !seamRowInScope(row, scope, lock.TenantWide) {
				return store.UnitResult{}, fault.New(fault.Forbidden, "a locked row is outside the owning scope")
			}
			if lock.ExpectVersion != 0 && row.version != lock.ExpectVersion {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a locked row was not at the expected version")
			}
			if !seamValuesMatch(row.values, lock.Expect) {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a locked row no longer matches its expected values")
			}
		}
	}

	staged := make(map[string]*seamRow)
	deleted := make(map[string]bool)
	for _, write := range unit.Writes {
		if err := controller.stageWrite(scope, write, staged, deleted); err != nil {
			return store.UnitResult{}, err
		}
	}
	for _, envelope := range unit.Events {
		key := seamRowKey("events", string(scope.TenantID), string(scope.ProjectID), string(envelope.ID))
		if controller.rows[key] != nil || staged[key] != nil {
			return store.UnitResult{}, fault.New(fault.VersionConflict, "an event row already exists")
		}
		controller.eventSequence++
		values := map[string]any{
			"id":               string(envelope.ID),
			"tenant_id":        string(scope.TenantID),
			"project_id":       string(scope.ProjectID),
			"type":             string(envelope.Type),
			"source_kind":      string(envelope.Source.Kind),
			"source_principal": string(envelope.Source.PrincipalID),
			"at":               envelope.At,
			"subject_id":       envelope.SubjectID,
			"subject_version":  envelope.SubjectVersion,
			"data":             envelope.Data,
			"publish_state":    "pending",
			"publish_sequence": controller.eventSequence,
		}
		if envelope.ReportType != "" {
			values["report_type"] = string(envelope.ReportType)
		}
		if envelope.CorrelationID != "" {
			values["correlation_id"] = envelope.CorrelationID
		}
		if envelope.CausationID != "" {
			values["causation_id"] = envelope.CausationID
		}
		staged[key] = &seamRow{table: "events", tenant: string(scope.TenantID), project: string(scope.ProjectID), id: string(envelope.ID), version: 1, values: values}
	}
	for _, audit := range unit.Audit {
		key := seamRowKey("audit", string(scope.TenantID), string(scope.ProjectID), string(audit.ID))
		if controller.rows[key] != nil || staged[key] != nil {
			return store.UnitResult{}, fault.New(fault.VersionConflict, "an audit row already exists")
		}
		values := map[string]any{
			"id":         string(audit.ID),
			"tenant_id":  string(scope.TenantID),
			"project_id": string(scope.ProjectID),
			"actor":      string(audit.Actor),
			"operation":  audit.Operation,
			"at":         audit.At,
		}
		if audit.Detail != "" {
			values["detail"] = audit.Detail
		}
		staged[key] = &seamRow{table: "audit", tenant: string(scope.TenantID), project: string(scope.ProjectID), id: string(audit.ID), version: 1, values: values}
	}

	for key, row := range staged {
		controller.rows[key] = row
	}
	for key := range deleted {
		delete(controller.rows, key)
	}
	return store.UnitResult{}, nil
}

func (controller *referenceEventController) stageWrite(scope tenancy.Scope, write store.Write, staged map[string]*seamRow, deleted map[string]bool) error {
	key := seamRowKey(write.Table, string(scope.TenantID), string(scope.ProjectID), write.ID)
	current := func() *seamRow {
		if row, ok := staged[key]; ok {
			return row
		}
		if deleted[key] {
			return nil
		}
		return controller.rows[key]
	}
	switch {
	case write.Insert:
		if write.ExpectVersion != 0 || len(write.Expect) > 0 {
			return fault.New(fault.InvalidInput, "an insert cannot carry an expected version or values")
		}
		if current() != nil {
			return fault.New(fault.VersionConflict, "an inserted row already exists")
		}
		values := make(map[string]any, len(write.Values))
		version := uint64(1)
		for column, value := range write.Values {
			switch column {
			case "id", "tenant_id", "project_id":
				return fault.New(fault.InvalidInput, fmt.Sprintf("column %q is supplied by the adapter", column))
			case "version":
				version = seamUint64(value)
			default:
				values[column] = seamNormalizeValue(write.Table, column, value)
			}
		}
		values["id"] = write.ID
		values["tenant_id"] = string(scope.TenantID)
		values["project_id"] = string(scope.ProjectID)
		values["version"] = version
		staged[key] = &seamRow{table: write.Table, tenant: string(scope.TenantID), project: string(scope.ProjectID), id: write.ID, version: version, values: values}
	case write.Delete:
		row := current()
		if row == nil {
			return fault.New(fault.VersionConflict, "a deleted row is missing or out of scope")
		}
		if !seamRowInScope(row, scope, false) {
			return fault.New(fault.Forbidden, "a deleted row is outside the owning scope")
		}
		if write.ExpectVersion != 0 && row.version != write.ExpectVersion {
			return fault.New(fault.VersionConflict, "a deleted row was not at the expected version")
		}
		if !seamValuesMatch(row.values, write.Expect) {
			return fault.New(fault.VersionConflict, "a deleted row no longer matches its expected values")
		}
		deleted[key] = true
	default:
		row := current()
		if row == nil {
			return fault.New(fault.VersionConflict, "an updated row is missing or out of scope")
		}
		if !seamRowInScope(row, scope, false) {
			return fault.New(fault.Forbidden, "an updated row is outside the owning scope")
		}
		if write.ExpectVersion != 0 && row.version != write.ExpectVersion {
			return fault.New(fault.VersionConflict, "an updated row was not at the expected version")
		}
		if !seamValuesMatch(row.values, write.Expect) {
			return fault.New(fault.VersionConflict, "an updated row no longer matches its expected values")
		}
		updated := *row
		updated.values = cloneScannerValues(row.values)
		for column, value := range write.Values {
			switch column {
			case "id", "tenant_id", "project_id":
				return fault.New(fault.InvalidInput, fmt.Sprintf("column %q is supplied by the adapter", column))
			case "version":
				return fault.New(fault.InvalidInput, "the adapter advances the version column")
			default:
				updated.values[column] = seamNormalizeValue(write.Table, column, value)
			}
		}
		updated.version++
		updated.values["version"] = updated.version
		staged[key] = &updated
	}
	return nil
}

// brokenEventController supplies the missing execution guard, so it accepts
// the event-without-guard vector and proves that vector is not vacuous.
type brokenEventController struct {
	*referenceEventController
}

func (controller brokenEventController) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	if len(unit.Events) > 0 || len(unit.Audit) > 0 {
		unit.Locks = append(unit.Locks, store.Lock{Table: "executions", IDs: []string{executionID}})
	}
	return controller.referenceEventController.Execute(ctx, executionID, unit)
}

func controllerExecutionGuarded(unit store.Unit, executionID string) bool {
	for _, lock := range unit.Locks {
		if lock.Table != "executions" {
			continue
		}
		for _, rowID := range lock.IDs {
			if rowID == executionID {
				return true
			}
		}
	}
	return false
}

// controllerInsertOnly derives the insert-only event/audit tables from the
// declared controller grants: a table is insert-only when the controller holds
// INSERT but no UPDATE. The contract test pins the grants themselves.
func controllerInsertOnly(table string) bool {
	grant := grantFor(store.ControllerGrants(), store.ControllerRole, table)
	if grant == nil {
		return false
	}
	hasInsert := false
	hasUpdate := false
	for _, operation := range grant.Operations {
		switch operation {
		case store.OperationInsert:
			hasInsert = true
		case store.OperationUpdate:
			hasUpdate = true
		}
	}
	return hasInsert && !hasUpdate
}

func sameTenancyScope(left, right tenancy.Scope) bool {
	return left.TenantID == right.TenantID && left.ProjectID == right.ProjectID
}
