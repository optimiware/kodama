package gitv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/optimiware/kodama/internal/domain"
)

const schemaURL = "https://kodama.invalid/contracts/git/v1/schema.json"

func strict[T any, P interface {
	*T
	Validate() error
}]() func([]byte) error {
	return func(b []byte) error { _, err := DecodeRequest[T, P](b); return err }
}

func tolerant[T any, P interface {
	*T
	Validate() error
}]() func([]byte) error {
	return func(b []byte) error { _, err := DecodeResponse[T, P](b); return err }
}

// decoders maps every message type to the strict (request, claims, stored
// records) or tolerant (response) decoder consumers use, so each fixture runs
// through the same code.
var decoders = map[string]func([]byte) error{
	"CreateRepositoryRequest":     strict[CreateRepositoryRequest](),
	"PatchRepositoryRequest":      strict[PatchRepositoryRequest](),
	"CredentialRequest":           strict[CredentialRequest](),
	"GrantClaims":                 strict[GrantClaims](),
	"ForwardingAssertion":         strict[ForwardingAssertion](),
	"WorkerIdentity":              strict[WorkerIdentity](),
	"PushAuthorizeRequest":        strict[PushAuthorizeRequest](),
	"CommitIntentRequest":         strict[CommitIntentRequest](),
	"OutcomeRequest":              strict[OutcomeRequest](),
	"InitializedRequest":          strict[InitializedRequest](),
	"InitializeRepositoryRequest": strict[InitializeRepositoryRequest](),
	"ReadRefsRequest":             strict[ReadRefsRequest](),
	"ReadRefValuesRequest":        strict[ReadRefValuesRequest](),
	"ReadJournalRequest":          strict[ReadJournalRequest](),
	"SetRetainedRefRequest":       strict[SetRetainedRefRequest](),
	"DeleteRetainedRefRequest":    strict[DeleteRetainedRefRequest](),
	"DeleteBranchRequest":         strict[DeleteBranchRequest](),
	"CreateBranchRequest":         strict[CreateBranchRequest](),
	"MaintainRequest":             strict[MaintainRequest](),
	"IsAncestorRequest":           strict[IsAncestorRequest](),
	"HasCommitRequest":            strict[HasCommitRequest](),
	"CommitTimeRequest":           strict[CommitTimeRequest](),
	"ReadFilesRequest":            strict[ReadFilesRequest](),
	"CreateBundleRequest":         strict[CreateBundleRequest](),
	"ReplayBundleRequest":         strict[ReplayBundleRequest](),
	"RestoreFenceRequest":         strict[RestoreFenceRequest](),
	"JournalEntry":                strict[JournalEntry](),
	"SourceSet":                   strict[SourceSet](),
	"TriggerConfig":               strict[TriggerConfig](),

	"Repository":                   tolerant[Repository](),
	"RepositoryList":               tolerant[RepositoryList](),
	"RefsResponse":                 tolerant[RefsResponse](),
	"CredentialResponse":           tolerant[CredentialResponse](),
	"PushAuthorizeResponse":        tolerant[PushAuthorizeResponse](),
	"CommitIntentResponse":         tolerant[CommitIntentResponse](),
	"OutcomeResponse":              tolerant[OutcomeResponse](),
	"InitializedResponse":          tolerant[InitializedResponse](),
	"AssignmentsResponse":          tolerant[AssignmentsResponse](),
	"InitializeRepositoryResponse": tolerant[InitializeRepositoryResponse](),
	"ReadRefValuesResponse":        tolerant[ReadRefValuesResponse](),
	"ReadJournalResponse":          tolerant[ReadJournalResponse](),
	"ReceiveLivenessResponse":      tolerant[ReceiveLivenessResponse](),
	"SetRetainedRefResponse":       tolerant[SetRetainedRefResponse](),
	"DeleteRetainedRefResponse":    tolerant[DeleteRetainedRefResponse](),
	"DeleteBranchResponse":         tolerant[DeleteBranchResponse](),
	"CreateBranchResponse":         tolerant[CreateBranchResponse](),
	"MaintainResponse":             tolerant[MaintainResponse](),
	"IsAncestorResponse":           tolerant[IsAncestorResponse](),
	"HasCommitResponse":            tolerant[HasCommitResponse](),
	"CommitTimeResponse":           tolerant[CommitTimeResponse](),
	"ReadFilesResponse":            tolerant[ReadFilesResponse](),
	"CreateBundleResponse":         tolerant[CreateBundleResponse](),
	"ReplayBundleResponse":         tolerant[ReplayBundleResponse](),
	"RestoreFenceResponse":         tolerant[RestoreFenceResponse](),
	"PushRecord":                   tolerant[PushRecord](),
	"PushRecordList":               tolerant[PushRecordList](),
	"ErrorResponse":                tolerant[ErrorResponse](),
}

