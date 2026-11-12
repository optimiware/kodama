package publiccontract

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// requirePublicError asserts that a refusal is an api.Error with the wanted
// stable public code. Decode/validate helpers never return a private cause as
// the public code.
func requirePublicError(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal with code %q; got nil", wantCode)
	}
	var publicError *api.Error
	if !errors.As(err, &publicError) {
		t.Fatalf("refusal %v is not an *api.Error", err)
	}
	if publicError.Code != wantCode {
		t.Fatalf("refusal code = %q; want %q", publicError.Code, wantCode)
	}
	if publicError.Message == "" {
		t.Fatalf("refusal code %q has an empty message", wantCode)
	}
}

// TestRequestEnvelopeRepresentation pins the single transport representation of
// input, expected version and idempotency key, plus its refusals.
func TestRequestEnvelopeRepresentation(t *testing.T) {
	version := uint64(3)
	request := api.Request{
		Input:           json.RawMessage(`{"ref":"refs/heads/main"}`),
		ExpectedVersion: &version,
		IdempotencyKey:  "caller-key-1",
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	for _, key := range []string{"input", "expected_version", "idempotency_key"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("request JSON is missing %q: %s", key, encoded)
		}
	}
	if len(keys) != 3 {
		t.Errorf("request JSON has keys %v; want exactly input/expected_version/idempotency_key", keys)
	}

	decoded, err := api.DecodeRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if decoded.ExpectedVersion == nil || *decoded.ExpectedVersion != 3 {
		t.Fatalf("decoded expected version = %v; want 3", decoded.ExpectedVersion)
	}
	if decoded.IdempotencyKey != "caller-key-1" {
		t.Fatalf("decoded idempotency key = %q", decoded.IdempotencyKey)
	}
	if string(decoded.Input) != `{"ref":"refs/heads/main"}` {
		t.Fatalf("decoded input = %s", decoded.Input)
	}

	minimal, err := api.DecodeRequest([]byte(`{}`))
	if err != nil {
		t.Fatalf("DecodeRequest({}) refused an empty request: %v", err)
	}
	if minimal.ExpectedVersion != nil || minimal.IdempotencyKey != "" || minimal.Input != nil {
		t.Fatalf("empty request = %+v; want zero value", minimal)
	}

	refusals := []string{
		`{"input":{},"extra":1}`,
		`{"input":[]}`,
		`{"input":"text"}`,
		`{"expected_version":0}`,
		`{"expected_version":"3"}`,
		`{"expected_version":-1}`,
		`{"idempotency_key":"has space"}`,
		`{"idempotency_key":"has/slash"}`,
		`{"idempotency_key":"café"}`,
		`{"idempotency_key":"` + strings.Repeat("k", api.MaxIdempotencyKeyBytes+1) + `"}`,
		`{} {}`,
		`[]`,
		`null`,
		`{"input":{}} trailing`,
		``,
	}
	for _, body := range refusals {
		_, err := api.DecodeRequest([]byte(body))
		requirePublicError(t, err, api.CodeInvalidInput)
	}

	oversized := []byte(`{"input":{},"idempotency_key":"` +
		strings.Repeat("k", api.MaxRequestBytes) + `"}`)
	if _, err := api.DecodeRequest(oversized); err == nil {
		t.Fatal("DecodeRequest accepted an oversized body")
	} else {
		requirePublicError(t, err, api.CodeInvalidInput)
	}

	if err := (api.Request{ExpectedVersion: uint64Pointer(0)}).Validate(); err == nil {
		t.Fatal("Request.Validate accepted expected_version 0")
	}
	// A valid idempotency key alphabet is exactly [A-Za-z0-9._:-].
	for _, key := range []string{"a", "A-1_z.9:x", strings.Repeat("k", api.MaxIdempotencyKeyBytes)} {
		if err := (api.Request{IdempotencyKey: key}).Validate(); err != nil {
			t.Errorf("valid idempotency key %q refused: %v", key, err)
		}
	}
}

