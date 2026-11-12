// Declared uniqueness: the memory adapter mirrors the schema's unique
// constraints and predicate-free unique indexes at the point PostgreSQL checks
// a constraint that is not deferrable, after each insert and update.
package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func pipelineNamed(name string) map[string]any {
	values := insertValues("pipelines")
	values["name"] = name
	return values
}

func serviceNamed(name string) map[string]any {
	values := insertValues("services")
	values["name"] = name
	return values
}

func actionAt(execution string, index int64, name string) map[string]any {
	values := actionValues(1, "pending")
	values["execution_id"] = execution
	values["index"] = index
	values["name"] = name
	return values
}

func insertWrite(table, identity string, values map[string]any) store.Write {
	return store.Write{Table: table, ID: identity, Insert: true, Values: values}
}

func renamePipeline(identity, name string, version uint64) store.Write {
	return store.Write{Table: "pipelines", ID: identity, ExpectVersion: version, Values: map[string]any{"name": name}}
}

func deletePipeline(identity string, version uint64) store.Write {
	return store.Write{Table: "pipelines", ID: identity, Delete: true, ExpectVersion: version}
}

func executeWrites(s *Store, scope tenancy.Scope, writes ...store.Write) error {
	_, err := s.Execute(context.Background(), scope, store.Unit{Writes: writes})
	return err
}

// assertUniqueViolation requires a version_conflict that names the violated
// constraint and none of the given row values.
func assertUniqueViolation(t *testing.T, err error, constraint string, values ...string) {
	t.Helper()
	assertFault(t, err, fault.VersionConflict)
	if !strings.Contains(err.Error(), constraint) {
		t.Fatalf("violation %q does not name the constraint %q", err, constraint)
	}
	for _, value := range values {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("violation %q leaks the row value %q", err, value)
		}
	}
}

func pipelineName(t *testing.T, s *Store, scope tenancy.Scope, identity string) string {
	t.Helper()
	return rowString(t, mustSnapshot(t, s, scope, "pipelines", identity), "name")
}

// TestUniqueRulesAreDerivedFromTheDeclaredSchema pins which declared rules the
// adapter checks per write, which the primary key already implies, which the
// publication records hold and which it leaves unenforced, and proves every one
// is accounted for from the declaration.
func TestUniqueRulesAreDerivedFromTheDeclaredSchema(t *testing.T) {
	var checked, implied, publication, partial []string
	for _, table := range store.SchemaDesign().Tables {
		// Receipts, audit rows and events are committed through their
		// publication records, never as business inserts: the receipt key index
		// and the publication counter hold their rules.
		published := store.IsPublicationTable(table.Name) || table.Name == "events"
		meta, ok := lookupTable(table.Name)
		if !ok {
			t.Fatalf("table %q is missing from the schema index", table.Name)
		}
		derived := make(map[string]uniqueRule, len(meta.unique))
		for _, rule := range meta.unique {
			derived[rule.name] = rule
		}
		for _, declared := range table.Unique {
			rule, found := derived[declared.Name]
			if impliedByPrimaryKey(table.PrimaryKey, declared.Columns) {
				if found {
					t.Errorf("%s is implied by the primary key but is checked per write", declared.Name)
				}
				implied = append(implied, declared.Name)
				continue
			}
			if !found {
				t.Errorf("declared unique constraint %s is not enforced", declared.Name)
				continue
			}
			if !slices.Equal(rule.columns, declared.Columns) || rule.nullsNotDistinct != declared.NullsNotDistinct {
				t.Errorf("rule %s = %+v; want the declared %+v", declared.Name, rule, declared)
			}
			if published {
				publication = append(publication, declared.Name)
				continue
			}
			checked = append(checked, declared.Name)
		}
		for _, index := range table.Indexes {
			if !index.Unique {
				continue
			}
			_, found := derived[index.Name]
			if index.Predicate != "" {
				if found {
					t.Errorf("partial unique index %s must not be enforced", index.Name)
				}
				partial = append(partial, index.Name)
				continue
			}
			if !found && !impliedByPrimaryKey(table.PrimaryKey, index.Columns) {
				t.Errorf("declared unique index %s is not enforced", index.Name)
			}
			checked = append(checked, index.Name)
		}
	}
	sort.Strings(checked)
	sort.Strings(implied)
	sort.Strings(publication)
	sort.Strings(partial)
	wantChecked := []string{
		"actions_execution_index", "actions_execution_name",
		"memberships_scope_principal", "pipelines_scope_name", "projects_tenant_name",
		"resource_bindings_scope_resource_service", "secrets_scope_name", "services_scope_name",
	}
	wantPublication := []string{"events_publish_sequence", "receipts_identity"}
	wantImplied := []string{
		"actions_scope_id", "content_records_scope_id", "event_consumptions_scope_id",
		"executions_scope_id", "model_usage_scope_id", "pipelines_scope_id",
		"projects_tenant_id", "resource_bindings_scope_id", "resources_scope_id",
		"services_tenant_id", "tasks_scope_id",
	}
	wantPartial := []string{"actions_one_running", "receipts_resource_report_dedupe"}
	if !slices.Equal(checked, wantChecked) {
		t.Errorf("rules checked per write = %v; want %v", checked, wantChecked)
	}
	if !slices.Equal(implied, wantImplied) {
		t.Errorf("rules implied by a primary key = %v; want %v", implied, wantImplied)
	}
	if !slices.Equal(publication, wantPublication) {
		t.Errorf("rules held by the publication records = %v; want %v", publication, wantPublication)
	}
	if !slices.Equal(partial, wantPartial) {
		t.Errorf("partial unique indexes = %v; want %v", partial, wantPartial)
	}
}