func codeOf(err error) string {
	var ce *ContractError
	if errors.As(err, &ce) {
		return string(ce.Code)
	}
	return "?"
}

func mustRead(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newCompiler(t testing.TB) *jsonschema.Compiler {
	t.Helper()
	c := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(mustRead(t, "schema.json")))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddResource(schemaURL, doc); err != nil {
		t.Fatal(err)
	}
	return c
}

func schemaVerdict(t testing.TB, c *jsonschema.Compiler, typeName string, payload []byte) bool {
	t.Helper()
	sch, err := c.Compile(schemaURL + "#/$defs/" + typeName)
	if err != nil {
		t.Fatalf("compile %s: %v", typeName, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	return sch.Validate(inst) == nil
}

func writeOrCompare(t *testing.T, rel string, body []byte) {
	t.Helper()
	if os.Getenv("KODAMA_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(rel, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := mustRead(t, rel); !bytes.Equal(want, body) {
		t.Fatalf("%s is out of date; regenerate with KODAMA_UPDATE_GOLDEN=1", rel)
	}
}

type payloadCase struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Raw         *string         `json:"raw,omitempty"`
	SchemaValid *bool           `json:"schemaValid,omitempty"`
	Valid       bool            `json:"valid"`
	Code        string          `json:"code,omitempty"`
	GoOnly      bool            `json:"goOnly,omitempty"`
}

func areaOf(t testing.TB, area string) []draftCase {
	for _, a := range allCases(t) {
		if a.area == area {
			return a.cases
		}
	}
	t.Fatalf("no fixture area %s", area)
	return nil
}

// runArea runs every fixture of an area through its Go decoder and the JSON
// Schema, checks that the two agree where the schema can express the rule, and
// pins the published file.
func runArea(t *testing.T, area string) {
	t.Helper()
	compiler := newCompiler(t)
	var published []payloadCase
	seen := map[string]bool{}
	var valid, invalid int
	for _, c := range areaOf(t, area) {
		if seen[c.name] {
			t.Fatalf("duplicate case %s", c.name)
		}
		seen[c.name] = true
		var body []byte
		if c.raw != nil {
			body = []byte(*c.raw)
		} else {
			var err error
			if body, err = json.Marshal(c.payload); err != nil {
				t.Fatal(err)
			}
		}
		pc := payloadCase{Name: c.name, Type: c.typ, Valid: c.valid, Code: c.code, GoOnly: c.goOnly}
		t.Run(c.name, func(t *testing.T) {
			decode := decoders[c.typ]
			if decode == nil {
				t.Fatalf("no decoder for %s", c.typ)
			}
			err := decode(body)
			if (err == nil) != c.valid {
				t.Fatalf("Go contract: valid=%v, fixture expects %v (%v)", err == nil, c.valid, err)
			}
			if err != nil && codeOf(err) != c.code {
				t.Fatalf("code %q, want %q (%v)", codeOf(err), c.code, err)
			}
			if c.raw != nil {
				return
			}
			verdict := schemaVerdict(t, compiler, c.typ, body)
			switch {
			case c.valid && !verdict:
				t.Errorf("Go accepts a payload the schema rejects")
			case !c.valid && !c.goOnly && verdict:
				t.Errorf("the schema accepts a payload that this fixture claims it refuses (mark it goOnly if the schema cannot express the rule)")
			case !c.valid && c.goOnly && !verdict:
				t.Errorf("fixture is marked goOnly but the schema refuses it too; drop the mark")
			}
		})
		if c.raw != nil {
			pc.Raw = c.raw
		} else {
			pc.Payload = body
			verdict := schemaVerdict(t, compiler, c.typ, body)
			pc.SchemaValid = &verdict
		}
		published = append(published, pc)
		if c.valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid == 0 || invalid == 0 {
		t.Fatalf("area %s needs positive and negative fixtures (%d, %d)", area, valid, invalid)
	}
	out, err := json.MarshalIndent(published, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeOrCompare(t, "fixtures/"+area+".json", append(out, '\n'))
}

func ts(t testing.TB, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func decodeGrant(t testing.TB, g obj) GrantClaims {
	t.Helper()
	b, _ := json.Marshal(g)
	v, err := DecodeRequest[GrantClaims](b)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return v
}

func decodeJSON[T any](t testing.TB, v any) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func wantCode(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want %s", code)
	}
	if got := codeOf(err); got != string(code) {
		t.Fatalf("code %q (%v), want %q", got, err, code)
	}
}

// ---- cross-cutting contract tests ----

// Test_unit_git_contract_schema_patterns: the schema's string patterns equal the
// Go patterns, and the negated "forbidden" sequences are the Go ones.
func Test_unit_git_contract_schema_patterns(t *testing.T) {
	var doc struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
			AllOf   []struct {
				Not struct {
					Pattern string `json:"pattern"`
				} `json:"not"`
			} `json:"allOf"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(mustRead(t, "schema.json"), &doc); err != nil {
		t.Fatal(err)
	}
	for name, pattern := range Patterns {
		if doc.Defs[name].Pattern != pattern {
			t.Errorf("pattern %s differs: schema %q, Go %q", name, doc.Defs[name].Pattern, pattern)
		}
	}
	for _, name := range []string{"BranchName", "RefName", "RefPattern", "BranchRef"} {
		found := false
		for _, a := range doc.Defs[name].AllOf {
			found = found || a.Not.Pattern == Forbidden
		}
		if !found {
			t.Errorf("%s does not exclude the forbidden ref sequences", name)
		}
	}
}

// Test_unit_git_contract_coverage: every operation names decodable DTOs with a
// schema definition, every request has a negative fixture, every response a
// positive one, and every reason and error code of the contract is defined by
// the schema.
func Test_unit_git_contract_coverage(t *testing.T) {
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(mustRead(t, "schema.json"), &doc); err != nil {
		t.Fatal(err)
	}
	validTypes, invalidTypes := map[string]bool{}, map[string]bool{}
	for _, a := range allCases(t) {
		for _, c := range a.cases {
			if c.valid {
				validTypes[c.typ] = true
			} else {
				invalidTypes[c.typ] = true
			}
		}
	}
	for name := range decoders {
		if _, ok := doc.Defs[name]; !ok {
			t.Errorf("%s has a decoder but no schema definition", name)
		}
	}
	ids := map[string]bool{}
	for _, op := range Operations {
		if ids[op.ID] {
			t.Errorf("duplicate operation %s", op.ID)
		}
		ids[op.ID] = true
		var types []string
		if op.Request != "" {
			types = append(types, op.Request)
			if !invalidTypes[op.Request] {
				t.Errorf("%s: no negative fixture for %s", op.ID, op.Request)
			}
		}
		for status, name := range op.Responses {
			types = append(types, name)
			if !validTypes[name] {
				t.Errorf("%s: no positive fixture for %s", op.ID, name)
			}
			if status < 200 || status > 299 {
				t.Errorf("%s: response status %d is not a success", op.ID, status)
			}
		}
		for _, name := range types {
			if decoders[name] == nil {
				t.Errorf("%s names %s, which has no decoder", op.ID, name)
			}
		}
		for _, code := range op.Errors {
			if _, ok := Status(code); !ok {
				t.Errorf("%s lists unknown code %s", op.ID, code)
			}
		}
		if op.Method == "GET" && op.Request != "" {
			t.Errorf("%s: a GET has no body", op.ID)
		}
	}
	for _, name := range []string{"GrantClaims", "ForwardingAssertion", "WorkerIdentity", "PushRecord", "JournalEntry", "SourceSet", "TriggerConfig"} {
		if !validTypes[name] || !invalidTypes[name] {
			t.Errorf("%s needs a positive and a negative fixture", name)
		}
	}
	// Every error code and reason code the Go contract defines is in the schema.
	var enums map[string]struct {
		Enum []string `json:"enum"`
	}
	raw, _ := json.Marshal(doc.Defs)
	if err := json.Unmarshal(raw, &enums); err != nil {
		t.Fatal(err)
	}
	var codes []string
	for code := range statuses {
		codes = append(codes, string(code))
	}
	sort.Strings(codes)
	if got := append([]string{}, enums["ErrorCode"].Enum...); !sameSet(got, codes) {
		t.Errorf("schema ErrorCode %v differs from Status table %v", got, codes)
	}
	if !sameSet(enums["PushRefusalReason"].Enum, pushRefusalReasons) {
		t.Errorf("schema PushRefusalReason differs from the Go reasons")
	}
	if !sameSet(enums["RefusedEntryReason"].Enum, append(append([]string{}, pushRefusalReasons...), settlementRefusalReasons...)) {
		t.Errorf("schema RefusedEntryReason differs from the Go reasons")
	}
	if !sameSet(enums["UnknownEntryReason"].Enum, unknownReasons) {
		t.Errorf("schema UnknownEntryReason differs from the Go reasons")
	}
}

func sameSet(a, b []string) bool {
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, ",") == strings.Join(y, ",")
}

// Test_unit_git_contract_error_statuses freezes the HTTP status of each code.
func Test_unit_git_contract_error_statuses(t *testing.T) {
	want := map[domain.ErrorCode]int{
		CodeRepositoryNameTaken: 409, CodeRepositoryNotReady: 409, CodeRepositoryArchived: 409, CodeHostingMismatch: 422,
		CodeRepositoryNotEmpty: 409, CodeSeedPending: 409, CodePushSettled: 409, CodeAttemptInactive: 409, CodeReceiveExpired: 409,
		CodeRefChanged: 409, CodeRetainedRefExists: 409, CodeBranchExists: 409, CodeTooManyUpdates: 422, CodeJournalUnreadable: 409,
		CodeCommitNotFound: 404, CodeRepositoryEmpty: 409, CodeBundleTooLarge: 422, CodeRestoreFenceRequired: 409,
		domain.CodeInvalidRequest: 400, domain.CodeInvalidCredential: 401, domain.CodeForbidden: 403, domain.CodeNotFound: 404,
	}
	for code, status := range want {
		if got, ok := Status(code); !ok || got != status {
			t.Errorf("Status(%s) = %d, %v; want %d", code, got, ok, status)
		}
	}
	if _, ok := Status("made_up"); ok {
		t.Error("an unknown code has a status")
	}
}

// Test_unit_git_contract_journal_unreadable_code: the journal read has a
// definite, non-transient code for an affirmatively unreadable journal,
// distinct from the 503 unavailable of transient inability.
func Test_unit_git_contract_journal_unreadable_code(t *testing.T) {
	if s, ok := Status(CodeJournalUnreadable); !ok || s != 409 {
		t.Fatalf("Status(journal_unreadable) = %d, %v; want 409", s, ok)
	}
	if u, _ := Status(domain.CodeUnavailable); u != 503 || CodeJournalUnreadable == domain.CodeUnavailable {
		t.Fatalf("unavailable must stay the distinct 503 code (%d)", u)
	}
	var op *Operation
	for i := range Operations {
		if Operations[i].ID == "readJournal" {
			op = &Operations[i]
		}
	}
	if op == nil {
		t.Fatal("readJournal is not an operation")
	}
	listed, transient := false, false
	for _, c := range op.Errors {
		listed = listed || c == CodeJournalUnreadable
		transient = transient || c == domain.CodeUnavailable
	}
	if !listed || !transient {
		t.Errorf("readJournal errors %v must list journal_unreadable and unavailable", op.Errors)
	}
	// The error body decodes and validates with the new code.
	body := []byte(`{"error":{"code":"journal_unreadable","message":"the ref journal is unreadable","requestId":"req_1","details":{}}}`)
	resp, err := DecodeResponse[ErrorResponse](body)
	if err != nil || resp.Error.Code != CodeJournalUnreadable {
		t.Fatalf("decode: %+v %v", resp, err)
	}
}
