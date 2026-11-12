package publiccontract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/resource"
)

// TestResourceOperationVocabulary pins the resource-operation vocabulary
// used by bindings, verification and reports. It is separate from the public
// router registry: `git.ref.update` is a resource operation and never a public
// operation.
func TestResourceOperationVocabulary(t *testing.T) {
	domain := resource.Operations()
	want := []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead}
	if len(domain) != len(want) || domain[0] != want[0] || domain[1] != want[1] {
		t.Fatalf("domain resource operations = %v; expected %v", domain, want)
	}
	public := api.ResourceOperations()
	if len(public) != len(domain) {
		t.Fatalf("api.ResourceOperations() = %v; want %v", public, domain)
	}
	for index, operation := range domain {
		if string(public[index]) != string(operation) {
			t.Errorf("resource operation %d = %q; want %q", index, public[index], operation)
		}
	}
	if api.ResourceOperationGitRefUpdate != "git.ref.update" {
		t.Fatalf("ResourceOperationGitRefUpdate = %q", api.ResourceOperationGitRefUpdate)
	}
	if api.ResourceOperationGitRead != "git.read" {
		t.Fatalf("ResourceOperationGitRead = %q", api.ResourceOperationGitRead)
	}
	if !api.ResourceOperationGitRefUpdate.Valid() || !api.ResourceOperationGitRead.Valid() {
		t.Error("a declared resource operation reported invalid")
	}
	for _, name := range []string{"", "git.ref.delete", "git.ref.read", "task.create", "resource.report", "Git.Ref.Update"} {
		if api.ResourceOperation(name).Valid() {
			t.Errorf("resource operation %q accepted", name)
		}
	}
	// Reads use the repository-level git.read operation, never a per-ref
	// git.ref.read operation.
	for _, operation := range api.ResourceOperations() {
		if operation == "git.ref.read" {
			t.Error("git.ref.read was added to the resource-operation vocabulary")
		}
	}
	for _, operation := range api.ResourceOperations() {
		if route, ok := api.LookupRoute(string(operation)); ok {
			t.Errorf("resource operation %q is also a public route %+v", operation, route)
		}
	}
}