func TestUniqueDuplicatePipelineNameIsRefusedAndWritesNothing(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "pipelines", "pipeline-a", pipelineNamed("build"))

	record := defaultReceipt(t)
	_, err := s.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{
			insertWrite("tasks", testTaskID, taskValues(1, "open")),
			insertWrite("pipelines", "pipeline-b", pipelineNamed("build")),
		},
		Receipt: &record,
		Audit:   []event.Audit{defaultAudit(scope, "audit-1", "pipeline.apply")},
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeConfigChanged, "pipeline-b")},
	})
	assertUniqueViolation(t, err, "pipelines_scope_name", "build", "pipeline-a", "pipeline-b")

	assertAbsent(t, s, scope, "pipelines", "pipeline-b")
	assertAbsent(t, s, scope, "tasks", testTaskID)
	if _, found := s.Receipt(record.Key); found {
		t.Error("a refused unit left its receipt behind")
	}
	if events := s.Events(scope); len(events) != 0 {
		t.Errorf("a refused unit left %d events behind", len(events))
	}
	if audits := s.Audit(scope); len(audits) != 0 {
		t.Errorf("a refused unit left %d audit rows behind", len(audits))
	}
	if got := pipelineName(t, s, scope, "pipeline-a"); got != "build" {
		t.Errorf("the existing pipeline name = %q; want build", got)
	}

	// The refusal consumed no publication sequence and the same receipt, audit
	// and event identities commit with a free name.
	mustExecute(t, s, scope, store.Unit{
		Writes:  []store.Write{insertWrite("pipelines", "pipeline-b", pipelineNamed("deploy"))},
		Receipt: &record,
		Audit:   []event.Audit{defaultAudit(scope, "audit-1", "pipeline.apply")},
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeConfigChanged, "pipeline-b")},
	})
	if events := s.Events(scope); len(events) != 1 || events[0].Sequence != 1 {
		t.Errorf("committed events = %+v; want one event at sequence 1", events)
	}
}

func TestUniquePipelineNameIsScopedToItsProject(t *testing.T) {
	s := New()
	seedRow(t, s, projectScope(), "pipelines", "pipeline-a", pipelineNamed("build"))

	if err := executeWrites(s, otherProjectScope(), insertWrite("pipelines", "pipeline-b", pipelineNamed("build"))); err != nil {
		t.Fatalf("the same name in another project: %v", err)
	}
	if err := executeWrites(s, scopeFor(otherTenantID, testProjectID), insertWrite("pipelines", "pipeline-c", pipelineNamed("build"))); err != nil {
		t.Fatalf("the same name in another tenant: %v", err)
	}
	err := executeWrites(s, otherProjectScope(), insertWrite("pipelines", "pipeline-d", pipelineNamed("build")))
	assertUniqueViolation(t, err, "pipelines_scope_name")
}

