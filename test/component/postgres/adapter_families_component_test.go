//go:build component

package postgrescomponent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
	"github.com/optimiware/kodama/internal/shell/verify"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// adapterScoped returns the runtime-role scoped store the product uses for
// non-controller adapters, plus the shard.
func adapterScoped(t *testing.T) (*pgtest.Shard, *storepg.Store) {
	t.Helper()
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	shard.SeedMembership(ctx, scope, "membership-1", "principal-1", 1, string(tenancy.RoleEngineer))
	runtime := shard.Runtime(4)
	t.Cleanup(runtime.Close)
	return shard, storepg.New(runtime)
}

// adapterController returns the non-owner controller seam.
func adapterController(t *testing.T, shard *pgtest.Shard) *storepg.Controller {
	t.Helper()
	db := shard.Role(store.ControllerRole, 4)
	t.Cleanup(db.Close)
	pgtest.RequireNonOwnerSession(t, context.Background(), db, store.ControllerRole)
	return storepg.NewController(db)
}

func TestBindingReaderAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedResource(ctx, scope, "resource-1")
	shard.SeedService(ctx, scope, "service-1", "enforcing")
	shard.SeedBinding(ctx, scope, "binding-1", "resource-1", "service-1")
	reader, err := readers.NewBindingReader(scoped)
	if err != nil {
		t.Fatalf("NewBindingReader: %v", err)
	}
	binding, err := reader.ReadBinding(ctx, verify.BindingQuery{
		Scope: scope, ResourceID: "resource-1", ServiceID: "service-1",
	})
	if err != nil {
		t.Fatalf("ReadBinding: %v%s", err, pgtest.CauseText(err))
	}
	if binding.ID != "binding-1" || binding.ResourceID != "resource-1" ||
		binding.EnforcingService != "service-1" || binding.Scope != scope || binding.Version != 1 {
		t.Fatalf("binding = %+v; want binding-1/resource-1/service-1/scope/version 1", binding)
	}
	// A binding in another project is not found through the caller's scope.
	other := pgtest.Scope("tenant-2", "project-2")
	shard.SeedResource(ctx, other, "resource-2")
	shard.SeedService(ctx, other, "service-2", "other")
	shard.SeedBinding(ctx, other, "binding-2", "resource-2", "service-2")
	if _, err := reader.ReadBinding(ctx, verify.BindingQuery{
		Scope: scope, ResourceID: "resource-2", ServiceID: "service-2",
	}); !adapterClass(err, fault.NotFound) {
		t.Fatalf("cross-scope binding error = %v; want not_found", err)
	}
	if _, err := reader.ReadBinding(ctx, verify.BindingQuery{
		Scope: scope, ResourceID: "resource-1", ServiceID: "service-2",
	}); !adapterClass(err, fault.NotFound) {
		t.Fatalf("wrong-service binding error = %v; want not_found", err)
	}
}

func TestCredentialValidityAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "running", 3, "", "")
	expiry := pgtest.FixtureTime.Add(37 * time.Minute)
	shard.SeedExecutionCredential(ctx, "execution-1", tokenNonce(1), expiry)
	shard.SeedService(ctx, scope, "service-1", "enforcing")
	shard.SeedServiceCredential(ctx, "service-1", tokenNonce(2), expiry.Add(5*time.Minute))

	reader, err := readers.NewCredentialValidity(scoped)
	if err != nil {
		t.Fatalf("NewCredentialValidity: %v", err)
	}
	execPrincipal := identity.Principal{
		ID: "principal-1", Kind: identity.KindExecution, Scope: scope,
		Enabled: true, Version: 3, ExecutionID: "execution-1",
	}
	got, err := reader.ValidUntil(ctx, execPrincipal)
	if err != nil {
		t.Fatalf("execution ValidUntil: %v%s", err, pgtest.CauseText(err))
	}
	if !got.Equal(expiry) {
		t.Fatalf("execution expiry = %s; want exactly %s", got, expiry)
	}
	// The stored execution principal owns the credential; a different principal
	// naming the same execution is refused.
	wrong := execPrincipal
	wrong.ID = "principal-2"
	if _, err := reader.ValidUntil(ctx, wrong); !adapterClass(err, fault.NotFound) {
		t.Fatalf("wrong-principal expiry error = %v; want not_found", err)
	}
	// An execution without an installed token pair fails closed.
	shard.SeedExecution(ctx, scope, "execution-2", "queued", 1, "", "")
	missing := execPrincipal
	missing.ExecutionID = "execution-2"
	if _, err := reader.ValidUntil(ctx, missing); !adapterClass(err, fault.NotFound) {
		t.Fatalf("absent expiry error = %v; want not_found", err)
	}
	// A human's token expiry is not store-backed.
	human := identity.Principal{ID: "principal-1", Kind: identity.KindHuman, Scope: scope, Enabled: true, Version: 1}
	if _, err := reader.ValidUntil(ctx, human); !adapterClass(err, fault.NotFound) {
		t.Fatalf("human expiry error = %v; want not_found", err)
	}
	// A service's current expiry is read from its own row.
	servicePrincipal := identity.Principal{ID: "service-1", Kind: identity.KindService, Scope: scope, Enabled: true, Version: 1}
	serviceExpiry, err := reader.ValidUntil(ctx, servicePrincipal)
	if err != nil || !serviceExpiry.Equal(expiry.Add(5*time.Minute)) {
		t.Fatalf("service expiry = %s, err=%v; want exactly %s", serviceExpiry, err, expiry.Add(5*time.Minute))
	}
}

func TestOwnershipReaderAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedTask(ctx, scope, "task-1", "open", 2, "")
	shard.SeedExecution(ctx, scope, "execution-1", "running", 2, "task-1", "")
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE tasks SET current_execution_id = 'execution-1' WHERE id = 'task-1'`); err != nil {
		t.Fatalf("set current execution: %v", err)
	}
	shard.SeedExecution(ctx, scope, "execution-2", "queued", 1, "", "")

	reader, err := readers.NewOwnershipReader(scoped)
	if err != nil {
		t.Fatalf("NewOwnershipReader: %v", err)
	}
	ownership, err := reader.Ownership(ctx, scope, "execution-1")
	if err != nil {
		t.Fatalf("Ownership: %v%s", err, pgtest.CauseText(err))
	}
	if ownership.TaskID != "task-1" || ownership.CurrentExecution != "execution-1" {
		t.Fatalf("ownership = %+v; want task-1/execution-1", ownership)
	}
	if _, err := reader.Ownership(ctx, scope, "execution-2"); !adapterClass(err, fault.NotFound) {
		t.Fatalf("task-free execution error = %v; want not_found", err)
	}
	if _, err := reader.Ownership(ctx, pgtest.Scope("tenant-2", "project-2"), "execution-1"); !adapterClass(err, fault.NotFound) {
		t.Fatalf("cross-scope ownership error = %v; want not_found", err)
	}
}

func TestVerificationReaderAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedResource(ctx, scope, "resource-1")
	shard.SeedService(ctx, scope, "service-1", "enforcing")
	shard.SeedBinding(ctx, scope, "binding-1", "resource-1", "service-1")
	shard.SeedVerification(ctx, scope, "verification-1", "binding-1", "resource-1")
	reader, err := readers.NewVerificationReader(scoped)
	if err != nil {
		t.Fatalf("NewVerificationReader: %v", err)
	}
	record, err := reader.ReadVerification(ctx, scope, "verification-1")
	if err != nil {
		t.Fatalf("ReadVerification: %v%s", err, pgtest.CauseText(err))
	}
	if record.ID != "verification-1" || record.Scope != scope || record.CallingService != "service-1" ||
		record.BindingID != "binding-1" || record.ResourceID != "resource-1" ||
		record.Operation != "git.ref.update" || record.Result != verify.ResultApproved ||
		!record.Approved || record.Actor.ID != "principal-1" || record.Actor.Kind != identity.KindHuman {
		t.Fatalf("verification = %+v; want exact durable fact", record)
	}
	if _, err := reader.ReadVerification(ctx, pgtest.Scope("tenant-2", "project-2"), "verification-1"); !adapterClass(err, fault.NotFound) {
		t.Fatalf("foreign-scope verification error = %v; want not_found", err)
	}
	if _, err := reader.ReadVerification(ctx, scope, "verification-missing"); !adapterClass(err, fault.NotFound) {
		t.Fatalf("absent verification error = %v; want not_found", err)
	}
}

func TestSecretLookupAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedSecret(ctx, scope, "secret-1", "DEPLOY_KEY", 4)
	lookup, err := platform.NewSecretLookup(scoped)
	if err != nil {
		t.Fatalf("NewSecretLookup: %v", err)
	}
	stored, err := lookup.Lookup(ctx, scope, "DEPLOY_KEY")
	if err != nil {
		t.Fatalf("Lookup: %v%s", err, pgtest.CauseText(err))
	}
	if stored.Metadata.ID != "secret-1" || stored.Metadata.Name != "DEPLOY_KEY" ||
		stored.Metadata.Scope != scope || stored.Metadata.Version != 4 {
		t.Fatalf("secret metadata = %+v; want secret-1/DEPLOY_KEY/scope/4", stored.Metadata)
	}
	if string(stored.Envelope.Ciphertext) != "ciphertext" || len(stored.Envelope.Nonce) != 12 ||
		stored.Envelope.Nonce[0] != 9 || len(stored.Envelope.WrappedKey) != 48 ||
		stored.Envelope.KeyRef != "kms/secret-key" || stored.Envelope.KeyVersion != 3 {
		t.Fatalf("secret envelope = %+v; want exact stored bytes", stored.Envelope)
	}
	if _, err := lookup.Lookup(ctx, scope, "ABSENT_NAME"); !adapterClass(err, fault.NotFound) {
		t.Fatalf("absent secret error = %v; want not_found", err)
	}
	if _, err := lookup.Lookup(ctx, pgtest.Scope("tenant-2", "project-2"), "DEPLOY_KEY"); !adapterClass(err, fault.NotFound) {
		t.Fatalf("cross-scope secret error = %v; want not_found", err)
	}
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "secrets", map[string]any{"id": "secret-1"}, map[string]any{
		"id": "secret-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"name": "DEPLOY_KEY", "ciphertext": []byte("ciphertext"),
		"wrapped_key": bytesOf(48, 7), "wrap_nonce": bytesOf(12, 8),
		"key_ref": "kms/secret-key", "key_version": int64(3), "nonce": bytesOf(12, 9),
		"version": int64(4), "created_at": pgtest.FixtureTime, "updated_at": pgtest.FixtureTime,
	})
}

func TestEvidenceReaderAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "running", 2, "", "")
	assignment := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer}
	assignmentBytes, _ := json.Marshal(assignment)
	subjectBytes := []byte(`{"ref":{"kind":"literal","literal":"refs/heads/main"},` +
		`"operation":{"kind":"literal","literal":"git.ref.update"},` +
		`"resource":{"kind":"literal","literal":"resource-1"},` +
		`"expected_base":{"kind":"literal","literal":"base-1"},` +
		`"candidate":{"kind":"literal","literal":"cand-1"}}`)
	shard.SeedRequestAction(ctx, scope, "action-1", "execution-1", "approve", "succeeded", 0, assignmentBytes, subjectBytes,
		"pass", "principal-1", pgtest.FixtureTime)
	shard.SeedRequestAction(ctx, scope, "action-2", "execution-1", "second", "succeeded", 1, assignmentBytes, subjectBytes,
		"fail", "principal-1", pgtest.FixtureTime)

	reader, err := platform.NewEvidenceReader(scoped)
	if err != nil {
		t.Fatalf("NewEvidenceReader: %v", err)
	}
	query := verify.EvidenceQuery{
		Scope: scope, ExecutionID: "execution-1", Assignment: assignment,
		Operation: "git.ref.update", ResourceID: "resource-1",
		Subject: verify.Subject{Fields: []verify.SubjectField{
			{Name: "operation", Kind: verify.KindString, Value: "git.ref.update"},
			{Name: "resource", Kind: verify.KindString, Value: "resource-1"},
			{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
			{Name: "expected_base", Kind: verify.KindString, Value: "base-1"},
			{Name: "candidate", Kind: verify.KindString, Value: "cand-1"},
		}},
	}
	evidence, err := reader.ReadEvidence(ctx, query)
	if err != nil {
		t.Fatalf("ReadEvidence: %v%s", err, pgtest.CauseText(err))
	}
	if len(evidence) != 1 || evidence[0].ActionID != "action-1" || evidence[0].Decision != request.DecisionPass ||
		evidence[0].Operation != "git.ref.update" || evidence[0].ResourceID != "resource-1" {
		t.Fatalf("evidence = %+v; want only the pass action-1", evidence)
	}
	// The stored operation and resource are preserved in the matched subject as
	// well as in their dedicated Evidence fields, so the canonical five-field
	// Git subject matches exactly.
	for name, want := range map[string]string{
		"operation":     "git.ref.update",
		"resource":      "resource-1",
		"ref":           "refs/heads/main",
		"expected_base": "base-1",
		"candidate":     "cand-1",
	} {
		field, ok := evidence[0].Subject.Field(name)
		if !ok || field.Kind != verify.KindString || field.Value != want {
			t.Errorf("evidence subject field %q = %+v, found %t; want %q", name, field, ok, want)
		}
	}
	if !evidence[0].ResolvedAt.Equal(pgtest.FixtureTime) {
		t.Fatalf("evidence resolvedAt = %s; want %s", evidence[0].ResolvedAt, pgtest.FixtureTime)
	}
}

type fixedPermission struct{ permission authorize.ExecutionPermission }

func (fixed fixedPermission) Permission(context.Context, platform.ExecutionFacts) (authorize.ExecutionPermission, error) {
	return fixed.permission, nil
}

func TestPlatformExecutionReaderAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, _ := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "running", 3, "", "")
	deadline := pgtest.FixtureTime.Add(2 * time.Minute)
	shard.SeedRunningAction(ctx, scope, "action-1", "execution-1", 0, deadline)
	reader, err := platform.NewExecutionReader(adapterController(t, shard), fixedPermission{permission: authorize.ExecOwnTaskContent})
	if err != nil {
		t.Fatalf("NewExecutionReader: %v", err)
	}
	admitted, err := reader.ReadExecution(ctx, "execution-1")
	if err != nil {
		t.Fatalf("ReadExecution: %v%s", err, pgtest.CauseText(err))
	}
	if admitted.ExecutionID != "execution-1" || admitted.ActionID != "action-1" ||
		admitted.Principal.ID != "principal-1" || admitted.Principal.Kind != identity.KindExecution ||
		!admitted.Principal.Enabled || admitted.Principal.Scope != scope || admitted.Principal.Version != 3 ||
		admitted.Permission != authorize.ExecOwnTaskContent || !admitted.Deadline.Equal(deadline) {
		t.Fatalf("admitted execution = %+v; want exact execution/action/principal facts", admitted)
	}
	if _, err := reader.ReadExecution(ctx, "execution-missing"); !adapterClass(err, fault.NotFound) {
		t.Fatalf("missing execution error = %v; want not_found", err)
	}
}

func TestInboxAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "running", 2, "", "")
	// The request item is self-sufficient: the authoritative admitted
	// snapshot declares the response output names and the action row carries
	// the immutable resolved subject.
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions: []\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	declared := definition.Request{
		Assignment: "role/engineer",
		Subject:    definition.NewSubject(map[string]definition.SubjectValue{"ref": {Kind: definition.SubjectLiteral, Literal: "refs/heads/main"}}),
		Outputs:    []id.OutputKey{"approved"},
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		PipelineID:      "pipeline-1",
		PipelineVersion: 1,
		Snapshot:        snapshot,
		Actions: []pipeline.AdmittedAction{{
			Index: 0, Name: "approve", Kind: definition.ActionKindRequest, Timeout: time.Minute, Request: &declared,
		}},
	})
	if err != nil {
		t.Fatalf("snapshotdoc.Encode: %v", err)
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE executions SET snapshot = $2, yaml_sha256 = $3 WHERE id = $1`,
		"execution-1", string(document), snapshot.YAMLSHA256[:]); err != nil {
		t.Fatalf("seed admitted snapshot: %v", err)
	}
	assignment := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer}
	assignmentBytes, _ := json.Marshal(assignment)
	subjectBytes, _ := json.Marshal(definition.NewSubject(map[string]definition.SubjectValue{
		"ref": {Kind: definition.SubjectLiteral, Literal: "refs/heads/main"},
	}))
	shard.SeedRequestAction(ctx, scope, "action-1", "execution-1", "approve", "running", 0, assignmentBytes, subjectBytes, "", "", time.Time{})
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE actions SET deadline = $1 WHERE id = 'action-1'`, pgtest.FixtureTime); err != nil {
		t.Fatalf("set action deadline: %v", err)
	}
	shard.SeedWaitingTask(ctx, scope, "task-1", 2, assignmentBytes)
	shard.SeedFailedTask(ctx, scope, "task-2", 4, "execution-2")
	shard.SeedExecution(ctx, scope, "execution-2", "failed", 5, "task-2", "")
	// A waiting task assigned to another role is not this principal's item.
	otherAssignment := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}
	otherBytes, _ := json.Marshal(otherAssignment)
	shard.SeedWaitingTask(ctx, scope, "task-3", 2, otherBytes)

	adapter, err := work.NewInbox(scoped, scoped)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	principal := identity.Principal{
		ID: "principal-1", Kind: identity.KindHuman, Scope: scope,
		Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, Enabled: true, Version: 1,
	}
	page, err := adapter.Query(ctx, inbox.Query{Scope: scope, Principal: principal})
	if err != nil {
		t.Fatalf("Inbox.Query: %v%s", err, pgtest.CauseText(err))
	}
	if len(page.Items) != 3 {
		t.Fatalf("inbox items = %+v; want request, waiting and failed items", page.Items)
	}
	seen := map[inbox.Kind]bool{}
	for _, item := range page.Items {
		seen[item.Kind] = true
		if !item.At.Equal(pgtest.FixtureTime) {
			t.Fatalf("inbox item %s at = %s; want %s", item.Kind, item.At, pgtest.FixtureTime)
		}
		switch item.Kind {
		case inbox.KindRequest:
			if item.Version != 1 || item.ActionID != "action-1" {
				t.Fatalf("request item = %+v; want action-1 at version 1", item)
			}
		case inbox.KindWaitingTask:
			if item.TaskID != "task-1" || item.Version != 1 {
				t.Fatalf("waiting item = %+v; want task-1 at question version 1", item)
			}
		case inbox.KindFailedTask:
			if item.TaskID != "task-2" || item.Version != 4 {
				t.Fatalf("failed item = %+v; want task-2 at lifecycle version 4", item)
			}
			// A failed_task item names only the task; the execution
			// identity stays local to the authoritative state lookup.
			if item.ExecutionID != "" || item.ActionID != "" || item.QuestionID != "" ||
				item.Question != "" || len(item.References) != 0 || item.LifecycleVersion != 0 ||
				item.Assignment != (request.Assignment{}) {
				t.Fatalf("failed item = %+v; want only kind/task/version/at", item)
			}
		}
	}
	if !seen[inbox.KindRequest] || !seen[inbox.KindWaitingTask] || !seen[inbox.KindFailedTask] {
		t.Fatalf("inbox kinds = %v; want all three", seen)
	}
	// A human without the operator role never sees a failed task, even in the
	// same scope (KODAMA.md assigns failed-task follow-up to human operators).
	reviewerOnly := principal
	reviewerOnly.ID = "principal-reviewer"
	reviewerOnly.Roles = []tenancy.Role{tenancy.RoleReviewer}
	if page, err := adapter.Query(ctx, inbox.Query{
		Scope: scope, Principal: reviewerOnly, Kinds: []inbox.Kind{inbox.KindFailedTask},
	}); err != nil {
		t.Fatalf("reviewer-only Inbox.Query: %v%s", err, pgtest.CauseText(err))
	} else if len(page.Items) != 0 {
		t.Fatalf("reviewer-only failed items = %+v; want none", page.Items)
	}
	// Following the cursor across pages returns every item exactly once.
	paged := make(map[string]int)
	var orderedKeys []string
	cursor := ""
	pages := 0
	for {
		page, err := adapter.Query(ctx, inbox.Query{Scope: scope, Principal: principal, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("paged Inbox.Query: %v%s", err, pgtest.CauseText(err))
		}
		pages++
		if len(page.Items) > 2 {
			t.Fatalf("paged inbox returned %d items for limit 2", len(page.Items))
		}
		for _, item := range page.Items {
			key := string(item.Kind) + "/" + string(item.TaskID) + "/" + string(item.ActionID)
			paged[key]++
			orderedKeys = append(orderedKeys, key)
		}
		if page.NextCursor == "" {
			break
		}
		if pages > 5 {
			t.Fatalf("inbox pagination did not terminate")
		}
		cursor = page.NextCursor
	}
	if len(paged) != 3 || pages != 2 {
		t.Fatalf("paged inbox = %v over %d pages; want 3 unique items over 2 pages", paged, pages)
	}
	if fmt.Sprint(orderedKeys) != "[request//action-1 waiting_task/task-1/ failed_task/task-2/]" {
		t.Fatalf("paged inbox order = %v; want request,waiting,failed at one instant", orderedKeys)
	}
	for key, count := range paged {
		if count != 1 {
			t.Fatalf("paged inbox item %s appeared %d times; want once", key, count)
		}
	}

	// The wrong principal scope is refused.
	foreign := principal
	foreign.Scope = pgtest.Scope("tenant-2", "project-2")
	if _, err := adapter.Query(ctx, inbox.Query{Scope: scope, Principal: foreign}); !adapterClass(err, fault.Forbidden) {
		t.Fatalf("foreign-principal inbox error = %v; want forbidden", err)
	}
}

func TestTriggersAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedPipeline(ctx, scope, "pipeline-1", "build", []byte("actions: []\n"), 1, "[]")
	shard.SeedEvent(ctx, scope, "event-1", "execution.queued", "subject-1", 1)
	shard.SeedEvent(ctx, scope, "event-2", "execution.queued", "subject-2", 1)
	shard.SeedEvent(ctx, scope, "event-3", "execution.queued", "subject-3", 1)
	adapter, err := work.NewTriggers(scoped, scoped, adapterController(t, shard), func() time.Time { return pgtest.FixtureTime }, nil)
	if err != nil {
		t.Fatalf("NewTriggers: %v", err)
	}
	limited, err := adapter.Pending(ctx, 0, 1)
	if err != nil || len(limited) != 1 || limited[0].Event.ID != "event-1" {
		t.Fatalf("Pending(limit 1) = %v, err=%v; want exactly event-1", adapterPendingIDs(limited), err)
	}
	secondPage, err := adapter.Pending(ctx, limited[0].Sequence, 1)
	if err != nil || len(secondPage) != 1 || secondPage[0].Event.ID != "event-2" {
		t.Fatalf("Pending after event-1 = %v, err=%v; want exactly event-2", adapterPendingIDs(secondPage), err)
	}
	pending, err := adapter.Pending(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Pending: %v%s", err, pgtest.CauseText(err))
	}
	if len(pending) != 3 || pending[0].Event.ID != "event-1" || pending[1].Event.ID != "event-2" || pending[2].Event.ID != "event-3" {
		t.Fatalf("pending = %v; want event-1,event-2,event-3", adapterPendingIDs(pending))
	}
	// The first selection persists its incomplete receipt; an equal retry
	// replays the first selection rather than replacing it.
	selection := trigger.Selection{
		PipelineID: "pipeline-1", EventID: "event-1", ServiceID: "service-1",
		Inputs: map[id.InputKey]string{"ref": "refs/heads/main"},
	}
	record := receipt.Record{
		ID: "receipt-1", Key: receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID,
			PrincipalID: "principal-1", Operation: "pipeline.start", CallerKey: "pipeline-1/event-1"},
		ContentHash: pgtest.Fingerprint("selection"), At: pgtest.FixtureTime,
	}
	selectEvent, err := event.NewEnvelope("event-select-1",
		event.Source{Kind: event.SourceShell, PrincipalID: "principal-1"}, scope, pgtest.FixtureTime,
		event.EnvelopeInput{Type: event.TypeExecutionQueued, SubjectID: "pipeline-1", SubjectVersion: 1})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	unit := trigger.SelectUnit{
		Owner: scope, PipelineID: "pipeline-1", EventID: "event-1",
		ExpectedPipelineVersion: 1, Selection: selection, Receipt: record,
		Audit:  []event.Audit{{ID: "audit-select-1", Scope: scope, Actor: "principal-1", Operation: "pipeline.start", At: pgtest.FixtureTime}},
		Events: []event.Envelope{selectEvent},
	}
	selectObserver := shard.ObserveUnit(t, "trigger", "select")
	first, err := adapter.Select(ctx, unit)
	if err != nil {
		t.Fatalf("Select: %v%s", err, pgtest.CauseText(err))
	}
	if first.Status != trigger.StatusSelected || first.Replayed || first.Selection.PipelineID != "pipeline-1" {
		t.Fatalf("selection = %+v; want a fresh selected result", first)
	}
	unit.Selection.Inputs = map[id.InputKey]string{"ref": "refs/heads/other"}
	second, err := adapter.Select(ctx, unit)
	if err != nil {
		t.Fatalf("Select replay: %v", err)
	}
	requireReplayOf(t, "trigger select replay", first, second)
	if second.Selection.Inputs["ref"] != "refs/heads/main" {
		t.Fatalf("replayed selection = %+v; want the first selection", second)
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM receipts WHERE id = 'receipt-1' AND completed_at IS NULL`, 1, "incomplete selection receipt")
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "receipts", map[string]any{"id": "receipt-1"}, map[string]any{
		"id": "receipt-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "pipeline.start", "caller_key": "pipeline-1/event-1",
		"content_sha256": pgtest.FingerprintBytes("selection"),
		// derived: the incomplete selection receipt stores the adapter's
		// selectionRecord (internal/shell/store/units/work/trigger.go): the
		// first Result, whose zero event content hash encodes as 32 zeros, and
		// the admitted YAML beside it (absent here).
		"result": pgtest.GoldenJSONOf(map[string]any{
			"result": map[string]any{
				"receipt_id": "receipt-1", "status": "selected", "replayed": false,
				"selection": map[string]any{
					"pipeline_id": "pipeline-1", "event_id": "event-1", "service_id": "service-1",
					"inputs":             map[string]any{"ref": "refs/heads/main"},
					"event_content_hash": make([]int, 32),
				},
				"receipt": map[string]any{
					"id": "receipt-1",
					"key": map[string]any{
						"tenant_id": "tenant-1", "project_id": "project-1", "principal_id": "principal-1",
						"operation": "pipeline.start", "caller_key": "pipeline-1/event-1",
					},
					"content_hash": pgtest.Fingerprint("selection"), "at": pgtest.FixtureTime,
				},
			},
			"yaml": nil,
		}),
		"completed_at": pgtest.NullValue, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "audit", map[string]any{"id": "audit-select-1"}, map[string]any{
		"id": "audit-select-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "pipeline.start", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "events", map[string]any{"id": "event-select-1"}, map[string]any{
		"id": "event-select-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "execution.queued", "report_type": pgtest.NullValue,
		"subject_id": "pipeline-1", "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	selectObserver.Finish(map[string]int{"receipts": 1, "audit": 1, "events": 1})
	// Completing an event writes the durable marker, and an equal retry applies
	// nothing.
	completeObserver := shard.ObserveUnit(t, "trigger", "complete")
	if err := adapter.Complete(ctx, trigger.CompletionUnit{Owner: scope, EventID: "event-1", Sequence: 1}); err != nil {
		t.Fatalf("Complete: %v%s", err, pgtest.CauseText(err))
	}
	if err := adapter.Complete(ctx, trigger.CompletionUnit{Owner: scope, EventID: "event-1", Sequence: 1}); err != nil {
		t.Fatalf("Complete replay: %v", err)
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM event_consumptions WHERE id = 'event-1' AND version = 1`, 1, "event marker")
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "event_consumptions", map[string]any{"id": "event-1"}, map[string]any{
		"id": "event-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"consumer": "trigger", "completed_at": pgtest.FixtureTime, "version": int64(1),
	})
	completeObserver.Finish(map[string]int{"event_consumptions": 1})
	// A consumed event is not offered again; a wrong sequence is refused and
	// leaves no marker.
	remaining, err := adapter.Pending(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Pending after complete: %v", err)
	}
	if len(remaining) != 3 || remaining[0].Event.ID != "event-2" || remaining[1].Event.ID != "event-3" || remaining[2].Event.ID != "event-select-1" {
		t.Fatalf("pending after complete = %v; want event-2,event-3,event-select-1", adapterPendingIDs(remaining))
	}
	// A consumed prefix row must not advance the internal scan cursor past the
	// next unconsumed row when the page size is smaller than the stream.
	afterConsume, err := adapter.Pending(ctx, 0, 2)
	if err != nil || len(afterConsume) != 2 || afterConsume[0].Event.ID != "event-2" || afterConsume[1].Event.ID != "event-3" {
		t.Fatalf("Pending(limit 2) after consuming event-1 = %v, err=%v; want event-2,event-3", adapterPendingIDs(afterConsume), err)
	}
	if err := adapter.Complete(ctx, trigger.CompletionUnit{Owner: scope, EventID: "event-2", Sequence: 99}); !adapterClass(err, fault.VersionConflict) {
		t.Fatalf("wrong-sequence complete error = %v; want version_conflict", err)
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM event_consumptions WHERE id = 'event-2'`, 0, "wrong-sequence marker")
}

func TestEngineAdapterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	executionID := startedExecution(t, env)
	// derived: applyPipelineAndCreateTask creates the first task of the
	// deterministic integrationIDs generator, and the pipeline generator of
	// newServiceEnv always allocates pipeline-1.
	const taskID = id.TaskID("task-1")
	engine := executionstore.NewEngine(adapterController(t, env.shard))
	loaded, err := engine.Load(ctx, executionID)
	if err != nil {
		t.Fatalf("Engine.Load: %v%s", err, pgtest.CauseText(err))
	}
	if loaded.ID != executionID || loaded.State != kernel.ExecutionQueued || loaded.Version != 1 || len(loaded.Actions) != 1 {
		t.Fatalf("loaded execution = %+v; want queued/1 with one action", loaded)
	}
	startRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT principal_id, workspace_id FROM executions WHERE id = $1`, string(executionID))
	if startRow[0] != "execution-principal-1" || startRow[1] != string(snapshotdoc.DeriveWorkspaceID(executionID)) {
		t.Fatalf("started execution principal/workspace = %v; want execution-principal-1/%s", startRow, snapshotdoc.DeriveWorkspaceID(executionID))
	}
	actionID := snapshotdoc.DeriveActionID(executionID, 0)
	engineObserver := env.shard.ObserveUnit(t, "engine", "commit")
	committed, err := engine.Commit(ctx, flow.Commit{
		ExecutionID: executionID, ExpectedExecutionVersion: 1,
		NextExecutionState: kernel.ExecutionRunning,
		ActionID:           actionID, ExpectedActionVersion: 1, NextActionState: kernel.ActionRunning,
		Output:   flow.OutputChange{Action: flow.OutputNone},
		Deadline: pgtest.FixtureTime.Add(time.Minute), At: pgtest.FixtureTime,
	})
	if err != nil {
		t.Fatalf("Engine.Commit: %v%s", err, pgtest.CauseText(err))
	}
	if committed.State != kernel.ExecutionRunning || committed.Version != 2 ||
		committed.Actions[0].State != kernel.ActionRunning || committed.Actions[0].Version != 2 {
		t.Fatalf("committed execution = %+v; want running/2 with a running action", committed)
	}
	actionDeadline := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT extract(epoch FROM deadline)::bigint::text, extract(epoch FROM activated_at)::bigint::text
		 FROM actions WHERE id = $1`, string(actionID))
	if actionDeadline[0] != strconv.FormatInt(pgtest.FixtureTime.Add(time.Minute).Unix(), 10) ||
		actionDeadline[1] != strconv.FormatInt(pgtest.FixtureTime.Unix(), 10) {
		t.Fatalf("committed action deadline/activated_at = %v; want %d/%d", actionDeadline, pgtest.FixtureTime.Add(time.Minute).Unix(), pgtest.FixtureTime.Unix())
	}
	pgtest.RequireRowCount(t, ctx, env.shard.Admin(),
		`SELECT count(*)::text FROM events WHERE type = 'execution.started' AND subject_id = $1`,
		1, "engine lifecycle event", string(executionID))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)},
		engineExecutionRow(taskID, executionID, "running", 2))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "build", "kind": "container", "state": "running", "version": int64(2),
		"timeout_ms": int64(300000), "deadline": pgtest.FixtureTime.Add(time.Minute),
		"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
		"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.FixtureTime, "answer_provenance": pgtest.NullValue,
	})
	// derived: event.ProjectLifecycle (internal/shell/event/contract_lifecycle.go)
	// projects the committed versions (execution 1->2, action 1->2) into these
	// system-sourced events; each id is the sha256 of the length-prefixed
	// (execution id, subject version, type, subject) tuple.
	for _, row := range []struct {
		id, eventType, subject string
		data                   map[string]any
	}{
		{"event-1c502446f0f9e297a402c3258f9db447917fee446b616771c0a31bec242f9a15", "execution.started", string(executionID),
			map[string]any{"execution_id": string(executionID), "previous_state": "queued", "state": "running", "version": 2}},
		{"event-3f2cb0380832312f7f642df69b287d860dab66498c58f614889355fd244073b0", "action.transition", string(actionID),
			map[string]any{"action_id": string(actionID), "index": 0, "kind": "container",
				"previous_state": "pending", "state": "running", "version": 2}},
	} {
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": row.id}, map[string]any{
			"id": row.id, "tenant_id": "tenant-1", "project_id": "project-1",
			"type": row.eventType, "report_type": pgtest.NullValue,
			"subject_id": row.subject, "subject_version": int64(2),
			"source_kind": "shell", "source_principal": "system",
			"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
			"at": pgtest.FixtureTime, "data": pgtest.GoldenJSONOf(row.data), "publish_state": "pending",
			"publish_sequence": pgtest.GoldenDBSequence(),
		})
	}
	engineObserver.Finish(map[string]int{"executions": 1, "actions": 1, "events": 2})
}