// TestRESTStatusAndBodyCarriage pins the public-code -> HTTP status
// table, the single success status, the error/result response bodies and the
// rule that every REST method, including GET and DELETE, carries an api.Request
// JSON body whose input may carry api.Page limit/cursor.
func TestRESTStatusAndBodyCarriage(t *testing.T) {
	if api.HTTPStatusSuccess != 200 {
		t.Fatalf("HTTPStatusSuccess = %d; want 200", api.HTTPStatusSuccess)
	}
	codes := api.ErrorCodes()
	seen := make(map[int]string, len(codes))
	for _, code := range codes {
		status, ok := api.HTTPStatus(code)
		if !ok {
			t.Errorf("HTTPStatus(%q) is unmapped", code)
			continue
		}
		if status < 400 || status > 599 {
			t.Errorf("HTTPStatus(%q) = %d; want a 4xx or 5xx status", code, status)
		}
		if other, duplicate := seen[status]; duplicate {
			t.Errorf("HTTPStatus(%q) and HTTPStatus(%q) both use %d; want one status per code", code, other, status)
		}
		seen[status] = code
		if _, ok := api.MCPErrorCode(code); !ok {
			t.Errorf("MCP error map is missing public code %q", code)
		}
	}
	if len(seen) != len(codes) {
		t.Fatalf("HTTP status table covers %d codes; want %d", len(seen), len(codes))
	}
	for _, unknown := range []string{"", "teapot"} {
		if status, ok := api.HTTPStatus(unknown); ok {
			t.Errorf("HTTPStatus(%q) = %d; want refusal", unknown, status)
		}
	}

	successBody, err := json.Marshal(api.Result{Operation: "task.read", Body: json.RawMessage(`{"id":"task-1"}`)})
	if err != nil {
		t.Fatalf("marshal success result: %v", err)
	}
	if _, err := api.DecodeResult(successBody); err != nil {
		t.Errorf("success body is not an api.Result: %v", err)
	}
	errorBody, err := json.Marshal(api.Error{Code: api.CodeNotFound, Message: "missing"})
	if err != nil {
		t.Fatalf("marshal error body: %v", err)
	}
	var decodedError api.Error
	if err := json.Unmarshal(errorBody, &decodedError); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if err := decodedError.Validate(); err != nil {
		t.Fatalf("failure body is not an api.Error: %v", err)
	}
	if status, ok := api.HTTPStatus(decodedError.Code); !ok || status != 404 {
		t.Fatalf("HTTPStatus(%q) = %d/%v; want 404", decodedError.Code, status, ok)
	}

	cursor, err := api.EncodeCursor("task-0001")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	readBody, err := json.Marshal(map[string]any{
		"input":            map[string]any{"limit": 10, "cursor": cursor},
		"expected_version": 2,
		"idempotency_key":  "read-key-1",
	})
	if err != nil {
		t.Fatalf("marshal GET/DELETE request body: %v", err)
	}

	reads := 0
	for _, route := range api.Routes() {
		if route.Method != api.MethodGet && route.Method != api.MethodDelete {
			continue
		}
		reads++
		request, err := api.DecodeRequest(readBody)
		if err != nil {
			t.Errorf("%s %s refused an api.Request body: %v", route.Method, route.Path, err)
			continue
		}
		var page api.Page
		if err := json.Unmarshal(request.Input, &page); err != nil {
			t.Errorf("%s %s input is not an api.Page: %v", route.Method, route.Path, err)
			continue
		}
		if err := page.Validate(); err != nil {
			t.Errorf("%s %s page refused: %v", route.Method, route.Path, err)
		}
		if page.EffectiveLimit() != 10 || page.Cursor != cursor {
			t.Errorf("%s %s page = %+v; want limit 10 and the request cursor", route.Method, route.Path, page)
		}
		if request.ExpectedVersion == nil || *request.ExpectedVersion != 2 {
			t.Errorf("%s %s expected version = %v; want 2", route.Method, route.Path, request.ExpectedVersion)
		}
		if request.IdempotencyKey != "read-key-1" {
			t.Errorf("%s %s idempotency key = %q", route.Method, route.Path, request.IdempotencyKey)
		}
	}
	if reads == 0 {
		t.Fatal("no GET/DELETE routes to exercise the request-body carriage")
	}
}