func TestUniqueRenameOntoAnExistingNameIsRefused(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "pipelines", "pipeline-a", pipelineNamed("build"))
	seedRow(t, s, scope, "pipelines", "pipeline-b", pipelineNamed("deploy"))

	err := executeWrites(s, scope, renamePipeline("pipeline-b", "build", 1))
	assertUniqueViolation(t, err, "pipelines_scope_name", "build")
	row := mustSnapshot(t, s, scope, "pipelines", "pipeline-b")
	if rowString(t, row, "name") != "deploy" || rowVersion(t, row, "version") != 1 {
		t.Fatalf("a refused rename changed the row: %v", row.Values)
	}

	// Rewriting a row's own name and moving to a free name both commit.
	if err := executeWrites(s, scope, renamePipeline("pipeline-b", "deploy", 1)); err != nil {
		t.Fatalf("rewriting a row's own name: %v", err)
	}
	if err := executeWrites(s, scope, renamePipeline("pipeline-b", "release", 2)); err != nil {
		t.Fatalf("renaming to a free name: %v", err)
	}
	// An update that sets no constrained column is not a uniqueness question.
	if err := executeWrites(s, scope, store.Write{
		Table: "pipelines", ID: "pipeline-a", ExpectVersion: 1, Values: map[string]any{"yaml": []byte("name: build\n")},
	}); err != nil {
		t.Fatalf("an update of an unconstrained column: %v", err)
	}
}

