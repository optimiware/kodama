//go:build component

package postgrescomponent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

type platformPrincipals struct{ principal identity.Principal }

func (fixed platformPrincipals) Resolve(context.Context, string) (identity.Principal, error) {
	return fixed.principal, nil
}

type platformBindings struct{ version uint64 }

func (fixed platformBindings) ReadBinding(_ context.Context, query verify.BindingQuery) (resource.Binding, error) {
	return resource.Binding{
		ID: "binding-1", ResourceID: query.ResourceID, Scope: query.Scope,
		EnforcingService: service.ServiceID(query.ServiceID),
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"}, Version: fixed.version,
	}, nil
}

type platformEvidence struct{}

func (platformEvidence) ReadEvidence(context.Context, verify.EvidenceQuery) ([]verify.Evidence, error) {
	return nil, nil
}

type platformVerifications struct{ record verify.Record }

func (fixed platformVerifications) ReadVerification(_ context.Context, scope tenancy.Scope, identity verify.VerificationID) (verify.Record, error) {
	if fixed.record.Scope != scope || fixed.record.ID != identity {
		return verify.Record{}, fault.New(fault.NotFound, "verification record is not visible")
	}
	return fixed.record, nil
}

type platformAuthorizer struct{ decision authorize.Decision }

func (fixed platformAuthorizer) Authorize(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
	return fixed.decision, nil
}

type platformAuthority struct {
	denied map[authorize.AuthorityCheck]bool
}

func (fixed platformAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		facts = append(facts, authorize.AuthorityFact{
			Check: read.Check, Scope: read.Scope, PrincipalID: read.PrincipalID,
			TargetID: read.TargetID, Version: read.ExpectedVersion, Allowed: !fixed.denied[read.Check],
		})
	}
	return facts, nil
}

type platformClock struct{}

func (platformClock) Now() time.Time { return pgtest.FixtureTime }

type platformIDs struct{ next int }

func (ids *platformIDs) NewEventID() (id.EventID, error) {
	ids.next++
	return id.EventID(fmt.Sprintf("event-%d", ids.next)), nil
}

func (ids *platformIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.next)), nil
}

func platformScope() tenancy.Scope { return pgtest.Scope("tenant-1", "project-1") }

func platformHuman() identity.Principal {
	return identity.Principal{
		ID: "principal-1", Kind: identity.KindHuman, Scope: platformScope(),
		Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1,
	}
}

func platformService() identity.Principal {
	return identity.Principal{
		ID: "service-a", Kind: identity.KindService, Scope: platformScope(),
		Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1,
	}
}