// TestVerifyRequestSchema pins the service-plus-actor verification input. The
// forwarded actor credential is the only credential field and is bounded.
func TestVerifyRequestSchema(t *testing.T) {
	request := api.VerifyRequest{
		ActorCredential: "kodex.exec-1.signature",
		ResourceID:      "res-1",
		Operation:       "git.ref.update",
		Subject: api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: "git.ref.update"},
			{Name: "resource", Kind: "string", Value: "shop/web"},
			{Name: "ref", Kind: "string", Value: "refs/heads/main"},
			{Name: "expected_base", Kind: "string", Value: "base-1"},
			{Name: "candidate", Kind: "string", Value: "cand-1"},
		}},
		Approval: "role/reviewer",
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid verify request refused: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal verify request: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("unmarshal verify request: %v", err)
	}
	for _, key := range []string{"actor_credential", "resource_id", "operation", "subject", "approval"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("verify request JSON is missing %q: %s", key, encoded)
		}
	}
	if len(keys) != 5 {
		t.Errorf("verify request JSON keys = %v; want exactly five", keys)
	}

	decoded, err := api.DecodeVerifyRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeVerifyRequest(round trip): %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded verify request refused: %v", err)
	}

	refusals := []struct {
		name    string
		request api.VerifyRequest
	}{
		{"empty credential", api.VerifyRequest{ResourceID: "res-1", Operation: "git.ref.update"}},
		{"oversized credential", api.VerifyRequest{
			ActorCredential: strings.Repeat("k", api.MaxCredentialBytes+1),
			ResourceID:      "res-1",
			Operation:       "git.ref.update",
		}},
		{"invalid resource", api.VerifyRequest{ActorCredential: "k", ResourceID: "res/1", Operation: "git.ref.update"}},
		{"empty resource", api.VerifyRequest{ActorCredential: "k", Operation: "git.ref.update"}},
		{"public operation", api.VerifyRequest{ActorCredential: "k", ResourceID: "res-1", Operation: "task.create"}},
		{"empty operation", api.VerifyRequest{ActorCredential: "k", ResourceID: "res-1"}},
		{"service approval", api.VerifyRequest{
			ActorCredential: "k", ResourceID: "res-1", Operation: "git.ref.update", Approval: "service/svc-1",
		}},
		{"unknown approval role", api.VerifyRequest{
			ActorCredential: "k", ResourceID: "res-1", Operation: "git.ref.update", Approval: "role/root",
		}},
		{"uppercase approval role", api.VerifyRequest{
			ActorCredential: "k", ResourceID: "res-1", Operation: "git.ref.update", Approval: "role/Reviewer",
		}},
		{"extra approval segment", api.VerifyRequest{
			ActorCredential: "k", ResourceID: "res-1", Operation: "git.ref.update", Approval: "role/reviewer/x",
		}},
		{"padded approval", api.VerifyRequest{
			ActorCredential: "k", ResourceID: "res-1", Operation: "git.ref.update", Approval: " role/reviewer",
		}},
		{"invalid subject", api.VerifyRequest{
			ActorCredential: "k", ResourceID: "res-1", Operation: "git.ref.update",
			Subject: api.Subject{Fields: []api.SubjectField{{Name: "", Kind: "string", Value: "x"}}},
		}},
	}
	for _, testCase := range refusals {
		if err := testCase.request.Validate(); err == nil {
			t.Errorf("%s: VerifyRequest.Validate accepted the request", testCase.name)
		}
	}
	for _, approval := range []string{"role/admin", "role/engineer", "role/reviewer", "role/operator", "role/viewer"} {
		if err := (api.VerifyRequest{
			ActorCredential: "k", ResourceID: "res-1", Operation: "git.ref.update", Approval: approval,
		}).Validate(); err != nil {
			t.Errorf("valid approval %q refused: %v", approval, err)
		}
	}

	for _, body := range []string{
		`{"actor_credential":"k","resource_id":"res-1","operation":"git.ref.update","approval":"role/reviewer","extra":1}`,
		`{"actor_credential":"k","resource_id":"res-1","operation":"git.ref.update","subject":{"fields":[{"name":"x","kind":"string","value":"y","display":true}]}}`,
		`[]`,
	} {
		_, err := api.DecodeVerifyRequest([]byte(body))
		if err == nil {
			t.Errorf("DecodeVerifyRequest(%s) accepted unknown/extra fields", body)
		}
	}

	response := api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "prn-1", Kind: "human"},
		Roles:          []string{"reviewer"},
		Approval:       true,
		VerificationID: "ver-1",
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("valid verify response refused: %v", err)
	}
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal verify response: %v", err)
	}
	if strings.Contains(string(encodedResponse), "kodex.exec-1.signature") {
		t.Fatalf("verify response echoes the forwarded credential: %s", encodedResponse)
	}
	for _, testCase := range []api.VerifyResponse{
		{Principal: api.VerifiedPrincipal{ID: "", Kind: "human"}, VerificationID: "ver-1"},
		{Principal: api.VerifiedPrincipal{ID: "prn-1", Kind: "robot"}, VerificationID: "ver-1"},
		{Principal: api.VerifiedPrincipal{ID: "prn-1", Kind: "human"}, Roles: []string{"root"}, VerificationID: "ver-1"},
		{Principal: api.VerifiedPrincipal{ID: "prn-1", Kind: "human"}},
	} {
		if err := testCase.Validate(); err == nil {
			t.Errorf("VerifyResponse.Validate accepted %+v", testCase)
		}
	}
}