// TestUniqueFollowsTheUnitWriteOrder pins the PostgreSQL behavior for a
// constraint that is not deferrable: each statement is checked when it runs, so
// what a unit may do depends on its write order, not on the state it would
// leave behind.
func TestUniqueFollowsTheUnitWriteOrder(t *testing.T) {
	scope := projectScope()
	seeded := func(t *testing.T) *Store {
		t.Helper()
		s := New()
		seedRow(t, s, scope, "pipelines", "pipeline-a", pipelineNamed("build"))
		seedRow(t, s, scope, "pipelines", "pipeline-b", pipelineNamed("deploy"))
		return s
	}

	t.Run("delete then recreate in one unit", func(t *testing.T) {
		s := seeded(t)
		if err := executeWrites(s, scope,
			deletePipeline("pipeline-a", 1),
			insertWrite("pipelines", "pipeline-c", pipelineNamed("build")),
		); err != nil {
			t.Fatalf("delete then recreate: %v", err)
		}
		assertAbsent(t, s, scope, "pipelines", "pipeline-a")
		if got := pipelineName(t, s, scope, "pipeline-c"); got != "build" {
			t.Fatalf("recreated pipeline name = %q; want build", got)
		}
	})

	t.Run("delete then recreate in two units", func(t *testing.T) {
		s := seeded(t)
		if err := executeWrites(s, scope, deletePipeline("pipeline-a", 1)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := executeWrites(s, scope, insertWrite("pipelines", "pipeline-c", pipelineNamed("build"))); err != nil {
			t.Fatalf("recreate: %v", err)
		}
	})

	t.Run("recreate before the delete in one unit", func(t *testing.T) {
		s := seeded(t)
		err := executeWrites(s, scope,
			insertWrite("pipelines", "pipeline-c", pipelineNamed("build")),
			deletePipeline("pipeline-a", 1),
		)
		assertUniqueViolation(t, err, "pipelines_scope_name")
		assertAbsent(t, s, scope, "pipelines", "pipeline-c")
		if got := pipelineName(t, s, scope, "pipeline-a"); got != "build" {
			t.Fatalf("a refused unit deleted or changed pipeline-a: %q", got)
		}
	})

	t.Run("rename away then reuse the name in one unit", func(t *testing.T) {
		s := seeded(t)
		if err := executeWrites(s, scope,
			renamePipeline("pipeline-a", "legacy", 1),
			insertWrite("pipelines", "pipeline-c", pipelineNamed("build")),
		); err != nil {
			t.Fatalf("rename away then reuse: %v", err)
		}
	})

	t.Run("direct swap in one unit", func(t *testing.T) {
		s := seeded(t)
		err := executeWrites(s, scope,
			renamePipeline("pipeline-a", "deploy", 1),
			renamePipeline("pipeline-b", "build", 1),
		)
		assertUniqueViolation(t, err, "pipelines_scope_name")
		if a, b := pipelineName(t, s, scope, "pipeline-a"), pipelineName(t, s, scope, "pipeline-b"); a != "build" || b != "deploy" {
			t.Fatalf("a refused swap left names %q/%q; want build/deploy", a, b)
		}
	})

	t.Run("swap through a free name in one unit", func(t *testing.T) {
		s := seeded(t)
		if err := executeWrites(s, scope,
			renamePipeline("pipeline-a", "swap", 1),
			renamePipeline("pipeline-b", "build", 1),
			renamePipeline("pipeline-a", "deploy", 2),
		); err != nil {
			t.Fatalf("swap through a free name: %v", err)
		}
		if a, b := pipelineName(t, s, scope, "pipeline-a"), pipelineName(t, s, scope, "pipeline-b"); a != "deploy" || b != "build" {
			t.Fatalf("swapped names = %q/%q; want deploy/build", a, b)
		}
	})

	t.Run("two inserts of one name in one unit", func(t *testing.T) {
		s := seeded(t)
		err := executeWrites(s, scope,
			insertWrite("pipelines", "pipeline-c", pipelineNamed("release")),
			insertWrite("pipelines", "pipeline-d", pipelineNamed("release")),
		)
		assertUniqueViolation(t, err, "pipelines_scope_name")
		assertAbsent(t, s, scope, "pipelines", "pipeline-c")
		assertAbsent(t, s, scope, "pipelines", "pipeline-d")
	})
}

// TestUniqueNullsNotDistinct covers services_scope_name and
// memberships_scope_principal, both declared NULLS NOT DISTINCT over the
// nullable project_id: two tenant-wide rows carry NULL there and still conflict.
func TestUniqueNullsNotDistinct(t *testing.T) {
	s := New()
	seedRow(t, s, tenantScope(), "services", "service-a", serviceNamed("git"))

	err := executeWrites(s, tenantScope(), insertWrite("services", "service-b", serviceNamed("git")))
	assertUniqueViolation(t, err, "services_scope_name", "git")
	assertAbsent(t, s, tenantScope(), "services", "service-b")

	// NULL equals only NULL: a project row with the same name is another key.
	if err := executeWrites(s, projectScope(), insertWrite("services", "service-c", serviceNamed("git"))); err != nil {
		t.Fatalf("a project service named like a tenant-wide one: %v", err)
	}
	err = executeWrites(s, projectScope(), insertWrite("services", "service-d", serviceNamed("git")))
	assertUniqueViolation(t, err, "services_scope_name")

	seedRow(t, s, tenantScope(), "memberships", "membership-a", membershipValues(testPrincipalID))
	err = executeWrites(s, tenantScope(), insertWrite("memberships", "membership-b", membershipValues(testPrincipalID)))
	assertUniqueViolation(t, err, "memberships_scope_principal", testPrincipalID)
	if err := executeWrites(s, projectScope(), insertWrite("memberships", "membership-c", membershipValues(testPrincipalID))); err != nil {
		t.Fatalf("a project membership for a tenant-wide member: %v", err)
	}
}

// TestUniqueOrdinaryNullsAreDistinct covers pipelines_scope_name and
// actions_execution_name, declared without NULLS NOT DISTINCT: a row whose
// constrained column is NULL conflicts with nothing. No ordinary declared rule
// has a nullable column, so the NULL here is a name the writer omitted, which
// this adapter stores as NULL and PostgreSQL refuses as a NOT NULL violation.
func TestUniqueOrdinaryNullsAreDistinct(t *testing.T) {
	s := New()
	scope := projectScope()
	unnamed := func() map[string]any {
		values := insertValues("pipelines")
		delete(values, "name")
		return values
	}
	seedRow(t, s, scope, "pipelines", "pipeline-a", unnamed())
	if err := executeWrites(s, scope, insertWrite("pipelines", "pipeline-b", unnamed())); err != nil {
		t.Fatalf("a second row with a NULL name: %v", err)
	}
	// A NULL key conflicts with no named row either, and named rows still do.
	seedRow(t, s, scope, "pipelines", "pipeline-c", pipelineNamed("build"))
	err := executeWrites(s, scope, insertWrite("pipelines", "pipeline-d", pipelineNamed("build")))
	assertUniqueViolation(t, err, "pipelines_scope_name")

	unnamedAction := func(index int64) map[string]any {
		values := actionAt(testExecutionID, index, "")
		delete(values, "name")
		return values
	}
	seedRow(t, s, scope, "actions", "action-a", unnamedAction(0))
	if err := executeWrites(s, scope, insertWrite("actions", "action-b", unnamedAction(1))); err != nil {
		t.Fatalf("a second action with a NULL name: %v", err)
	}
}

// TestUniqueIsTableWideAcrossScopes covers a rule with no scope column:
// actions_execution_index compares rows of every tenant and project.
func TestUniqueIsTableWideAcrossScopes(t *testing.T) {
	s := New()
	seedRow(t, s, projectScope(), "actions", "action-a", actionAt(testExecutionID, 0, "build"))

	err := executeWrites(s, otherProjectScope(), insertWrite("actions", "action-b", actionAt(testExecutionID, 0, "test")))
	assertUniqueViolation(t, err, "actions_execution_index")
	err = executeWrites(s, scopeFor(otherTenantID, testProjectID), insertWrite("actions", "action-c", actionAt(testExecutionID, 1, "build")))
	assertUniqueViolation(t, err, "actions_execution_name", "build")
	if err := executeWrites(s, otherProjectScope(), insertWrite("actions", "action-d", actionAt(testExecutionID, 1, "test"))); err != nil {
		t.Fatalf("a distinct index and name under the same execution: %v", err)
	}
}

// TestUniqueRemainingDeclaredRules covers the checked rules the other tests do
// not: projects_tenant_name, resource_bindings_scope_resource_service and the
// NULLS NOT DISTINCT secrets_scope_name.
func TestUniqueRemainingDeclaredRules(t *testing.T) {
	s := New()
	seedRow(t, s, tenantScope(), "projects", "project-a", insertValues("projects"))
	err := executeWrites(s, tenantScope(), insertWrite("projects", "project-b", insertValues("projects")))
	assertUniqueViolation(t, err, "projects_tenant_name", "app")
	if err := executeWrites(s, scopeFor(otherTenantID, ""), insertWrite("projects", "project-c", insertValues("projects"))); err != nil {
		t.Fatalf("the same project name in another tenant: %v", err)
	}

	binding := func(resourceID, serviceID string) map[string]any {
		return map[string]any{"resource_id": resourceID, "service_id": serviceID, "version": uint64(1)}
	}
	seedRow(t, s, projectScope(), "resource_bindings", "binding-a", binding("repo-1", "service-1"))
	err = executeWrites(s, projectScope(), insertWrite("resource_bindings", "binding-b", binding("repo-1", "service-1")))
	assertUniqueViolation(t, err, "resource_bindings_scope_resource_service", "repo-1", "service-1")
	for identity, values := range map[string]map[string]any{
		"binding-c": binding("repo-1", "service-2"),
		"binding-d": binding("repo-2", "service-1"),
	} {
		if err := executeWrites(s, projectScope(), insertWrite("resource_bindings", identity, values)); err != nil {
			t.Fatalf("%s shares only one key column: %v", identity, err)
		}
	}
	err = executeWrites(s, projectScope(), store.Write{
		Table: "resource_bindings", ID: "binding-c", ExpectVersion: 1, Values: map[string]any{"service_id": "service-1"},
	})
	assertUniqueViolation(t, err, "resource_bindings_scope_resource_service")

	secret := map[string]any{"name": "deploy-key", "version": uint64(1)}
	seedRow(t, s, tenantScope(), "secrets", "secret-a", secret)
	err = executeWrites(s, tenantScope(), insertWrite("secrets", "secret-b", cloneValues(secret)))
	assertUniqueViolation(t, err, "secrets_scope_name", "deploy-key")
	if err := executeWrites(s, projectScope(), insertWrite("secrets", "secret-c", cloneValues(secret))); err != nil {
		t.Fatalf("a project secret named like a tenant-wide one: %v", err)
	}
}

func TestUniqueReceiptReplayIsNotAViolation(t *testing.T) {
	s := New()
	scope := projectScope()
	key := keyFor(testTenantID, testProjectID, testPrincipalID, "pipeline.apply", "apply-build")
	create := func(receiptID, rowID, content string) (store.UnitResult, error) {
		record := receiptRecord(t, receiptID, key, content, `{"pipeline":"pipeline-a"}`)
		return s.Execute(context.Background(), scope, store.Unit{
			Writes:  []store.Write{insertWrite("pipelines", rowID, pipelineNamed("build"))},
			Receipt: &record,
		})
	}
	first, err := create("receipt-1", "pipeline-a", `{"name":"build"}`)
	if err != nil || first.Replayed {
		t.Fatalf("first create = %+v, %v; want a fresh commit", first, err)
	}

	// The retry allocates a new row identity, as a service does, and names the
	// same pipeline. It replays the original result and inserts nothing.
	retried, err := create("receipt-2", "pipeline-b", `{"name":"build"}`)
	if err != nil {
		t.Fatalf("an equal retry was refused: %v", err)
	}
	if !retried.Replayed || retried.Receipt.ID != first.Receipt.ID {
		t.Fatalf("retry = %+v; want a replay of receipt %q", retried, first.Receipt.ID)
	}
	assertAbsent(t, s, scope, "pipelines", "pipeline-b")

	// Changed content under the same key keeps its own class.
	_, err = create("receipt-3", "pipeline-c", `{"name":"other"}`)
	assertFault(t, err, fault.ChangedIdempotencyContent)

	// A new request for the taken name is the violation, and stores no receipt.
	fresh := receiptRecord(t, "receipt-4",
		keyFor(testTenantID, testProjectID, testPrincipalID, "pipeline.apply", "apply-again"),
		`{"name":"build"}`, `{"pipeline":"pipeline-d"}`)
	_, err = s.Execute(context.Background(), scope, store.Unit{
		Writes:  []store.Write{insertWrite("pipelines", "pipeline-d", pipelineNamed("build"))},
		Receipt: &fresh,
	})
	assertUniqueViolation(t, err, "pipelines_scope_name")
	if _, found := s.Receipt(fresh.Key); found {
		t.Error("a refused create stored its receipt")
	}
}

func TestUniqueConcurrentDuplicateInsertsHaveOneWinner(t *testing.T) {
	const writers = 16
	s := New()
	scope := projectScope()
	errs := runConcurrently(t, writers, func(index int) error {
		return executeWrites(s, scope, insertWrite("pipelines", fmt.Sprintf("pipeline-%d", index), pipelineNamed("build")))
	})
	if countNil(errs) != 1 {
		t.Fatalf("successful inserts = %d; want exactly 1 (%v)", countNil(errs), describeErrors(errs))
	}
	if got := countFault(t, errs, fault.VersionConflict); got != writers-1 {
		t.Fatalf("version conflicts = %d; want %d (%v)", got, writers-1, describeErrors(errs))
	}
	stored := 0
	for index := range writers {
		if _, found := s.Snapshot(scope, "pipelines", fmt.Sprintf("pipeline-%d", index)); found {
			if errs[index] != nil {
				t.Errorf("writer %d was refused but its row is stored", index)
			}
			stored++
		}
	}
	if stored != 1 {
		t.Fatalf("stored rows named build = %d; want 1", stored)
	}
}

func TestUniqueControllerSeamIsChecked(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "running"))
	seedRow(t, s, scope, "actions", "action-a", actionAt(testExecutionID, 0, "build"))
	controller := s.Controller()
	insert := func(identity string, index int64, name string) error {
		_, err := controller.Execute(context.Background(), testExecutionID, store.Unit{
			Writes: []store.Write{
				{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "succeeded"}},
				insertWrite("actions", identity, actionAt(testExecutionID, index, name)),
			},
		})
		return err
	}
	assertUniqueViolation(t, insert("action-b", 0, "test"), "actions_execution_index")
	assertUniqueViolation(t, insert("action-b", 1, "build"), "actions_execution_name", "build")
	assertAbsent(t, s, scope, "actions", "action-b")
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if rowString(t, execution, "state") != "running" || rowVersion(t, execution, "version") != 1 {
		t.Fatalf("a refused controller unit changed the execution: %v", execution.Values)
	}
	if err := insert("action-b", 1, "test"); err != nil {
		t.Fatalf("a distinct action through the controller seam: %v", err)
	}
}

