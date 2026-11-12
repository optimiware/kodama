package pipeline_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func projectScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

func tenantScope(tenant string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant)}
}

func humanActor(tenant, project, principal string, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      id.PrincipalID(principal),
		Kind:    identity.KindHuman,
		Scope:   projectScope(tenant, project),
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
}

func serviceActor(tenant, project, principal string, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      id.PrincipalID(principal),
		Kind:    identity.KindService,
		Scope:   projectScope(tenant, project),
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 1,
	}
}

func receiptContent(body string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(body)}
}

func digestFor(reference string) string {
	sum := sha256.Sum256([]byte(reference))
	return "sha256:" + hex.EncodeToString(sum[:])
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

func requireNoError(t *testing.T, err error, label string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

func requireAuthority(t *testing.T, reads []authorize.AuthorityRead, operation authorize.OperationName, check authorize.AuthorityCheck) authorize.AuthorityRead {
	t.Helper()
	registry, ok := authorize.LookupOperation(operation)
	if !ok {
		t.Fatalf("registry operation %q is not declared", operation)
	}
	for _, read := range reads {
		if read.Check != check {
			continue
		}
		if read.Operation != operation {
			t.Fatalf("authority read operation = %q; want %q", read.Operation, operation)
		}
		if !reflect.DeepEqual(read.Required, registry.Requires) {
			t.Fatalf("authority read required = %+v; want registry grant %+v", read.Required, registry.Requires)
		}
		return read
	}
	t.Fatalf("authority reads %v do not contain check %q", reads, check)
	return authorize.AuthorityRead{}
}

func requireNoAuthority(t *testing.T, reads []authorize.AuthorityRead, check authorize.AuthorityCheck) {
	t.Helper()
	for _, read := range reads {
		if read.Check == check {
			t.Fatalf("authority reads unexpectedly contain %q: %v", check, reads)
		}
	}
}

func requireReceipt(
	t *testing.T,
	record receipt.Record,
	scope tenancy.Scope,
	principal id.PrincipalID,
	operation, callerKey string,
	content receipt.Content,
	at time.Time,
) {
	t.Helper()
	if record.ID == "" {
		t.Fatal("receipt has no identity")
	}
	if record.Key.TenantID != scope.TenantID || record.Key.ProjectID != scope.ProjectID {
		t.Fatalf("receipt scope = %+v; want %+v", record.Key, scope)
	}
	if record.Key.PrincipalID != principal {
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

func requireAudit(
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

func requireConfigEvent(
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

func requirePublishPostCommit(t *testing.T, effects []event.PostCommitEffect) {
	t.Helper()
	if len(effects) != 1 {
		t.Fatalf("post-commit effects = %d; want exactly one publish_event", len(effects))
	}
	if effects[0].Kind != event.PostPublishEvent {
		t.Fatalf("post-commit kind = %q; want %q", effects[0].Kind, event.PostPublishEvent)
	}
}

// validPipelineYAML is the primary supported document: a container action with
// every supported field and a request action with literal and direct-lookup
// subject values.
func validPipelineYAML() []byte {
	return []byte(`inputs:
  - repository
  - key: retries
    kind: number
    required: false
env:
  LOG_LEVEL: info
actions:
  - name: build
    image: registry.example.com/build:latest
    commands:
      - make
      - |
        echo built
    env:
      APP_ENV: test
    secrets:
      - DEPLOY_TOKEN
    timeout: 10m
    outputs:
      - candidate
  - name: review
    request: role/reviewer
    timeout: 72h
    subject:
      operation: git.ref.update
      resource:
        from: inputs.repository
      candidate:
        from: build.outputs.candidate
    outputs:
      - approved
`)
}

func typedInputsYAML() []byte {
	return []byte(`inputs:
  - key: repository
    kind: string
    required: true
  - key: retries
    kind: number
  - key: dry_run
    kind: boolean
actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [make]
`)
}

func requestOnlyYAML() []byte {
	return []byte(`inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.repository
`)
}

func pushPipelineYAML() []byte {
	return []byte(`repository: repo-1
service: git-service
on:
  push:
    - refs/heads/main
    - refs/heads/release
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: test
    image: registry.example.com/test:latest
    commands: [test-commit]
`)
}

func pushInputs() map[id.InputKey]string {
	return map[id.InputKey]string{
		"repository":      "repo-1",
		"ref":             "refs/heads/main",
		"commit":          "0123456789abcdef",
		"previous_commit": "fedcba9876543210",
	}
}

func newAdmission(t *testing.T, resolver pipeline.Resolver, reader pipeline.Reader) *pipeline.AdmissionService {
	t.Helper()
	service, err := pipeline.NewAdmissionService(pipeline.AdmissionConfig{Resolver: resolver, Reader: reader})
	if err != nil {
		t.Fatalf("NewAdmissionService: %v", err)
	}
	if service == nil {
		t.Fatal("NewAdmissionService returned a nil service")
	}
	return service
}

func newManagement(t *testing.T, changes pipeline.ChangePort, reader pipeline.Reader) (*pipeline.Management, *fakeAuthorizer, *fakeAuthorityReader) {
	t.Helper()
	authorizer := &fakeAuthorizer{}
	authority := &fakeAuthorityReader{}
	management, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock:      fixedClock{now: testNow},
		IDs:        &sequentialIDs{},
		Authorizer: authorizer,
		Authority:  authority,
		Changes:    changes,
		Reader:     reader,
	})
	if err != nil {
		t.Fatalf("NewManagement: %v", err)
	}
	if management == nil {
		t.Fatal("NewManagement returned a nil management service")
	}
	return management, authorizer, authority
}

func newManagementWithAuthority(t *testing.T, changes pipeline.ChangePort, reader pipeline.Reader, authority authorize.AuthorityReader) *pipeline.Management {
	t.Helper()
	management, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock:      fixedClock{now: testNow},
		IDs:        &sequentialIDs{},
		Authorizer: &fakeAuthorizer{},
		Authority:  authority,
		Changes:    changes,
		Reader:     reader,
	})
	if err != nil {
		t.Fatalf("NewManagement: %v", err)
	}
	if management == nil {
		t.Fatal("NewManagement returned a nil management service")
	}
	return management
}

func requireDefinition(t *testing.T, got pipeline.Definition, want pipeline.Definition) {
	t.Helper()
	if got.ID != want.ID || got.Name != want.Name || got.Version != want.Version {
		t.Fatalf("definition = %+v; want %+v", got, want)
	}
	if !bytes.Equal(got.YAML, want.YAML) {
		t.Fatalf("definition YAML = %q; want %q", got.YAML, want.YAML)
	}
	if !reflect.DeepEqual(got.Push, want.Push) {
		t.Fatalf("definition push = %+v; want %+v", got.Push, want.Push)
	}
}

func requireAction(t *testing.T, actions []pipeline.AdmittedAction, index int, name string, kind definition.ActionKind, timeout time.Duration) pipeline.AdmittedAction {
	t.Helper()
	if index >= len(actions) {
		t.Fatalf("admitted actions = %d; want index %d", len(actions), index)
	}
	action := actions[index]
	if action.Index != index {
		t.Fatalf("action %d index = %d", index, action.Index)
	}
	if string(action.Name) != name {
		t.Fatalf("action %d name = %q; want %q", index, action.Name, name)
	}
	if action.Kind != kind {
		t.Fatalf("action %d kind = %q; want %q", index, action.Kind, kind)
	}
	if action.Timeout != timeout {
		t.Fatalf("action %d timeout = %s; want %s", index, action.Timeout, timeout)
	}
	if err := action.Validate(); err != nil {
		t.Fatalf("action %d is not valid: %v", index, err)
	}
	return action
}

func actionNames(actions []pipeline.AdmittedAction) []string {
	names := make([]string, len(actions))
	for index, action := range actions {
		names[index] = string(action.Name)
	}
	return names
}

func joinNames(actions []pipeline.AdmittedAction) string {
	return strings.Join(actionNames(actions), ",")
}

func capturedUnit(t *testing.T, port *fakeChangePort) pipeline.ChangeUnit {
	t.Helper()
	units := port.captured()
	if len(units) != 1 {
		t.Fatalf("change port captured %d units; want exactly 1", len(units))
	}
	return units[0]
}

func requireActions(t *testing.T, actions []pipeline.AdmittedAction, want int) {
	t.Helper()
	if len(actions) != want {
		t.Fatalf("admitted actions = %d; want %d", len(actions), want)
	}
}