// TestReportRequestSchema pins the verified-report input: the binding-declared
// report type is separate from the trusted lifecycle type, only resource
// operations are accepted, and the report never carries actor authority or a
// credential.
func TestReportRequestSchema(t *testing.T) {
	request := api.ReportRequest{
		ResourceID:     "res-1",
		SourceEventID:  "git-event-1",
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: "ver-1",
		Subject: api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: "git.ref.update"},
			{Name: "resource", Kind: "string", Value: "shop/web"},
			{Name: "ref", Kind: "string", Value: "refs/heads/main"},
			{Name: "expected_base", Kind: "string", Value: "base-1"},
			{Name: "candidate", Kind: "string", Value: "cand-1"},
		}},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid report request refused: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal report request: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("unmarshal report request: %v", err)
	}
	for _, key := range []string{"resource_id", "source_event_id", "event_type", "operation", "verification_id", "subject"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("report request JSON is missing %q: %s", key, encoded)
		}
	}
	if len(keys) != 6 {
		t.Errorf("report request JSON keys = %v; want exactly six", keys)
	}
	decoded, err := api.DecodeReportRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeReportRequest(round trip): %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded report request refused: %v", err)
	}

	refusals := []struct {
		name    string
		request api.ReportRequest
	}{
		{"public operation", api.ReportRequest{
			ResourceID: "res-1", SourceEventID: "e-1", EventType: "git.ref.updated",
			Operation: "resource.report", VerificationID: "ver-1",
		}},
		{"lifecycle report type", api.ReportRequest{
			ResourceID: "res-1", SourceEventID: "e-1", EventType: string(event.TypeVerifiedReport),
			Operation: "git.ref.update", VerificationID: "ver-1",
		}},
		{"custom observation as report type", api.ReportRequest{
			ResourceID: "res-1", SourceEventID: "e-1", EventType: string(event.TypeCustomObservation),
			Operation: "git.ref.update", VerificationID: "ver-1",
		}},
		{"malformed report type", api.ReportRequest{
			ResourceID: "res-1", SourceEventID: "e-1", EventType: "Git.Ref.Updated",
			Operation: "git.ref.update", VerificationID: "ver-1",
		}},
		{"empty source event", api.ReportRequest{
			ResourceID: "res-1", EventType: "git.ref.updated", Operation: "git.ref.update", VerificationID: "ver-1",
		}},
		{"source event with slash", api.ReportRequest{
			ResourceID: "res-1", SourceEventID: "e/1", EventType: "git.ref.updated",
			Operation: "git.ref.update", VerificationID: "ver-1",
		}},
		{"missing verification", api.ReportRequest{
			ResourceID: "res-1", SourceEventID: "e-1", EventType: "git.ref.updated", Operation: "git.ref.update",
		}},
		{"invalid resource", api.ReportRequest{
			ResourceID: "res/1", SourceEventID: "e-1", EventType: "git.ref.updated",
			Operation: "git.ref.update", VerificationID: "ver-1",
		}},
		{"invalid subject", api.ReportRequest{
			ResourceID: "res-1", SourceEventID: "e-1", EventType: "git.ref.updated",
			Operation: "git.ref.update", VerificationID: "ver-1",
			Subject: api.Subject{Fields: []api.SubjectField{{Name: "ref", Kind: "blob", Value: "x"}}},
		}},
	}
	for _, testCase := range refusals {
		if err := testCase.request.Validate(); err == nil {
			t.Errorf("%s: ReportRequest.Validate accepted the request", testCase.name)
		}
	}

	for _, body := range []string{
		`{"resource_id":"res-1","source_event_id":"e-1","event_type":"git.ref.updated","operation":"git.ref.update","verification_id":"ver-1","approval":"role/reviewer"}`,
		`{"resource_id":"res-1","source_event_id":"e-1","event_type":"git.ref.updated","operation":"git.ref.update","verification_id":"ver-1","actor_credential":"kodex.x.y"}`,
		`{"resource_id":"res-1","source_event_id":"e-1","event_type":"git.ref.updated","operation":"git.ref.update","verification_id":"ver-1","extra":1}`,
	} {
		_, err := api.DecodeReportRequest([]byte(body))
		if err == nil {
			t.Errorf("DecodeReportRequest(%s) accepted actor authority or unknown fields", body)
		}
	}

	// Causation is taken from the verification record: the report schema has no
	// principal, approval, credential or token field at all.
	assertNoFieldMatching(t, reflect.TypeOf(api.ReportRequest{}), "credential", "token", "password", "secret", "approval", "actor", "principal", "role")
	assertNoFieldMatching(t, reflect.TypeOf(api.VerifyResponse{}), "credential", "token", "password", "secret")

	response := api.ReportResponse{VerificationID: "ver-1", EventID: "evt-1", Replayed: false}
	if err := response.Validate(); err != nil {
		t.Fatalf("valid report response refused: %v", err)
	}
	for _, invalid := range []api.ReportResponse{
		{EventID: "evt-1"},
		{VerificationID: "ver-1"},
		{VerificationID: "ver-1", EventID: "evt/1"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("ReportResponse.Validate accepted %+v", invalid)
		}
	}
}