// engineExecutionRow is the whole executions row of the "build" pipeline the
// task service admitted and started: the digest, snapshot and workspace id are
// the exact values the product derives from the admitted YAML and the execution
// id. The row carries no credential, so the nonce and expiry are NULL.
func engineExecutionRow(taskID id.TaskID, executionID id.ExecutionID, state string, version int64) map[string]any {
	yaml := integrationPipelineYAML("echo v1")
	return map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": "pipeline-1", "task_id": string(taskID),
		"principal_id": "execution-principal-1", "state": state, "version": version,
		"yaml_sha256": pgtest.SHA256(yaml),
		"snapshot": admittedSnapshot(admittedSnapshotWant{
			PipelineID: "pipeline-1", PipelineVersion: 1, YAML: yaml,
			Actions: []admittedActionWant{{Name: "build", Kind: definition.ActionKindContainer, Timeout: 5 * time.Minute}},
		}),
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID(executionID)),
		"failure":      pgtest.NullValue, "created_at": pgtest.FixtureTime,
	}
}

// TestEngineCommitCleanupIntentOnPostgreSQL drives the cleanup-INTENT branch of
// the engine commit: a cancellation that carries a flow.CleanupIntent and no
// CleanupUpdate must create the one pending cleanup row with zero attempts and
// the commit time as its first retry.
func TestEngineCommitCleanupIntentOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	executionID := startedExecution(t, env)
	const taskID = id.TaskID("task-1")
	engine := executionstore.NewEngine(adapterController(t, env.shard))
	actionID := snapshotdoc.DeriveActionID(executionID, 0)
	workspaceID := snapshotdoc.DeriveWorkspaceID(executionID)
	noCurrent := -1
	observer := env.shard.ObserveUnit(t, "engine", "commit.cleanup-intent")
	committed, err := engine.Commit(ctx, flow.Commit{
		ExecutionID: executionID, ExpectedExecutionVersion: 1,
		NextExecutionState: kernel.ExecutionCancelled,
		PendingCancellations: []flow.ActionUpdate{
			{ActionID: actionID, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
		},
		NextCurrentAction:    &noCurrent,
		Output:               flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential: true,
		Cleanup:              &flow.CleanupIntent{WorkspaceID: workspaceID, Reason: "cancelled"},
		At:                   pgtest.FixtureTime,
	})
	if err != nil {
		t.Fatalf("Engine.Commit with a cleanup intent: %v%s", err, pgtest.CauseText(err))
	}
	if committed.State != kernel.ExecutionCancelled || committed.Version != 2 ||
		committed.Actions[0].State != kernel.ActionCancelled || committed.Actions[0].Version != 2 {
		t.Fatalf("committed execution = %+v; want cancelled/2 with a cancelled action", committed)
	}
	if committed.Cleanup != (kernel.Cleanup{State: kernel.CleanupPending, WorkspaceID: workspaceID}) {
		t.Fatalf("committed cleanup = %+v; want pending for workspace %s with no attempts", committed.Cleanup, workspaceID)
	}
	// derived: cleanupID is "cln-" plus the first 16 bytes of
	// sha256(execution id + "\x00cleanup") (internal/shell/store/units/execution/codec.go).
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "cleanup", map[string]any{"execution_id": string(executionID)}, map[string]any{
		"id": "cln-f7ad8200d3f20d57ccccb16ef308d8b7", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": string(executionID), "workload_id": pgtest.NullValue, "workspace_id": string(workspaceID),
		"state": "pending", "attempts": int64(0), "last_error": pgtest.NullValue, "next_retry_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)},
		engineExecutionRow(taskID, executionID, "cancelled", 2))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "build", "kind": "container", "state": "cancelled", "version": int64(2),
		"timeout_ms": int64(300000), "deadline": pgtest.NullValue,
		"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
		"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	// derived: the lifecycle projection (internal/shell/event/contract_lifecycle.go)
	// emits the execution and pending-action transitions; the commit carries no
	// CleanupUpdate, so no execution.cleanup event is projected.
	for _, row := range []struct {
		id, eventType, subject string
		data                   map[string]any
	}{
		{"event-ca1951af2c9a95f6e21a188a6b3318162dedb2f530be062bd5000265c71de432", "execution.cancelled", string(executionID),
			map[string]any{"execution_id": string(executionID), "previous_state": "queued", "state": "cancelled", "version": 2}},
		{"event-3f2cb0380832312f7f642df69b287d860dab66498c58f614889355fd244073b0", "action.transition", string(actionID),
			map[string]any{"action_id": string(actionID), "index": 0, "kind": "container",
				"previous_state": "pending", "state": "cancelled", "version": 2}},
	} {
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": row.id}, map[string]any{
			"id": row.id, "tenant_id": "tenant-1", "project_id": "project-1",
			"type": row.eventType, "report_type": pgtest.NullValue,
			"subject_id": row.subject, "subject_version": int64(2),
			"source_kind": "shell", "source_principal": "system",
			"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
			"at": pgtest.FixtureTime, "data": pgtest.GoldenJSONOf(row.data), "publish_state": "pending",
			"publish_sequence": pgtest.GoldenDBSequence(),
		})
	}
	observer.Finish(map[string]int{"executions": 1, "actions": 1, "cleanup": 1, "events": 2})
}