// TestResultEnvelope pins the shared result representation and its bound.
func TestResultEnvelope(t *testing.T) {
	result := api.Result{Operation: "task.read", Body: json.RawMessage(`{"id":"task-1"}`), Replayed: true}
	if err := result.Validate(); err != nil {
		t.Fatalf("valid result refused: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	decoded, err := api.DecodeResult(encoded)
	if err != nil {
		t.Fatalf("DecodeResult: %v", err)
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("result round trip = %+v; want %+v", decoded, result)
	}

	for _, body := range []string{
		`{"operation":"task.read","body":{},"extra":1}`,
		`{"operation":"task.unknown"}`,
		`{"operation":""}`,
		`[]`,
		`{"operation":"task.read"} trailing`,
	} {
		_, err := api.DecodeResult([]byte(body))
		if err == nil {
			t.Errorf("DecodeResult(%s) was accepted; want refusal", body)
		}
	}

	oversized := []byte(`{"operation":"task.read","body":"` +
		strings.Repeat("x", api.MaxResponseBytes) + `"}`)
	if _, err := api.DecodeResult(oversized); err == nil {
		t.Fatal("DecodeResult accepted an oversized response body")
	}
}

// TestPaginationLimitsAndCursor pins the bounded page representation and the
// opaque, versioned, secret-free cursor.
func TestPaginationLimitsAndCursor(t *testing.T) {
	if api.DefaultPageLimit != 50 || api.MaxPageLimit != 200 {
		t.Fatalf("page limits = %d/%d; want 50/200", api.DefaultPageLimit, api.MaxPageLimit)
	}
	if err := (api.Page{}).Validate(); err != nil {
		t.Fatalf("empty page refused: %v", err)
	}
	if got := (api.Page{}).EffectiveLimit(); got != api.DefaultPageLimit {
		t.Fatalf("EffectiveLimit() = %d; want %d", got, api.DefaultPageLimit)
	}
	if got := (api.Page{Limit: 10}).EffectiveLimit(); got != 10 {
		t.Fatalf("EffectiveLimit(10) = %d; want 10", got)
	}
	for _, limit := range []int{-1, api.MaxPageLimit + 1, 1 << 20} {
		if err := (api.Page{Limit: limit}).Validate(); err == nil {
			t.Errorf("Page{Limit:%d}.Validate() accepted an out-of-range limit", limit)
		}
	}
	if err := (api.Page{Limit: api.MaxPageLimit}).Validate(); err != nil {
		t.Errorf("Page{Limit:%d}.Validate() refused the maximum: %v", api.MaxPageLimit, err)
	}
	if err := (api.Page{Cursor: strings.Repeat("A", api.MaxCursorBytes+1)}).Validate(); err == nil {
		t.Error("Page.Validate accepted an oversized cursor")
	}

	cursor, err := api.EncodeCursor("evt-0001")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	if len(cursor) > api.MaxCursorBytes {
		t.Fatalf("encoded cursor is %d bytes; bound is %d", len(cursor), api.MaxCursorBytes)
	}
	if strings.Contains(cursor, "evt-0001") || strings.Contains(cursor, "ten-1") || strings.Contains(cursor, "prn-1") {
		t.Fatalf("cursor %q leaks its sort key or scope", cursor)
	}
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatalf("cursor %q is not unpadded base64url: %v", cursor, err)
	}
	if !strings.Contains(string(payload), "evt-0001") {
		t.Fatalf("cursor payload %q does not carry the sort key", payload)
	}
	after, err := api.DecodeCursor(cursor)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if after != "evt-0001" {
		t.Fatalf("DecodeCursor = %q; want evt-0001", after)
	}

	if _, err := api.EncodeCursor(""); err == nil {
		t.Error("EncodeCursor accepted an empty sort key")
	}
	if _, err := api.EncodeCursor(strings.Repeat("x", 1024)); err == nil {
		t.Error("EncodeCursor accepted an oversized sort key")
	}
	otherVersion := base64.RawURLEncoding.EncodeToString([]byte("v2\nevt-0001"))
	for _, bad := range []string{"", "not base64!!", strings.Repeat("A", api.MaxCursorBytes+1), otherVersion, cursor + "="} {
		if _, err := api.DecodeCursor(bad); err == nil {
			t.Errorf("DecodeCursor(%q) was accepted; want refusal", bad)
		}
	}
}

// TestEventEnvelopeRoundTrip pins the public event envelope: it round trips,
// matches the domain wire shape, and enforces the public limits and
// report-type separation.
func TestEventEnvelopeRoundTrip(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, "test", "contracts", "public", "testdata", "wire", "event.json"))
	if err != nil {
		t.Fatalf("read event vector: %v", err)
	}
	var publicEvent api.Event
	if err := json.Unmarshal(data, &publicEvent); err != nil {
		t.Fatalf("unmarshal api.Event vector: %v", err)
	}
	if err := publicEvent.Validate(); err != nil {
		t.Fatalf("vector event refused: %v", err)
	}
	if publicEvent.Type != string(event.TypeTaskCreated) {
		t.Fatalf("vector type = %q; want task.created", publicEvent.Type)
	}
	if publicEvent.Source.Kind != string(event.SourceShell) {
		t.Fatalf("vector source = %q; want shell", publicEvent.Source.Kind)
	}
	if !reflect.DeepEqual(publicEvent.Data, []byte(`{"ok":true}`)) {
		t.Fatalf("vector data = %q; want {\"ok\":true}", publicEvent.Data)
	}

	var domainEvent event.Envelope
	if err := json.Unmarshal(data, &domainEvent); err != nil {
		t.Fatalf("vector does not unmarshal into the domain event.Envelope: %v", err)
	}
	if err := domainEvent.Validate(); err != nil {
		t.Fatalf("vector refused by the domain event.Envelope.Validate: %v", err)
	}
	if string(domainEvent.Type) != publicEvent.Type || domainEvent.SubjectID != publicEvent.SubjectID {
		t.Fatalf("public/domain event disagree: %+v vs %+v", publicEvent, domainEvent)
	}

	encoded, err := json.Marshal(publicEvent)
	if err != nil {
		t.Fatalf("marshal api.Event: %v", err)
	}
	var roundTripped api.Event
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal round trip: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, publicEvent) {
		t.Fatalf("event round trip = %+v; want %+v", roundTripped, publicEvent)
	}

	publicTypes := api.EventTypes()
	domainTypes := event.Types()
	if len(publicTypes) != 18 {
		t.Fatalf("public event types = %v; want the 18 canonical domain types", publicTypes)
	}
	domainSet := make(map[string]bool, len(domainTypes))
	for _, domainType := range domainTypes {
		domainSet[string(domainType)] = true
	}
	for index, publicType := range publicTypes {
		if !domainSet[publicType] {
			t.Errorf("public event type %d = %q is not a domain lifecycle type", index, publicType)
		}
		if publicType == "request.activated" {
			t.Errorf("public event types expose the domain-internal request.activated alias")
		}
	}
	// The public lifecycle name is action.requested; the domain's
	// request.activated alias is mapped to it and never accepted as a public
	// type.
	if !domainSet["action.requested"] || !domainSet["request.activated"] {
		t.Fatalf("domain lifecycle types = %v; want action.requested and its internal alias", domainTypes)
	}
	if canonical, ok := api.CanonicalEventType("request.activated"); !ok || canonical != "action.requested" {
		t.Errorf("CanonicalEventType(request.activated) = %q,%v; want action.requested,true", canonical, ok)
	}
	if canonical, ok := api.CanonicalEventType("action.requested"); !ok || canonical != "action.requested" {
		t.Errorf("CanonicalEventType(action.requested) = %q,%v; want action.requested,true", canonical, ok)
	}
	for _, unknown := range []string{"", "task.teleported", "request.resolved.alias"} {
		if canonical, ok := api.CanonicalEventType(unknown); ok {
			t.Errorf("CanonicalEventType(%q) = %q; want refusal", unknown, canonical)
		}
	}
	aliasEvent := publicEvent
	aliasEvent.Type = "request.activated"
	if err := aliasEvent.Validate(); err == nil {
		t.Error("public Event.Validate accepted the domain-internal request.activated alias")
	}
	publicSources := api.EventSources()
	domainSources := event.SourceKinds()
	if len(publicSources) != len(domainSources) || len(publicSources) != 3 {
		t.Fatalf("public event sources = %v; want the 3 domain sources", publicSources)
	}
	for index, domainSource := range domainSources {
		if publicSources[index] != string(domainSource) {
			t.Errorf("event source %d = %q; want %q", index, publicSources[index], domainSource)
		}
	}

	verified := publicEvent
	verified.ID = "evt-0002"
	verified.Type = string(event.TypeVerifiedReport)
	verified.ReportType = "git.ref.updated"
	if err := verified.Validate(); err != nil {
		t.Fatalf("verified report event refused: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*api.Event)
	}{
		{"unknown type", func(e *api.Event) { e.Type = "task.teleported" }},
		{"verified without report type", func(e *api.Event) { e.Type = string(event.TypeVerifiedReport); e.ReportType = "" }},
		{"report type on lifecycle event", func(e *api.Event) { e.ReportType = "git.ref.updated" }},
		{"lifecycle type as report type", func(e *api.Event) {
			e.Type = string(event.TypeVerifiedReport)
			e.ReportType = string(event.TypeCustomObservation)
		}},
		{"malformed report type", func(e *api.Event) {
			e.Type = string(event.TypeVerifiedReport)
			e.ReportType = "Git.Ref.Updated"
		}},
		{"zero time", func(e *api.Event) { e.At = time.Time{} }},
		{"empty subject", func(e *api.Event) { e.SubjectID = "" }},
		{"empty tenant", func(e *api.Event) { e.Scope.TenantID = "" }},
		{"invalid id", func(e *api.Event) { e.ID = "evt/0001" }},
		{"empty source principal", func(e *api.Event) { e.Source.PrincipalID = "" }},
		{"unknown source", func(e *api.Event) { e.Source.Kind = "broker" }},
		{"oversized data", func(e *api.Event) { e.Data = make([]byte, api.MaxEventDataBytes+1) }},
	}
	for _, testCase := range mutations {
		mutated := publicEvent
		testCase.mutate(&mutated)
		if err := mutated.Validate(); err == nil {
			t.Errorf("%s: Event.Validate accepted the mutation", testCase.name)
		}
	}
	if err := verified.Validate(); err != nil {
		t.Fatalf("verified report event refused before mutation loop: %v", err)
	}

	for _, value := range []string{"git.ref.updated", "resource.merged", "a.b-c_1"} {
		if !api.ReportTypeValid(value) {
			t.Errorf("ReportTypeValid(%q) = false", value)
		}
	}
	for _, value := range []string{"", "Git.Ref.Updated", "git ref updated", strings.Repeat("a", 129)} {
		if api.ReportTypeValid(value) {
			t.Errorf("ReportTypeValid(%q) = true", value)
		}
	}
}