// TestVerifiedReportEventSeparation pins the accepted report event: the trusted
// lifecycle type is always resource.verified and the binding-declared report
// type rides in ReportType, so a custom observation cannot impersonate it.
func TestVerifiedReportEventSeparation(t *testing.T) {
	accepted := api.Event{
		ID:             "evt-1",
		Type:           string(event.TypeVerifiedReport),
		ReportType:     "git.ref.updated",
		Source:         api.EventSource{Kind: string(event.SourceService), PrincipalID: "svc-1"},
		At:             timeFixture(),
		Scope:          api.EventScope{TenantID: "ten-1", ProjectID: "prj-1"},
		SubjectID:      "res-1",
		SubjectVersion: 4,
		Data:           []byte(`{"ref":"refs/heads/main"}`),
	}
	if err := accepted.Validate(); err != nil {
		t.Fatalf("accepted report event refused: %v", err)
	}

	// The same declared report type on a custom observation is not a lifecycle
	// type and must be refused.
	forged := accepted
	forged.Type = string(event.TypeCustomObservation)
	if err := forged.Validate(); err == nil {
		t.Fatal("custom observation carrying a report type was accepted")
	}

	// A lifecycle type is never a valid report type.
	for _, lifecycle := range event.Types() {
		if !api.ReportTypeValid(string(lifecycle)) {
			continue
		}
		// Grammar alone permits dotted lowercase names, so only the report
		// schema can refuse lifecycle types. Both the report request and the
		// verified envelope must refuse them.
		report := api.ReportRequest{
			ResourceID: "res-1", SourceEventID: "e-1", EventType: string(lifecycle),
			Operation: "git.ref.update", VerificationID: "ver-1",
		}
		if err := report.Validate(); err == nil {
			t.Errorf("ReportRequest accepted lifecycle type %q as a report type", lifecycle)
		}
		envelope := accepted
		envelope.ReportType = string(lifecycle)
		if err := envelope.Validate(); err == nil {
			t.Errorf("verified envelope accepted lifecycle type %q as a report type", lifecycle)
		}
	}

	// The domain keeps `request.activated` as an internal alias. The public
	// mapping canonicalizes it to action.requested. Grammar alone accepts the
	// dotted lowercase name, so only the report schemas can refuse it as a
	// lifecycle type; both must do so.
	aliasReport := api.ReportRequest{
		ResourceID: "res-1", SourceEventID: "e-1", EventType: "request.activated",
		Operation: "git.ref.update", VerificationID: "ver-1",
	}
	if err := aliasReport.Validate(); err == nil {
		t.Error("ReportRequest accepted the request.activated lifecycle alias as a report type")
	}
	aliasEnvelope := accepted
	aliasEnvelope.ReportType = "request.activated"
	if err := aliasEnvelope.Validate(); err == nil {
		t.Error("verified envelope accepted the request.activated lifecycle alias as a report type")
	}
}

// assertNoFieldMatching refuses any struct field whose lower-case name contains
// one of the forbidden fragments.
func assertNoFieldMatching(t *testing.T, structType reflect.Type, fragments ...string) {
	t.Helper()
	if structType.Kind() != reflect.Struct {
		t.Fatalf("%v is not a struct", structType)
	}
	for index := 0; index < structType.NumField(); index++ {
		name := strings.ToLower(structType.Field(index).Name)
		for _, fragment := range fragments {
			if strings.Contains(name, fragment) {
				t.Errorf("%v field %q carries forbidden %q authority", structType, structType.Field(index).Name, fragment)
			}
		}
	}
}

// timeFixture is the fixed timestamp used by wire vectors.
func timeFixture() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}