// tokenNonce returns a deterministic 32-byte token nonce.
func tokenNonce(fill byte) []byte {
	nonce := make([]byte, 32)
	for index := range nonce {
		nonce[index] = fill
	}
	return nonce
}

// scopedDB returns the runtime-role connection a golden read uses.
func scopedDB(t *testing.T, shard *pgtest.Shard) *pgdriver.DB {
	t.Helper()
	db := shard.Runtime(2)
	t.Cleanup(db.Close)
	return db
}

// bytesOf returns n copies of fill for a deterministic bytea expectation.
func bytesOf(n int, fill byte) []byte {
	out := make([]byte, n)
	for index := range out {
		out[index] = fill
	}
	return out
}

func adapterClass(err error, want fault.Class) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == want
}

func adapterPendingIDs(pending []event.PendingEvent) []string {
	out := make([]string, len(pending))
	for index, row := range pending {
		out[index] = string(row.Event.ID)
	}
	return out
}

// TestInboxRequestProjectionOnPostgreSQL proves the PostgreSQL inbox adapter
// projects the self-sufficient request item exactly like the memory adapter:
// the immutable resolved subject, the declared response output names from the
// admitted snapshot, and the action deadline. An unresolved stored subject
// fails closed instead of leaking the runtime lookup.
func TestInboxRequestProjectionOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedPipeline(ctx, scope, "pipeline-1", "build", []byte("actions: []\n"), 1, "[]")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executions := executionstore.New(storepg.New(runtime), storepg.New(runtime))

	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions: []\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	declared := definition.Request{
		Assignment: "role/engineer",
		Subject: definition.NewSubject(map[string]definition.SubjectValue{
			"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
			"seed":     {Kind: definition.SubjectReference, Reference: definition.Reference{Input: true, Key: "seed"}},
		}),
		Outputs: []id.OutputKey{"approved", "decision"},
	}
	if _, err := executions.Start(ctx, execution.StartUnit{
		Owner:                   scope,
		PipelineID:              "pipeline-1",
		ExpectedPipelineVersion: 1,
		Snapshot:                snapshot,
		Actions: []pipeline.AdmittedAction{{
			Index:   0,
			Name:    "approve",
			Kind:    definition.ActionKindRequest,
			Timeout: time.Minute,
			Request: &declared,
		}},
		Initiator:          "principal-1",
		ExecutionPrincipal: "execution-principal-1",
		NewExecutionID:     "execution-request",
		At:                 pgtest.FixtureTime,
	}); err != nil {
		t.Fatalf("execution Start: %v%s", err, pgtest.CauseText(err))
	}
	actionID := snapshotdoc.DeriveActionID("execution-request", 0)
	resolved, err := json.Marshal(definition.NewSubject(map[string]definition.SubjectValue{
		"artifact": {Kind: definition.SubjectLiteral, Literal: "built:seed-1"},
		"seed":     {Kind: definition.SubjectLiteral, Literal: "seed-1"},
	}))
	if err != nil {
		t.Fatalf("marshal resolved subject: %v", err)
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE actions SET state = 'running', deadline = $2, request_subject = $3 WHERE id = $1`,
		actionID, pgtest.FixtureTime, resolved); err != nil {
		t.Fatalf("activate request action: %v", err)
	}

	adapter, err := work.NewInbox(scoped, scoped)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	principal := identity.Principal{
		ID: "principal-1", Kind: identity.KindHuman, Scope: scope,
		Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1,
	}
	assertRequestProjection := func(item inbox.Item) {
		t.Helper()
		if item.Subject == nil {
			t.Fatalf("request item carries no immutable subject: %+v", item)
		}
		fields := item.Subject.Fields()
		if value := fields["artifact"]; value.Kind != definition.SubjectLiteral || value.Literal != "built:seed-1" {
			t.Errorf("request subject artifact = %+v; want the resolved literal built:seed-1", value)
		}
		if value := fields["seed"]; value.Kind != definition.SubjectLiteral || value.Literal != "seed-1" {
			t.Errorf("request subject seed = %+v; want the resolved literal seed-1", value)
		}
		if len(item.Outputs) != 2 || item.Outputs[0] != "approved" || item.Outputs[1] != "decision" {
			t.Errorf("request outputs = %v; want the declared names [approved decision]", item.Outputs)
		}
		if !item.At.Equal(pgtest.FixtureTime) {
			t.Errorf("request deadline = %s; want %s", item.At, pgtest.FixtureTime)
		}
	}

	page, err := adapter.Query(ctx, inbox.Query{Scope: scope, Principal: principal, Kinds: []inbox.Kind{inbox.KindRequest}})
	if err != nil {
		t.Fatalf("Inbox.Query: %v%s", err, pgtest.CauseText(err))
	}
	if len(page.Items) != 1 {
		t.Fatalf("request inbox items = %+v; want one", page.Items)
	}
	assertRequestProjection(page.Items[0])

	// The projection travels with every page item.
	cursor := ""
	seen := 0
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("inbox pagination did not terminate")
		}
		page, err := adapter.Query(ctx, inbox.Query{Scope: scope, Principal: principal, Kinds: []inbox.Kind{inbox.KindRequest}, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("paged Inbox.Query: %v%s", err, pgtest.CauseText(err))
		}
		for _, item := range page.Items {
			assertRequestProjection(item)
			seen++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if seen != 1 {
		t.Fatalf("paged request items = %d; want 1", seen)
	}

	// A running request row whose stored subject still carries an unresolved
	// runtime lookup is corruption: the read fails closed and never leaks it.
	unresolved, err := json.Marshal(definition.NewSubject(map[string]definition.SubjectValue{
		"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
	}))
	if err != nil {
		t.Fatalf("marshal unresolved subject: %v", err)
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE actions SET request_subject = $2 WHERE id = $1`, actionID, unresolved); err != nil {
		t.Fatalf("corrupt request subject: %v", err)
	}
	if _, err := adapter.Query(ctx, inbox.Query{Scope: scope, Principal: principal, Kinds: []inbox.Kind{inbox.KindRequest}}); !adapterClass(err, fault.InternalFailure) {
		t.Fatalf("unresolved-subject inbox error = %v; want internal_failure", err)
	}

	// A present but empty subject is corruption too: the running request item
	// is never returned without an immutable subject.
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE actions SET request_subject = '{}' WHERE id = $1`, actionID); err != nil {
		t.Fatalf("empty request subject: %v", err)
	}
	if _, err := adapter.Query(ctx, inbox.Query{Scope: scope, Principal: principal, Kinds: []inbox.Kind{inbox.KindRequest}}); !adapterClass(err, fault.InternalFailure) {
		t.Fatalf("empty-subject inbox error = %v; want internal_failure", err)
	}

	// A stored assignment that no longer satisfies the declared grammar fails
	// both the inbox projection and execution.inspect closed.
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE actions SET request_subject = $2, request_assignment = '{"kind":"role","role":"not-a-role"}' WHERE id = $1`,
		actionID, resolved); err != nil {
		t.Fatalf("corrupt request assignment: %v", err)
	}
	if _, err := adapter.Query(ctx, inbox.Query{Scope: scope, Principal: principal, Kinds: []inbox.Kind{inbox.KindRequest}}); !adapterClass(err, fault.InternalFailure) {
		t.Fatalf("invalid-assignment inbox error = %v; want internal_failure", err)
	}
	if _, err := executions.Inspect(ctx, scope, "execution-request"); !adapterClass(err, fault.InternalFailure) {
		t.Fatalf("invalid-assignment inspect error = %v; want internal_failure", err)
	}
}