func platformSubject() verify.Subject {
	return verify.Subject{Fields: []verify.SubjectField{{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"}}}
}

func platformSeed(t *testing.T) (*pgtest.Shard, *storepg.Store) {
	t.Helper()
	ctx := context.Background()
	shard := migratedShard(t)
	scope := platformScope()
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedMembership(ctx, scope, "membership-1", "principal-1", 1, string(tenancy.RoleEngineer))
	shard.SeedService(ctx, scope, "service-a", "service-a", string(tenancy.RoleEngineer))
	shard.SeedResource(ctx, scope, "resource-1")
	shard.SeedBinding(ctx, scope, "binding-1", "resource-1", "service-a")
	runtime := shard.Runtime(4)
	t.Cleanup(runtime.Close)
	return shard, storepg.New(runtime)
}

// platformVerifyAuthority rebuilds the actor.verify current-authority reads the
// production verifier attaches to its record unit for a human actor, so the
// adapter re-observes the same service, binding and principal rows.
func platformVerifyAuthority(t *testing.T, actor identity.Principal) []authorize.AuthorityRead {
	t.Helper()
	operation, ok := authorize.LookupOperation("actor.verify")
	if !ok {
		t.Fatal("actor.verify is not a registered operation")
	}
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		read := authorize.AuthorityRead{
			Operation: operation.Name, Required: operation.Requires, Check: check,
			Scope: platformScope(), PrincipalID: actor.ID,
		}
		// A human actor names no execution or action, so those reads carry no
		// target and the adapter treats them as not applicable.
		switch check {
		case authorize.CheckServiceEnabled:
			read.PrincipalID = "service-a"
		case authorize.CheckBindingVersion:
			read.TargetID = "binding-1"
			read.ExpectedVersion = 1
		}
		reads = append(reads, read)
	}
	return reads
}

// TestRealVerifierAndReporterOnPostgreSQL drives the real verifier and reporter
// onto the PostgreSQL record/report adapters, including the report replay.
func TestRealVerifierAndReporterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := platformSeed(t)
	records, err := platform.NewRecordAdapter(scoped, scoped)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	next := 0
	verifier, err := verify.New(verify.Config{
		Principals: platformPrincipals{principal: platformHuman()},
		Bindings:   platformBindings{version: 1},
		Evidence:   platformEvidence{},
		Records:    records,
		Clock:      platformClock{},
		NewID: func() (verify.VerificationID, error) {
			next++
			return verify.VerificationID(fmt.Sprintf("verification-%d", next)), nil
		},
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	verifyObserver := shard.ObserveUnit(t, "verification", "record")
	record, err := verifier.Verify(ctx, verify.Attempt{
		Scope: platformScope(), Service: platformService(), Credential: "credential",
		ResourceID: "resource-1", Operation: resource.OperationGitRefUpdate, Subject: platformSubject(),
	})
	if err != nil {
		t.Fatalf("real Verify: %v%s", err, pgtest.CauseText(err))
	}
	if record.Result != verify.ResultApproved {
		t.Fatalf("verification result = %q; want approved", record.Result)
	}
	count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM verifications WHERE id = $1`, string(record.ID))
	if err != nil || count != 1 {
		t.Fatalf("verification rows = %d, err=%v; want 1", count, err)
	}
	// The durable verification row carries the exact approved fact, not just a
	// row count.
	durable := scopedRow(t, ctx, shard.Admin(), platformScope(), 5,
		`SELECT result, approved::text, actor_id, binding_id, operation FROM verifications WHERE id = $1`,
		string(record.ID))
	if durable[0] != "approved" || durable[1] != "false" || durable[2] != "principal-1" ||
		durable[3] != "binding-1" || durable[4] != "git.ref.update" {
		t.Fatalf("durable verification = %v; want approved/not-approved-evidence/principal-1/binding-1/git.ref.update", durable)
	}
	subjectText := scopedRow(t, ctx, shard.Admin(), platformScope(), 1,
		`SELECT subject::text FROM verifications WHERE id = $1`, string(record.ID))
	var storedSubject verify.Subject
	if err := json.Unmarshal([]byte(subjectText[0]), &storedSubject); err != nil {
		t.Fatalf("decode durable verification subject %q: %v", subjectText[0], err)
	}
	if field, ok := storedSubject.Field("ref"); !ok || field.Kind != verify.KindString || field.Value != "refs/heads/main" {
		t.Fatalf("durable verification subject = %+v; want the ref string", storedSubject)
	}
	nullableIDs := scopedRow(t, ctx, shard.Admin(), platformScope(), 2,
		`SELECT (execution_id IS NULL)::text, (action_id IS NULL)::text FROM verifications WHERE id = $1`, string(record.ID))
	if nullableIDs[0] != "true" || nullableIDs[1] != "true" {
		t.Fatalf("durable verification execution/action = %v; want both NULL for a human actor", nullableIDs)
	}
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "verifications", map[string]any{"id": string(record.ID)}, map[string]any{
		"id": string(record.ID), "tenant_id": "tenant-1", "project_id": "project-1",
		"service_id": "service-a", "actor_id": "principal-1",
		"execution_id": pgtest.NullValue, "action_id": pgtest.NullValue,
		"binding_id": "binding-1", "resource_id": "resource-1", "operation": "git.ref.update",
		"subject":            `{"fields":[{"name":"ref","kind":"string","value":"refs/heads/main"}]}`,
		"requested_approval": pgtest.NullValue, "result": "approved", "approved": false,
		"created_at": pgtest.FixtureTime,
	})
	verifyObserver.Finish(map[string]int{"verifications": 1})

	// The production verifier attaches no receipt, audit or event to its
	// RecordUnit, but the adapter persists them; drive that intent path with a
	// second record under a new verification id.
	intentRecord := record
	intentRecord.ID = "verification-with-intent"
	intentEvent, err := event.NewEnvelope("event-with-intent",
		event.Source{Kind: event.SourceService, PrincipalID: "service-a"}, platformScope(), pgtest.FixtureTime,
		event.EnvelopeInput{
			Type: event.TypeCustomObservation, SubjectID: "resource-1", SubjectVersion: 1,
			CausationID: string(intentRecord.ID), Data: []byte(`{"verified":true}`),
		})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	intentResult, err := json.Marshal(verify.RecordResult{ID: intentRecord.ID})
	if err != nil {
		t.Fatalf("encode record result: %v", err)
	}
	intentObserver := shard.ObserveUnit(t, "verification", "record.with-intent")
	intentCommitted, err := records.Record(ctx, verify.RecordUnit{
		Owner: platformScope(), Record: intentRecord,
		Receipt: receipt.Record{
			ID: "receipt-with-intent",
			Key: receipt.Key{TenantID: platformScope().TenantID, ProjectID: platformScope().ProjectID,
				PrincipalID: "principal-1", Operation: "actor.verify", CallerKey: "verify-intent-1"},
			ContentHash: pgtest.Fingerprint("verify-intent-1"), Result: intentResult, At: pgtest.FixtureTime,
		},
		Audit: []event.Audit{{
			ID: "audit-with-intent", Scope: platformScope(), Actor: "principal-1",
			Operation: "actor.verify", At: pgtest.FixtureTime, Detail: "verified git.ref.update",
		}},
		Events:    []event.Envelope{intentEvent},
		Authority: platformVerifyAuthority(t, record.Actor),
	})
	if err != nil {
		t.Fatalf("Record with receipt, audit and event: %v%s", err, pgtest.CauseText(err))
	}
	if intentCommitted != (verify.RecordResult{ID: "verification-with-intent"}) {
		t.Fatalf("record with intent = %+v; want a fresh verification-with-intent", intentCommitted)
	}
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "verifications", map[string]any{"id": "verification-with-intent"}, map[string]any{
		"id": "verification-with-intent", "tenant_id": "tenant-1", "project_id": "project-1",
		"service_id": "service-a", "actor_id": "principal-1",
		"execution_id": pgtest.NullValue, "action_id": pgtest.NullValue,
		"binding_id": "binding-1", "resource_id": "resource-1", "operation": "git.ref.update",
		"subject":            `{"fields":[{"name":"ref","kind":"string","value":"refs/heads/main"}]}`,
		"requested_approval": pgtest.NullValue, "result": "approved", "approved": false,
		"created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "receipts", map[string]any{"id": "receipt-with-intent"}, map[string]any{
		"id": "receipt-with-intent", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "actor.verify", "caller_key": "verify-intent-1",
		"content_sha256": pgtest.FingerprintBytes("verify-intent-1"), "result": pgtest.GoldenJSONOf(verify.RecordResult{ID: "verification-with-intent"}),
		"completed_at": pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "audit", map[string]any{"id": "audit-with-intent"}, map[string]any{
		"id": "audit-with-intent", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "actor.verify", "at": pgtest.FixtureTime,
		"detail": "verified git.ref.update",
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "events", map[string]any{"id": "event-with-intent"}, map[string]any{
		"id": "event-with-intent", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "observation.custom", "report_type": pgtest.NullValue,
		"subject_id": "resource-1", "subject_version": int64(1),
		"source_kind": "service", "source_principal": "service-a",
		"correlation_id": pgtest.NullValue, "causation_id": "verification-with-intent",
		"at": pgtest.FixtureTime, "data": []byte(`{"verified":true}`), "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	intentObserver.Finish(map[string]int{"verifications": 1, "receipts": 1, "audit": 1, "events": 1})

	reports, err := platform.NewReportAdapter(scoped, scoped)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	reporter, err := verify.NewReporter(verify.ReportConfig{
		Authorizer:    platformAuthorizer{decision: authorize.DecisionAllow},
		Authority:     platformAuthority{},
		Bindings:      platformBindings{version: 1},
		Verifications: platformVerifications{record: record},
		Reports:       reports,
		Clock:         platformClock{},
		IDs:           &platformIDs{},
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	command := verify.ReportCommand{
		Scope: platformScope(), Service: platformService(), SourceEventID: "source-event-1",
		EventType: event.ReportType("git.ref.updated"), VerificationID: record.ID,
		ResourceID: "resource-1", Operation: resource.OperationGitRefUpdate,
		Subject: platformSubject(), Data: []byte(`{"commit":"abc"}`),
	}
	// derived: the report receipt content is the capability's reportContent
	// document (internal/shell/verify/report.go); Data marshals as base64.
	reportReceiptContent := []byte(fmt.Sprintf(
		`{"binding_id":"binding-1","resource_id":"resource-1","event_type":"git.ref.updated","operation":"git.ref.update",`+
			`"verification_id":%q,"subject":{"fields":[{"name":"ref","kind":"string","value":"refs/heads/main"}]},`+
			`"data":"eyJjb21taXQiOiJhYmMifQ=="}`, record.ID))
	reportObserver := shard.ObserveUnit(t, "verification", "report")
	first, err := reporter.Report(ctx, command)
	if err != nil {
		t.Fatalf("real Report: %v%s", err, pgtest.CauseText(err))
	}
	if first.Replayed {
		t.Fatal("first report was marked replayed")
	}
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "events", map[string]any{"id": "event-1"}, map[string]any{
		"id": "event-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "resource.verified", "report_type": "git.ref.updated",
		"subject_id": "resource-1", "subject_version": int64(1),
		"source_kind": "service", "source_principal": "service-a",
		"correlation_id": pgtest.NullValue, "causation_id": string(record.ID),
		"at": pgtest.FixtureTime, "data": []byte(`{"commit":"abc"}`), "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "audit", map[string]any{"id": "event-2"}, map[string]any{
		"id": "event-2", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "service-a", "operation": "resource.report", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "receipts", map[string]any{"id": "receipt-3"}, map[string]any{
		"id": "receipt-3", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "service-a", "operation": "resource.report", "caller_key": "source-event-1",
		"content_sha256": pgtest.ContentSHA256(t, receipt.Content{MediaType: "application/json", Bytes: reportReceiptContent}),
		"result":         pgtest.GoldenJSONOf(first),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	reportObserver.Finish(map[string]int{"events": 1, "audit": 1, "receipts": 1})
	second, err := reporter.Report(ctx, command)
	if err != nil {
		t.Fatalf("report replay: %v", err)
	}
	requireReplayOf(t, "verified report replay", first, second)
	events, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM events WHERE type = 'resource.verified'`)
	if err != nil || events != 1 {
		t.Fatalf("verified events = %d, err=%v; want 1", events, err)
	}
}

// TestVerificationRecordReplayOnPostgreSQL proves the record adapter reports a
// replayed verification commit and returns the original durable identity.
func TestVerificationRecordReplayOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	_, scoped := platformSeed(t)
	records, err := platform.NewRecordAdapter(scoped, scoped)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	record := verify.Record{
		ID: "verification-1", Scope: platformScope(),
		CallingService: "service-a", BindingID: "binding-1",
		Actor: platformHuman(), ResourceID: "resource-1", Operation: "git.ref.update",
		Subject: platformSubject(), Result: verify.ResultApproved, Approved: true,
		At: pgtest.FixtureTime,
	}
	recordReceipt := receipt.Record{
		ID: "receipt-verification-1",
		Key: receipt.Key{TenantID: platformScope().TenantID, ProjectID: platformScope().ProjectID,
			PrincipalID: "principal-1", Operation: "git.ref.verify", CallerKey: "verify-1"},
		ContentHash: pgtest.Fingerprint("verify-1"),
		Result:      []byte(`{"id":"verification-1","replayed":false}`), At: pgtest.FixtureTime,
	}
	unit := verify.RecordUnit{Owner: platformScope(), Record: record, Receipt: recordReceipt}
	first, err := records.Record(ctx, unit)
	if err != nil {
		t.Fatalf("first Record: %v%s", err, pgtest.CauseText(err))
	}
	if first.Replayed || first.ID != "verification-1" {
		t.Fatalf("first record = %+v; want a fresh verification-1", first)
	}
	second, err := records.Record(ctx, unit)
	if err != nil {
		t.Fatalf("replayed Record: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "verification record replay", first, second)
}

// TestRealEventIngressOnPostgreSQL drives the real ingress onto the PostgreSQL
// ingress adapter: publish, replay and deny.
func TestRealEventIngressOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := platformSeed(t)
	port, err := platform.NewIngressAdapter(scoped, scoped)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	ids := &platformIDs{}
	ingress, err := event.NewIngress(event.IngressConfig{
		Credentials: platformPrincipals{principal: platformHuman()},
		Authority:   platformAuthority{},
		Authorizer:  platformAuthorizer{decision: authorize.DecisionAllow},
		Port:        port, Clock: platformClock{},
		NewEventID: ids.NewEventID, NewReceiptID: ids.NewReceiptID,
	})
	if err != nil {
		t.Fatalf("NewIngress: %v", err)
	}
	attempt := event.PublishAttempt{
		Credential: "credential", CallerKey: "caller-1",
		Content: receipt.Content{MediaType: "application/json", Bytes: []byte(`{"observation":1}`)},
		Type:    string(event.TypeCustomObservation), SubjectID: "subject-1",
	}
	publishObserver := shard.ObserveUnit(t, "ingress", "publish")
	first, err := ingress.Publish(ctx, attempt)
	if err != nil {
		t.Fatalf("ingress publish: %v%s", err, pgtest.CauseText(err))
	}
	replayObserver := shard.ObserveUnit(t, "ingress", "replay")
	second, err := ingress.Publish(ctx, attempt)
	replayObserver.Finish(map[string]int{})
	if err != nil {
		t.Fatalf("ingress replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "ingress publish replay", first, second)
	count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM events WHERE id = $1`, string(first.EventID))
	if err != nil || count != 1 {
		t.Fatalf("ingress event rows = %d, err=%v; want 1", count, err)
	}
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "events", map[string]any{"id": string(first.EventID)}, map[string]any{
		"id": string(first.EventID), "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "observation.custom", "report_type": pgtest.NullValue,
		"subject_id": "subject-1", "subject_version": pgtest.NullValue,
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue,
		"publish_state": "pending", "publish_sequence": pgtest.GoldenDBSequence(),
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "audit", map[string]any{"id": string(first.EventID)}, map[string]any{
		"id": string(first.EventID), "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "event.publish", "at": pgtest.FixtureTime,
		"detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), platformScope(), "receipts", map[string]any{
		"principal_id": "principal-1", "operation": "event.publish", "caller_key": "caller-1",
	}, map[string]any{
		"id": "receipt-2", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "event.publish", "caller_key": "caller-1",
		"content_sha256": pgtest.ContentSHA256(t, attempt.Content), "result": []byte(string(first.EventID)),
		"completed_at": pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	publishObserver.Finish(map[string]int{"events": 1, "audit": 1, "receipts": 1})

	for _, table := range []string{"events", "audit", "receipts"} {
		count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM `+table)
		if err != nil || count != 1 {
			t.Fatalf("%s rows after ingress replay = %d, err=%v; want 1 (no new rows)", table, count, err)
		}
	}

	denied, err := event.NewIngress(event.IngressConfig{
		Credentials: platformPrincipals{principal: platformHuman()},
		Authority:   platformAuthority{},
		Authorizer:  platformAuthorizer{decision: authorize.DecisionDeny},
		Port:        port, Clock: platformClock{},
		NewEventID: ids.NewEventID, NewReceiptID: ids.NewReceiptID,
	})
	if err != nil {
		t.Fatalf("NewIngress deny: %v", err)
	}
	denyAttempt := attempt
	denyAttempt.CallerKey = "caller-denied"
	_, err = denied.Publish(ctx, denyAttempt)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("denied ingress error = %v; want forbidden", err)
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM events WHERE type = 'observation.custom'`, 1, "denied ingress event rows")
}
