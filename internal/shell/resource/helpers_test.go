package resource_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func receiptContent(body string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(body)}
}

type scopeFixture struct {
	tenant  string
	project string
}

func tenancyScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s fault, got nil", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v is not a fault; want class %s", err, want)
	}
	if got != want {
		t.Fatalf("fault class = %s; want %s (%v)", got, want, err)
	}
}

func findAuthority(t *testing.T, reads []authorize.AuthorityRead, check authorize.AuthorityCheck) authorize.AuthorityRead {
	t.Helper()
	for _, read := range reads {
		if read.Check == check {
			return read
		}
	}
	t.Fatalf("authority reads %v do not contain check %q", reads, check)
	return authorize.AuthorityRead{}
}

func requireBindingAuthority(
	t *testing.T,
	reads []authorize.AuthorityRead,
	operation authorize.OperationName,
	check authorize.AuthorityCheck,
	scope tenancy.Scope,
	principal id.PrincipalID,
) authorize.AuthorityRead {
	t.Helper()
	registry, ok := authorize.LookupOperation(operation)
	if !ok {
		t.Fatalf("registry operation %q is not declared", operation)
	}
	read := findAuthority(t, reads, check)
	if read.Operation != operation {
		t.Fatalf("authority read operation = %q; want %q", read.Operation, operation)
	}
	if !reflect.DeepEqual(read.Required, registry.Requires) {
		t.Fatalf("authority read required = %+v; want registry grant %+v", read.Required, registry.Requires)
	}
	if read.Scope != scope {
		t.Fatalf("authority read scope = %+v; want %+v", read.Scope, scope)
	}
	if read.PrincipalID != principal {
		t.Fatalf("authority read principal = %q; want %q", read.PrincipalID, principal)
	}
	return read
}

func requireBindingConfigEvent(
	t *testing.T,
	events []event.Envelope,
	scope tenancy.Scope,
	subject string,
	subjectVersion uint64,
	actor id.PrincipalID,
	at time.Time,
) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("events = %d; want exactly one config.changed intent", len(events))
	}
	envelope := events[0]
	if envelope.Type != event.TypeConfigChanged {
		t.Fatalf("event type = %q; want %q", envelope.Type, event.TypeConfigChanged)
	}
	if envelope.ReportType != "" {
		t.Fatalf("config event carries report type %q", envelope.ReportType)
	}
	if envelope.Source.Kind != event.SourceShell {
		t.Fatalf("event source kind = %q; want %q", envelope.Source.Kind, event.SourceShell)
	}
	if envelope.Source.PrincipalID != actor {
		t.Fatalf("event source principal = %q; want %q", envelope.Source.PrincipalID, actor)
	}
	if !envelope.At.Equal(at) {
		t.Fatalf("event time = %v; want %v", envelope.At, at)
	}
	if envelope.Scope != scope {
		t.Fatalf("event scope = %+v; want %+v", envelope.Scope, scope)
	}
	if envelope.SubjectID != subject {
		t.Fatalf("event subject = %q; want %q", envelope.SubjectID, subject)
	}
	if envelope.SubjectVersion != subjectVersion {
		t.Fatalf("event subject version = %d; want %d", envelope.SubjectVersion, subjectVersion)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("event intent is not valid: %v", err)
	}
}

func requireBindingAudit(
	t *testing.T,
	audits []event.Audit,
	scope tenancy.Scope,
	actor id.PrincipalID,
	operation string,
	at time.Time,
) {
	t.Helper()
	if len(audits) != 1 {
		t.Fatalf("audit records = %d; want exactly one", len(audits))
	}
	audit := audits[0]
	if audit.ID == "" {
		t.Fatal("audit record has no identity")
	}
	if audit.Scope != scope {
		t.Fatalf("audit scope = %+v; want %+v", audit.Scope, scope)
	}
	if audit.Actor != actor {
		t.Fatalf("audit actor = %q; want %q", audit.Actor, actor)
	}
	if audit.Operation != operation {
		t.Fatalf("audit operation = %q; want %q", audit.Operation, operation)
	}
	if !audit.At.Equal(at) {
		t.Fatalf("audit time = %v; want %v", audit.At, at)
	}
}

func requireBindingReceipt(
	t *testing.T,
	record receipt.Record,
	tenant, project, principal, operation, callerKey string,
	content receipt.Content,
	at time.Time,
) {
	t.Helper()
	if record.ID == "" {
		t.Fatal("receipt has no identity")
	}
	if record.Key.TenantID != id.TenantID(tenant) {
		t.Fatalf("receipt tenant = %q; want %q", record.Key.TenantID, tenant)
	}
	if record.Key.ProjectID != id.ProjectID(project) {
		t.Fatalf("receipt project = %q; want %q", record.Key.ProjectID, project)
	}
	if record.Key.PrincipalID != id.PrincipalID(principal) {
		t.Fatalf("receipt principal = %q; want %q", record.Key.PrincipalID, principal)
	}
	if record.Key.Operation != operation {
		t.Fatalf("receipt operation = %q; want %q", record.Key.Operation, operation)
	}
	if record.Key.CallerKey != callerKey {
		t.Fatalf("receipt caller key = %q; want %q", record.Key.CallerKey, callerKey)
	}
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		t.Fatalf("receipt.Fingerprint: %v", err)
	}
	if record.ContentHash != fingerprint {
		t.Fatalf("receipt content hash = %q; want %q", record.ContentHash, fingerprint)
	}
	if !record.At.Equal(at) {
		t.Fatalf("receipt time = %v; want %v", record.At, at)
	}
}

func requirePublishPostCommit(t *testing.T, effects []event.PostCommitEffect) {
	t.Helper()
	if len(effects) != 1 {
		t.Fatalf("post-commit effects = %d; want exactly one publish_event", len(effects))
	}
	if effects[0].Kind != event.PostPublishEvent {
		t.Fatalf("post-commit kind = %q; want %q", effects[0].Kind, event.PostPublishEvent)
	}
}