// TestSubjectSchema pins the typed subject shape used by verify and report and
// proves it is wire-compatible with the domain verify.Subject.
func TestSubjectSchema(t *testing.T) {
	subject := api.Subject{Fields: []api.SubjectField{
		{Name: "operation", Kind: "string", Value: "git.ref.update"},
		{Name: "resource", Kind: "string", Value: "shop/web"},
		{Name: "ref", Kind: "string", Value: "refs/heads/main"},
		{Name: "expected_base", Kind: "string", Value: "0123abc"},
		{Name: "candidate", Kind: "string", Value: "4567def"},
		{Name: "force", Kind: "boolean", Value: "false"},
		{Name: "retries", Kind: "number", Value: "1.0"},
		{Name: "deploy", Kind: "null", Value: ""},
	}}
	if err := subject.Validate(); err != nil {
		t.Fatalf("valid subject refused: %v", err)
	}
	encoded, err := json.Marshal(subject)
	if err != nil {
		t.Fatalf("marshal subject: %v", err)
	}
	var domainSubject verify.Subject
	if err := json.Unmarshal(encoded, &domainSubject); err != nil {
		t.Fatalf("public subject does not unmarshal into the domain verify.Subject: %v", err)
	}
	if len(domainSubject.Fields) != len(subject.Fields) {
		t.Fatalf("domain subject has %d fields; want %d", len(domainSubject.Fields), len(subject.Fields))
	}
	for index, field := range domainSubject.Fields {
		if field.Name != subject.Fields[index].Name ||
			string(field.Kind) != subject.Fields[index].Kind ||
			field.Value != subject.Fields[index].Value {
			t.Fatalf("field %d = %+v; want %+v", index, field, subject.Fields[index])
		}
	}

	invalid := []struct {
		name    string
		subject api.Subject
	}{
		{"empty name", api.Subject{Fields: []api.SubjectField{{Name: "", Kind: "string", Value: "x"}}}},
		{"duplicate name", api.Subject{Fields: []api.SubjectField{
			{Name: "ref", Kind: "string", Value: "a"},
			{Name: "ref", Kind: "string", Value: "b"},
		}}},
		{"unknown kind", api.Subject{Fields: []api.SubjectField{{Name: "ref", Kind: "object", Value: "x"}}}},
		{"bad number", api.Subject{Fields: []api.SubjectField{{Name: "n", Kind: "number", Value: "01"}}}},
		{"non-finite number", api.Subject{Fields: []api.SubjectField{{Name: "n", Kind: "number", Value: "Infinity"}}}},
		{"bad boolean", api.Subject{Fields: []api.SubjectField{{Name: "b", Kind: "boolean", Value: "TRUE"}}}},
		{"null with value", api.Subject{Fields: []api.SubjectField{{Name: "z", Kind: "null", Value: "null"}}}},
		{"oversized value", api.Subject{Fields: []api.SubjectField{{Name: "s", Kind: "string", Value: strings.Repeat("x", api.MaxSubjectValueBytes+1)}}}},
	}
	for _, testCase := range invalid {
		if err := testCase.subject.Validate(); err == nil {
			t.Errorf("%s: Subject.Validate accepted the mutation", testCase.name)
		}
	}
	tooMany := make([]api.SubjectField, 0, api.MaxSubjectFields+1)
	for index := 0; index <= api.MaxSubjectFields; index++ {
		tooMany = append(tooMany, api.SubjectField{Name: "field-" + strings.Repeat("a", index+1), Kind: "string", Value: "x"})
	}
	if err := (api.Subject{Fields: tooMany}).Validate(); err == nil {
		t.Error("Subject.Validate accepted more than MaxSubjectFields")
	}
	if err := (api.Subject{}).Validate(); err != nil {
		t.Errorf("empty subject refused: %v", err)
	}
}

// TestOutputReferenceIsString pins that an output reference is an ordinary
// string and that no artifact entity or artifact operation exists.
func TestOutputReferenceIsString(t *testing.T) {
	output := api.Output{Name: "image", Reference: "registry.example.com/shop/web@sha256:" + strings.Repeat("a", 64)}
	if err := output.Validate(); err != nil {
		t.Fatalf("valid output refused: %v", err)
	}
	field, ok := reflect.TypeOf(api.Output{}).FieldByName("Reference")
	if !ok {
		t.Fatal("api.Output has no Reference field")
	}
	if field.Type.Kind() != reflect.String {
		t.Fatalf("Output.Reference kind = %s; want string", field.Type.Kind())
	}
	if err := (api.Output{Name: "image"}).Validate(); err == nil {
		t.Error("Output.Validate accepted an empty reference")
	}
	if err := (api.Output{Reference: "x"}).Validate(); err == nil {
		t.Error("Output.Validate accepted an empty name")
	}
	for _, route := range api.Routes() {
		if strings.Contains(strings.ToLower(route.Operation), "artifact") || strings.Contains(strings.ToLower(route.Path), "artifact") {
			t.Errorf("route %q/%q exposes an artifact entity", route.Operation, route.Path)
		}
	}
}