// TestUniqueIndexWithoutPredicateIsEnforced uses a synthetic table: the schema
// declares no unique index without a predicate, so the derivation and the
// check are exercised directly.
func TestUniqueIndexWithoutPredicateIsEnforced(t *testing.T) {
	table := store.Table{
		Name:       "widgets",
		PrimaryKey: []string{"id"},
		Indexes: []store.Index{
			{Name: "widgets_code", Unique: true, Columns: []string{"code"}},
			{Name: "widgets_one_active", Unique: true, Predicate: "state = 'active'", Columns: []string{"owner"}},
			{Name: "widgets_owner_lookup", Columns: []string{"owner"}},
			{Name: "widgets_id_code", Unique: true, Columns: []string{"id", "code"}},
		},
	}
	rules := uniqueRules(table)
	if len(rules) != 1 || rules[0].name != "widgets_code" || rules[0].nullsNotDistinct {
		t.Fatalf("derived rules = %+v; want only the ordinary widgets_code", rules)
	}

	meta := &tableMeta{table: table, unique: rules}
	staged := newState()
	staged.rows["widgets"] = map[string]map[string]any{
		"widget-a": {"id": "widget-a", "code": "alpha", "owner": "owner-1", "state": "active"},
		"widget-b": {"id": "widget-b", "code": "beta", "owner": "owner-1", "state": "active"},
		"widget-c": {"id": "widget-c", "code": nil, "owner": "owner-2", "state": "active"},
		"widget-d": {"id": "widget-d", "code": nil, "owner": "owner-2", "state": "active"},
	}
	written := func(identity string) plannedWrite {
		return plannedWrite{meta: meta, table: "widgets", identity: identity, insert: true}
	}
	// Equal owners under the partial index and NULL codes under the plain one
	// are all accepted.
	for identity := range staged.rows["widgets"] {
		if err := checkUnique(staged, written(identity)); err != nil {
			t.Fatalf("%s: %v", identity, err)
		}
	}
	staged.rows["widgets"]["widget-b"]["code"] = "alpha"
	assertUniqueViolation(t, checkUnique(staged, written("widget-b")), "widgets_code", "alpha")
	update := plannedWrite{meta: meta, table: "widgets", identity: "widget-b", values: map[string]any{"owner": "owner-3"}}
	if err := checkUnique(staged, update); err != nil {
		t.Fatalf("an update that sets no constrained column was checked: %v", err)
	}
	update.values = map[string]any{"code": "alpha"}
	assertUniqueViolation(t, checkUnique(staged, update), "widgets_code")
}

// TestUniquePartialIndexIsNotEnforced pins the documented gap: PostgreSQL
// refuses a second running action of one execution through actions_one_running,
// and this adapter does not evaluate that predicate.
func TestUniquePartialIndexIsNotEnforced(t *testing.T) {
	s := New()
	scope := projectScope()
	running := func(index int64, name string) map[string]any {
		values := actionAt(testExecutionID, index, name)
		values["state"] = "running"
		return values
	}
	seedRow(t, s, scope, "actions", "action-a", running(0, "build"))
	if err := executeWrites(s, scope, insertWrite("actions", "action-b", running(1, "test"))); err != nil {
		t.Fatalf("a second running action: %v", err)
	}
}
