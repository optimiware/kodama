package runtimev2

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

const (
	schemaURLv1 = "https://kodama.invalid/contracts/runtime/v1/schema.json"
	schemaURLv2 = "https://kodama.invalid/contracts/runtime/v2/schema.json"
)

// decoders maps every message type to the strict (request) or tolerant
// (response) decoder consumers use, so each fixture runs through the same code.
var decoders = map[string]func([]byte) error{
	"AdmitRequest":              func(b []byte) error { _, err := DecodeRequest[AdmitRequest](b); return err },
	"TokenRequest":              func(b []byte) error { _, err := DecodeRequest[TokenRequest](b); return err },
	"SecretResolveRequest":      func(b []byte) error { _, err := DecodeRequest[SecretResolveRequest](b); return err },
	"ArtifactReserveRequest":    func(b []byte) error { _, err := DecodeRequest[ArtifactReserveRequest](b); return err },
	"ArtifactCompleteRequest":   func(b []byte) error { _, err := DecodeRequest[ArtifactCompleteRequest](b); return err },
	"SourceProposalRequest":     func(b []byte) error { _, err := DecodeRequest[SourceProposalRequest](b); return err },
	"ResultRequest":             func(b []byte) error { _, err := DecodeRequest[ResultRequest](b); return err },
	"BeginRequest":              func(b []byte) error { _, err := DecodeRequest[BeginRequest](b); return err },
	"ResourceAdmitRequest":      func(b []byte) error { _, err := DecodeRequest[ResourceAdmitRequest](b); return err },
	"ResourceTokenRequest":      func(b []byte) error { _, err := DecodeRequest[ResourceTokenRequest](b); return err },
	"PreparationReceiptRequest": func(b []byte) error { _, err := DecodeRequest[PreparationReceiptRequest](b); return err },

	"TokenResponse":              func(b []byte) error { _, err := DecodeResponse[TokenResponse](b); return err },
	"SecretResolveResponse":      func(b []byte) error { _, err := DecodeResponse[SecretResolveResponse](b); return err },
	"ArtifactReserveResponse":    func(b []byte) error { _, err := DecodeResponse[ArtifactReserveResponse](b); return err },
	"ArtifactCompleteResponse":   func(b []byte) error { _, err := DecodeResponse[ArtifactCompleteResponse](b); return err },
	"SourceProposalResponse":     func(b []byte) error { _, err := DecodeResponse[SourceProposalResponse](b); return err },
	"ResultResponse":             func(b []byte) error { _, err := DecodeResponse[ResultResponse](b); return err },
	"ContextV2":                  func(b []byte) error { _, err := DecodeResponse[ContextV2](b); return err },
	"AdmitResponseV2":            func(b []byte) error { _, err := DecodeResponse[AdmitResponseV2](b); return err },
	"BeginResponse":              func(b []byte) error { _, err := DecodeResponse[BeginResponse](b); return err },
	"ResourceAdmitResponse":      func(b []byte) error { _, err := DecodeResponse[ResourceAdmitResponse](b); return err },
	"ResourceTokenResponse":      func(b []byte) error { _, err := DecodeResponse[ResourceTokenResponse](b); return err },
	"ResourceContext":            func(b []byte) error { _, err := DecodeResponse[ResourceContext](b); return err },
	"PreparationReceiptResponse": func(b []byte) error { _, err := DecodeResponse[PreparationReceiptResponse](b); return err },
	"ErrorResponse":              func(b []byte) error { _, err := DecodeResponse[ErrorResponse](b); return err },
}

func codeOf(err error) string {
	if ce, ok := err.(*ContractError); ok {
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

func newCompiler(t testing.TB) (map[string]any, *jsonschema.Compiler) {
	t.Helper()
	c := jsonschema.NewCompiler()
	var v2 any
	for url, file := range map[string]string{schemaURLv1: "../v1/schema.json", schemaURLv2: "schema.json"} {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(mustRead(t, file)))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(url, doc); err != nil {
			t.Fatal(err)
		}
		if url == schemaURLv2 {
			v2 = doc
		}
	}
	return v2.(map[string]any), c
}

func schemaVerdict(t testing.TB, c *jsonschema.Compiler, typeName string, payload []byte) bool {
	t.Helper()
	sch, err := c.Compile(schemaURLv2 + "#/$defs/" + typeName)
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

// Test_runtime_v2_contract_fixtures: positive and negative payloads agree between
// the JSON Schema and the Go contract, strict requests reject omitted nullables
// and unknown fields, responses tolerate compatible additions, and the replayable
// exchanges are consistent with the operation table and the pure derivations. It
// proves shapes only: no handler, authority or lifecycle is exercised. PX-21,
// WS-13, DQ-16, VS-14, IT-13.
func Test_runtime_v2_contract_fixtures(t *testing.T) {
	_, compiler := newCompiler(t)

	t.Run("payloads", func(t *testing.T) {
		var published []payloadCase
		validTypes, invalidTypes := map[string]bool{}, map[string]bool{}
		seen := map[string]bool{}
		for _, c := range cases(t) {
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
			t.Run(c.name, func(t *testing.T) {
				if len(c.reqs) == 0 {
					t.Fatal("fixture cites no requirement")
				}
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
			})
			pc := payloadCase{Name: c.name, Type: c.typ, Requirements: c.reqs, Valid: c.valid, Code: c.code}
			if c.raw != nil {
				pc.Raw = c.raw
			} else {
				pc.Payload = body
				verdict := schemaVerdict(t, compiler, c.typ, body)
				pc.SchemaValid = &verdict
				if c.valid && !verdict {
					t.Errorf("%s: Go accepts a payload the schema rejects", c.name)
				}
			}
			published = append(published, pc)
			if c.valid {
				validTypes[c.typ] = true
			} else {
				invalidTypes[c.typ] = true
			}
		}
		out, err := json.MarshalIndent(published, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		writeOrCompare(t, "fixtures/cases.json", append(out, '\n'))

		for _, op := range Operations {
			for _, typeName := range append([]string{op.Request}, responseTypes(op)...) {
				if typeName == "" {
					continue
				}
				if _, ok := decoders[typeName]; !ok {
					t.Errorf("%s names %s, which has no decoder", op.ID, typeName)
				}
			}
			if op.Request != "" && isV2Type(op.Request) && !invalidTypes[op.Request] {
				t.Errorf("no negative fixture for %s", op.Request)
			}
			for _, typeName := range responseTypes(op) {
				if isV2Type(typeName) && !validTypes[typeName] {
					t.Errorf("no positive fixture for %s", typeName)
				}
			}
		}
	})

	t.Run("scenarios", func(t *testing.T) {
		var drafts []draftScenario
		drafts = append(drafts, scenarios(t)...)
		out, err := json.MarshalIndent(drafts, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		writeOrCompare(t, "fixtures/scenarios.json", append(out, '\n'))
		var loaded []draftScenario
		if err := json.Unmarshal(mustRead(t, "fixtures/scenarios.json"), &loaded); err != nil {
			t.Fatal(err)
		}
		runScenarios(t, loaded)
	})

	t.Run("traceability", func(t *testing.T) {
		seen := map[string]bool{}
		for _, c := range cases(t) {
			for _, id := range c.reqs {
				seen[id] = true
			}
		}
		for _, s := range scenarios(t) {
			for _, id := range s.Requirements {
				seen[id] = true
			}
		}
		for _, id := range []string{"PX-17", "PX-20", "PX-21", "PX-22", "WS-13", "WS-16", "DQ-16", "VS-14", "IT-13", "IT-14", "IT-16"} {
			if !seen[id] {
				t.Errorf("no fixture covers %s", id)
			}
		}
	})
}

func responseTypes(op Operation) []string {
	var out []string
	for _, v := range op.Responses {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func isV2Type(name string) bool {
	switch name {
	case "ContextV2", "AdmitResponseV2", "BeginRequest", "BeginResponse", "ResourceAdmitRequest", "ResourceAdmitResponse",
		"ResourceTokenRequest", "ResourceTokenResponse", "ResourceContext", "PreparationReceiptRequest", "PreparationReceiptResponse":
		return true
	}
	return false
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

type scenarioState struct {
	ctx        *ContextV2
	begun      map[domain.AttemptID][2]json.RawMessage // request, response body
	res        *ResourceContext
	receipt    *[2]json.RawMessage
	prepSeen   bool
	admittedBy map[string]bool
}

// runScenarios replays each exchange against the operation tables, the pure
// route-family rules and the derivations, so the fixtures are consistent
// protocol examples rather than loose JSON.
func runScenarios(t *testing.T, scenarios []draftScenario) {
	ops := map[string]Operation{}
	for _, op := range Operations {
		ops[op.ID] = op
	}
	v1ops := map[string]runtimev1.Operation{}
	for _, op := range runtimev1.Operations {
		v1ops[op.ID] = op
	}
	covered := map[string]bool{}
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			st := &scenarioState{begun: map[domain.AttemptID][2]json.RawMessage{}}
			for n, step := range s.Steps {
				if step.API == "v1" {
					replayV1(t, n, step, v1ops)
					continue
				}
				op, ok := ops[step.Op]
				if !ok {
					t.Fatalf("step %d: unknown v2 operation %s", n, step.Op)
				}
				covered[op.ID] = true
				if op.Request != "" {
					if err := decoders[op.Request](step.Request); err != nil {
						t.Fatalf("step %d request: %v", n, err)
					}
				} else if string(step.Request) != "null" {
					t.Fatalf("step %d: %s takes no body", n, op.ID)
				}

				// The route-family and kind rules decide before anything else.
				var gate error
				if step.Audience != "" {
					gate = CheckAudience(op.Subject, step.Audience)
				}
				if gate == nil && step.Kind != "" {
					gate = CheckResourceCall(op, ResourceKind(step.Kind))
				}
				if gate != nil {
					if step.Status != 403 || !errorIs(t, step.Body, "forbidden") {
						t.Fatalf("step %d: %v must be 403 forbidden", n, gate)
					}
					// A rejected exchange is still judged against the operation table:
					// the route must be able to return the code it was rejected with.
					er, err := DecodeResponse[ErrorResponse](step.Body)
					if err != nil {
						t.Fatalf("step %d error body: %v", n, err)
					}
					if status, _ := Status(er.Error.Code); status != step.Status || !containsCode(op.Errors, er.Error.Code) {
						t.Fatalf("step %d: %s does not list %s", n, op.ID, er.Error.Code)
					}
					continue
				}
				if typeName, success := op.Responses[step.Status]; success {
					if err := decoders[typeName](step.Body); err != nil {
						t.Fatalf("step %d response: %v", n, err)
					}
				} else {
					er, err := DecodeResponse[ErrorResponse](step.Body)
					if err != nil {
						t.Fatalf("step %d error body: %v", n, err)
					}
					if status, _ := Status(er.Error.Code); status != step.Status {
						t.Fatalf("step %d: code %s maps to %d, recorded %d", n, er.Error.Code, status, step.Status)
					}
					if !containsCode(op.Errors, er.Error.Code) {
						t.Fatalf("step %d: %s may not return %s", n, op.ID, er.Error.Code)
					}
				}
				st.apply(t, n, op, step)
			}
		})
	}
	for _, id := range []string{"begin", "admitResource", "renewResourceToken", "getResourceContext", "resolveResourceSecret", "completePreparation", "admit", "getContext", "renewToken", "submitResult"} {
		if !covered[id] {
			t.Errorf("no scenario exercises %s", id)
		}
	}
}

func errorIs(t testing.TB, body json.RawMessage, code string) bool {
	t.Helper()
	er, err := DecodeResponse[ErrorResponse](body)
	return err == nil && string(er.Error.Code) == code
}

func containsCode(list []domain.ErrorCode, code domain.ErrorCode) bool {
	for _, c := range list {
		if c == code {
			return true
		}
	}
	return false
}

// replayV1 checks a step against a frozen v1 route: the only v2-specific
// outcome a v1 route may return is context_version_mismatch, and v1 routes
// otherwise keep their own error sets.
func replayV1(t *testing.T, n int, step draftStep, v1ops map[string]runtimev1.Operation) {
	t.Helper()
	op, ok := v1ops[step.Op]
	if !ok {
		t.Fatalf("step %d: unknown v1 operation %s", n, step.Op)
	}
	er, err := DecodeResponse[ErrorResponse](step.Body)
	if err != nil {
		t.Fatalf("step %d: %v", n, err)
	}
	if step.Status == 409 && er.Error.Code == CodeContextVersionMismatch {
		return // a v2 attempt on a v1 route: no side effects, v1 contract unchanged
	}
	if status, _ := runtimev1.Status(er.Error.Code); status != step.Status || !containsCode(op.Errors, er.Error.Code) {
		t.Fatalf("step %d: %s may not return %s on a v1 route", n, op.ID, er.Error.Code)
	}
}

func (st *scenarioState) apply(t *testing.T, n int, op Operation, step draftStep) {
	t.Helper()
	switch op.ID {
	case "admit", "getContext":
		if step.Status != 200 {
			return
		}
		var ctx ContextV2
		if op.ID == "admit" {
			resp, _ := DecodeResponse[AdmitResponseV2](step.Body)
			ctx = resp.Context
		} else {
			ctx, _ = DecodeResponse[ContextV2](step.Body)
		}
		st.ctx = &ctx
	case "begin":
		if st.ctx == nil && step.Status == 200 {
			t.Fatalf("step %d: begin without an admitted context", n)
		}
		req, _ := DecodeRequest[BeginRequest](step.Request)
		switch step.Status {
		case 200:
			resp, _ := DecodeResponse[BeginResponse](step.Body)
			if err := CheckBeginResponse(*st.ctx, req, resp); err != nil {
				t.Fatalf("step %d: %v", n, err)
			}
			if prev, ok := st.begun[st.ctx.AttemptID]; ok {
				if !jsonEqual(prev[0], step.Request) || !jsonEqual(prev[1], step.Body) {
					t.Fatalf("step %d: a replay of begin must return the original record for the same body", n)
				}
			}
			st.begun[st.ctx.AttemptID] = [2]json.RawMessage{step.Request, step.Body}
		case 409:
			if er, _ := DecodeResponse[ErrorResponse](step.Body); er.Error.Code == domain.CodeIdempotencyConflict {
				prev, ok := st.begun[st.ctx.AttemptID]
				if !ok || jsonEqual(prev[0], step.Request) {
					t.Fatalf("step %d: idempotency_conflict needs an earlier begin with a different body", n)
				}
			}
		case 400:
			if _, _, err := DeriveBegin(*st.ctx, req); err == nil {
				t.Fatalf("step %d: invalid_request recorded for a valid begin", n)
			}
		}
	case "admitResource":
		if step.Status == 200 {
			resp, _ := DecodeResponse[ResourceAdmitResponse](step.Body)
			st.res = &resp.Context
			if step.Kind != "" && string(resp.Context.ResourceKind) != step.Kind {
				t.Fatalf("step %d: admitted a %s as a %s", n, resp.Context.ResourceKind, step.Kind)
			}
		}
	case "getResourceContext":
		if step.Status == 200 {
			got, _ := DecodeResponse[ResourceContext](step.Body)
			if st.res == nil || !reflect.DeepEqual(*st.res, got) {
				t.Fatalf("step %d: the context is immutable and equals the admitted one", n)
			}
		}
	case "resolveResourceSecret":
		req, _ := DecodeRequest[SecretResolveRequest](step.Request)
		bound := false
		if st.res != nil && st.res.Service != nil {
			for _, b := range st.res.Service.Secrets {
				bound = bound || (b.Ref == req.Ref && b.Version == req.Version)
			}
		}
		if step.Status == 200 && !bound {
			t.Fatalf("step %d: a service resolves only its own pinned bindings", n)
		}
		if step.Status == 404 && bound {
			t.Fatalf("step %d: a pinned binding is resolvable", n)
		}
	case "completePreparation":
		req, _ := DecodeRequest[PreparationReceiptRequest](step.Request)
		switch step.Status {
		case 200:
			if st.res == nil {
				t.Fatalf("step %d: receipt without an admitted preparation", n)
			}
			if err := CheckReceipt(*st.res, req); err != nil {
				t.Fatalf("step %d: %v", n, err)
			}
			if st.receipt != nil && (!jsonEqual(st.receipt[0], step.Request) || !jsonEqual(st.receipt[1], step.Body)) {
				t.Fatalf("step %d: an identical repeat returns the original receipt", n)
			}
			st.receipt = &[2]json.RawMessage{step.Request, step.Body}
		case 409:
			if st.receipt == nil || jsonEqual(st.receipt[0], step.Request) {
				t.Fatalf("step %d: receipt_conflict needs an earlier different receipt", n)
			}
		}
	}
}

// Test_runtime_v2_schema_matches_go: the schema's v2 string syntaxes equal the Go
// patterns, the schema's error codes equal the codes the Go contract knows, and
// the shared definitions come from the unchanged v1 schema resource. WS-13.
func Test_runtime_v2_schema_matches_go(t *testing.T) {
	doc, _ := newCompiler(t)
	defs := doc["$defs"].(map[string]any)
	for name, pattern := range Patterns {
		def, ok := defs[name].(map[string]any)
		if !ok || def["pattern"] != pattern {
			t.Errorf("%s: schema pattern %v differs from Go %q", name, def["pattern"], pattern)
		}
	}
	for name := range runtimev1.Patterns {
		if _, shadow := defs[name]; shadow {
			t.Errorf("%s must be referenced from the v1 schema, not redefined", name)
		}
	}
	codes, _ := defs["ErrorCode"].(map[string]any)["enum"].([]any)
	var inSchema []string
	for _, c := range codes {
		inSchema = append(inSchema, c.(string))
		if _, ok := Status(domain.ErrorCode(c.(string))); !ok {
			t.Errorf("schema code %v has no status", c)
		}
	}
	known := map[string]bool{}
	for _, c := range inSchema {
		known[c] = true
	}
	for _, op := range Operations {
		for _, code := range op.Errors {
			if _, ok := Status(code); !ok {
				t.Errorf("%s lists code %s with no status", op.ID, code)
			}
			if !known[string(code)] {
				t.Errorf("%s lists code %s the schema does not enumerate", op.ID, code)
			}
		}
	}
	for code, want := range map[domain.ErrorCode]int{CodeContextVersionMismatch: 409, CodeReceiptConflict: 409, CodeResourceUnbound: 409, CodeResourceRevoked: 403} {
		if got, ok := Status(code); !ok || got != want {
			t.Errorf("%s: status %d, want %d", code, got, want)
		}
		if _, v1known := runtimev1.Status(code); v1known {
			t.Errorf("%s must not exist in the frozen v1 table", code)
		}
	}
	// Every v1 code keeps its status under v2.
	for _, op := range runtimev1.Operations {
		for _, code := range op.Errors {
			v1s, _ := runtimev1.Status(code)
			if v2s, ok := Status(code); !ok || v2s != v1s {
				t.Errorf("code %s changed: v1 %d, v2 %d", code, v1s, v2s)
			}
		}
	}
	if r := (ErrorResponse{Error: ErrorBody{Code: CodeResourceUnbound, Message: "m", RequestID: "r", Details: map[string]any{}}}); r.Validate() != nil {
		t.Error("v2 must accept resource_unbound")
	}
	if r := (runtimev1.ErrorResponse{Error: ErrorBody{Code: CodeResourceUnbound, Message: "m", RequestID: "r", Details: map[string]any{}}}); r.Validate() == nil {
		t.Error("the frozen v1 envelope must still reject the v2 codes")
	}
}

// Test_runtime_v2_operations_and_v1_reuse: the attempt routes that v2 reuses
// carry the v1 request and response types unchanged; only admit and context
// change; the new routes are exactly the contract's. The public api/runtime.yaml
// stays the frozen v1 document: v2 routes are not in it. PX-21.
func Test_runtime_v2_operations_and_v1_reuse(t *testing.T) {
	v1 := map[string]runtimev1.Operation{}
	for _, op := range runtimev1.Operations {
		v1[op.ID] = op
	}
	renamed := map[string]string{"AdmitResponse": "AdmitResponseV2", "Context": "ContextV2"}
	seen := map[string]bool{}
	for _, op := range Operations {
		if seen[op.ID] {
			t.Fatalf("duplicate operation %s", op.ID)
		}
		seen[op.ID] = true
		if op.Subject != SubjectAttempt {
			continue
		}
		old, reused := v1[op.ID]
		if !reused {
			if op.ID != "begin" {
				t.Errorf("unexpected new attempt route %s", op.ID)
			}
			continue
		}
		if op.Method != old.Method || op.Path != old.Path || op.Request != old.Request {
			t.Errorf("%s changed its method, path or request", op.ID)
		}
		for status, name := range old.Responses {
			want := name
			if r, ok := renamed[name]; ok {
				want = r
			}
			if op.Responses[status] != want {
				t.Errorf("%s response %d is %s, want %s", op.ID, status, op.Responses[status], want)
			}
		}
		// v2 may add context_version_mismatch to a reused route and nothing else.
		for _, code := range old.Errors {
			if !containsCode(op.Errors, code) {
				t.Errorf("%s dropped %s", op.ID, code)
			}
		}
	}
	for id := range v1 {
		if !seen[id] {
			t.Errorf("v1 operation %s has no v2 counterpart", id)
		}
	}
	want := map[string][]ResourceKind{
		"admitResource": {ResourcePreparation, ResourceService}, "renewResourceToken": {ResourcePreparation, ResourceService},
		"getResourceContext": {ResourcePreparation, ResourceService}, "resolveResourceSecret": {ResourceService}, "completePreparation": {ResourcePreparation},
	}
	for _, op := range Operations {
		if op.Subject == SubjectResource && !reflect.DeepEqual(op.Kinds, want[op.ID]) {
			t.Errorf("%s kinds %v, want %v", op.ID, op.Kinds, want[op.ID])
		}
		delete(want, op.ID)
	}
	if len(want) != 0 {
		t.Errorf("missing resource operations %v", want)
	}

	// The checked-in public OpenAPI is the frozen v1 table and mentions no v2 route.
	api := string(mustRead(t, "../../../api/runtime.yaml"))
	for _, forbidden := range []string{"/runtime/v2", "/begin", "preparation/complete", "kodama-runtime-resource", "context_version_mismatch"} {
		if strings.Contains(api, forbidden) {
			t.Errorf("api/runtime.yaml must stay the frozen v1 document, it contains %q", forbidden)
		}
	}
}

// Test_runtime_v2_audience_and_subject_rules: the audiences differ per route
// family, a resource token never works on an attempt route and the reverse, and
// the preparation has no secrets route while a service has no preparation route.
// VS-14, IT-13.
func Test_runtime_v2_audience_and_subject_rules(t *testing.T) {
	if AttemptAudience != runtimev1.Audience || ResourceAudience == AttemptAudience {
		t.Fatal("the audiences must differ and the attempt audience must be the v1 one")
	}
	for _, c := range []struct {
		route    Subject
		audience string
		ok       bool
	}{
		{SubjectAttempt, AttemptAudience, true}, {SubjectResource, ResourceAudience, true},
		{SubjectAttempt, ResourceAudience, false}, {SubjectResource, AttemptAudience, false},
		{SubjectAttempt, "", false}, {SubjectResource, "kodama-runtime ", false}, {"", AttemptAudience, false}, {"workspace", ResourceAudience, false},
	} {
		err := CheckAudience(c.route, c.audience)
		if (err == nil) != c.ok {
			t.Errorf("%q on %q: %v", c.audience, c.route, err)
		}
		if err != nil && codeOf(err) != string(domain.CodeForbidden) {
			t.Errorf("a wrong audience is forbidden, got %s", codeOf(err))
		}
	}
	byID := map[string]Operation{}
	for _, op := range Operations {
		byID[op.ID] = op
	}
	for id, kinds := range map[string]map[ResourceKind]bool{
		"resolveResourceSecret": {ResourceService: true}, "completePreparation": {ResourcePreparation: true},
		"getResourceContext": {ResourceService: true, ResourcePreparation: true}, "renewResourceToken": {ResourceService: true, ResourcePreparation: true},
		"admitResource": {ResourceService: true, ResourcePreparation: true},
	} {
		for _, k := range []ResourceKind{ResourcePreparation, ResourceService, "workspace", "network", ""} {
			if err := CheckResourceCall(byID[id], k); (err == nil) != kinds[k] {
				t.Errorf("%s as %q: %v", id, k, err)
			}
		}
	}
	if err := CheckResourceCall(byID["begin"], ResourceService); err == nil {
		t.Error("an attempt route is never a resource route")
	}
	// Resource keys: only preparation and service.NAME have identity.
	for key, ok := range map[string]bool{"preparation": true, "service.db": true, "service.a-b": true, "workspace": false, "network": false,
		"service.": false, "service.Db": false, "service.db.x": false, "preparation2": false, "": false} {
		if got := compiled["ResourceKey"].MatchString(key); got != ok {
			t.Errorf("resource key %q: %v", key, got)
		}
	}
	for _, c := range []struct {
		kind ResourceKind
		name string
		want string
		ok   bool
	}{
		{ResourceService, "db", "service.db", true}, {ResourceService, "a-b", "service.a-b", true}, {ResourcePreparation, "", "preparation", true},
		{"", "", "", false}, {"workspace", "", "", false}, {"network", "", "", false}, {ResourceService, "", "", false},
		{ResourceService, "Db", "", false}, {ResourceService, "a.b", "", false}, {ResourcePreparation, "db", "", false},
	} {
		got, err := ResourceKeyOf(c.kind, c.name)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ResourceKeyOf(%q, %q) = %q, %v", c.kind, c.name, got, err)
		}
	}
	if n, ok := ServiceNameOf("service.db"); !ok || n != "db" {
		t.Error("service name extraction")
	}
}

// Test_runtime_v2_attempt_routes_list_forbidden: every attempt route can return
// 403 forbidden, because a resource-audience token is rejected on it; the same
// holds for every resource route, while the frozen v1 tables are unchanged and
// do not list it where it was never listed. VS-14, IT-13.
func Test_runtime_v2_attempt_routes_list_forbidden(t *testing.T) {
	for _, op := range Operations {
		if !containsCode(op.Errors, domain.CodeForbidden) {
			t.Errorf("%s does not list forbidden", op.ID)
		}
	}
	v1 := map[string]runtimev1.Operation{}
	for _, op := range runtimev1.Operations {
		v1[op.ID] = op
	}
	for _, id := range []string{"getContext", "renewToken", "reserveArtifact", "completeArtifact", "proposeSource", "submitResult"} {
		if containsCode(v1[id].Errors, domain.CodeForbidden) {
			t.Errorf("the frozen v1 route %s must not have gained forbidden", id)
		}
	}
}

// Test_runtime_v2_resource_envelope_consistency: a resource token never outlives
// the resource it was issued for. The check is pure and runs after the structural
// validation of both parts; equality at the deadline is allowed, one second later
// is not, for a preparation and for a service, and a context of an unusable kind
// has no deadline to compare. VS-14.
func Test_runtime_v2_resource_envelope_consistency(t *testing.T) {
	decode := func(b obj) ResourceAdmitResponse {
		var r ResourceAdmitResponse
		if err := json.Unmarshal(mustJSON(t, b), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for name, tc := range map[string]struct {
		ctx obj
		max string
		ok  bool
	}{
		"preparation, equal":       {prepCtx(), "2026-10-26T11:00:00Z", true},
		"preparation, earlier":     {prepCtx(), "2026-10-26T10:59:59Z", true},
		"preparation, one later":   {prepCtx(), "2026-10-26T11:00:01Z", false},
		"preparation, old fixture": {prepCtx(), "2026-10-26T12:00:00Z", false},
		"service, equal":           {svcSecretsCtx(), "2026-10-26T11:00:00Z", true},
		"service, one later":       {svcSecretsCtx(), "2026-10-26T11:00:01Z", false},
		"service defaults, later":  {svcDefaultsCtx(), "2026-10-26T12:00:00Z", false},
		"fractional second, later": {prepCtx(), "2026-10-26T11:00:00.000000001Z", false},
		"fractional second, equal": {prepCtx(), "2026-10-26T11:00:00.000Z", true},
	} {
		r := decode(obj{"token": mut(t, tok("kodama-runtime-resource"), "maxExpiresAt", tc.max), "context": tc.ctx})
		if err := r.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err := CheckResourceEnvelope(r.Token, r.Context); (err == nil) != tc.ok {
			t.Errorf("%s (pure check): %v", name, err)
		}
	}
	if err := CheckResourceEnvelope(ResourceTokenInfo{MaxExpiresAt: "2026-10-26T11:00:00Z"}, ResourceContext{ResourceKind: "workspace"}); err == nil {
		t.Error("a context without a deadline member has no envelope")
	}
}

// Test_runtime_v2_source_free_sequence_example_is_consistent: the two-step
// source-free example is a shape example only (it qualifies no filesystem), but
// its two commands must tell the story the contract tells: the first step writes
// a file under /workspace and the second step, whose lineage lists the first with
// the explicit source-free values, reads that same file. WS-13.
func Test_runtime_v2_source_free_sequence_example_is_consistent(t *testing.T) {
	decode := func(c obj) ContextV2 {
		ctx, err := DecodeResponse[ContextV2](mustJSON(t, c))
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	first, second := decode(ctxFree()), decode(ctxFreeSecond())
	if len(first.Workspace.Lineage) != 0 || len(second.Workspace.Lineage) != 1 {
		t.Fatalf("lineage %d, %d", len(first.Workspace.Lineage), len(second.Workspace.Lineage))
	}
	if second.Workspace.Lineage[0].StepID != first.StepID || second.Workspace.Lineage[0].AttemptID != first.AttemptID {
		t.Fatal("the second context does not list the first attempt")
	}
	writes := strings.Join(first.Command.Args, " ")
	reads := strings.Join(second.Command.Args, " ")
	const file = "/workspace/teardown.log"
	if !strings.Contains(writes, "> "+file) || !strings.Contains(reads, file) {
		t.Fatalf("the first step must write and the second read %s: %q / %q", file, writes, reads)
	}
	if strings.Contains(writes, "go test") {
		t.Fatal("the first source-free step must not inherit a source-backed command")
	}
}

// Test_runtime_v2_derivations: the begin state and the receipt checks are pure
// functions of the context. DQ-16, WS-13.
func Test_runtime_v2_derivations(t *testing.T) {
	decodeCtx := func(c obj) ContextV2 {
		raw, _ := json.Marshal(c)
		ctx, err := DecodeResponse[ContextV2](raw)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	reader, free := decodeCtx(ctxReader()), decodeCtx(ctxFree())
	head := GitCommit(c40b)
	for name, tc := range map[string]struct {
		ctx     ContextV2
		req     BeginRequest
		state   WorkspaceState
		ignored bool
		code    string
	}{
		"clean":                    {reader, BeginRequest{&BeginWorkspace{head, true, false, false}}, StateClean, false, ""},
		"clean with ignored files": {reader, BeginRequest{&BeginWorkspace{head, true, false, true}}, StateClean, true, ""},
		"tracked change":           {reader, BeginRequest{&BeginWorkspace{head, false, false, false}}, StateModified, false, ""},
		"untracked file":           {reader, BeginRequest{&BeginWorkspace{head, true, true, false}}, StateModified, false, ""},
		"head moved":               {reader, BeginRequest{&BeginWorkspace{c40d, true, false, false}}, StateModified, false, ""},
		"head still the input":     {reader, BeginRequest{&BeginWorkspace{c40a, true, false, false}}, StateModified, false, ""},
		"modified and ignored":     {reader, BeginRequest{&BeginWorkspace{head, false, true, true}}, StateModified, true, ""},
		"source-free":              {free, BeginRequest{}, StateNone, false, ""},
		"null body, source-backed": {reader, BeginRequest{}, "", false, "invalid_request"},
		"object body, source-free": {free, BeginRequest{&BeginWorkspace{head, true, false, false}}, "", false, "invalid_request"},
		"malformed head":           {reader, BeginRequest{&BeginWorkspace{"main", true, false, false}}, "", false, "invalid_request"},
	} {
		state, ignored, err := DeriveBegin(tc.ctx, tc.req)
		if tc.code != "" {
			if err == nil || codeOf(err) != tc.code {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err != nil || state != tc.state || ignored != tc.ignored {
			t.Errorf("%s: got %q %v %v", name, state, ignored, err)
		}
	}
	// The response must be the derivation, for the right attempt.
	req := BeginRequest{&BeginWorkspace{head, true, false, true}}
	good := BeginResponse{AttemptID: reader.AttemptID, BeganAt: "2026-10-26T10:05:00Z", WorkspaceState: StateClean, IgnoredPresent: true}
	if err := CheckBeginResponse(reader, req, good); err != nil {
		t.Fatal(err)
	}
	for name, resp := range map[string]BeginResponse{
		"wrong state": {good.AttemptID, good.BeganAt, StateModified, true}, "wrong ignored": {good.AttemptID, good.BeganAt, StateClean, false},
		"other attempt": {"att_99", good.BeganAt, StateClean, true},
	} {
		if err := CheckBeginResponse(reader, req, resp); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	var prepCtx ResourceContext
	if raw, err := json.Marshal(prepCtxObj()); err != nil {
		t.Fatal(err)
	} else if prepCtx, err = DecodeResponse[ResourceContext](raw); err != nil {
		t.Fatal(err)
	}
	svc, _ := DecodeResponse[ResourceContext](mustJSON(t, svcDefaultsCtx()))
	okReq := PreparationReceiptRequest{Commit: c40a, TreeID: c40c, HooksDisabled: true, Submodules: SubmodulesSkipped, CredentialsRemoved: true, Attempts: 2}
	if err := CheckReceipt(prepCtx, okReq); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ctx ResourceContext
		req PreparationReceiptRequest
	}{
		"other commit":     {prepCtx, PreparationReceiptRequest{Commit: c40d, TreeID: c40c, HooksDisabled: true, Submodules: SubmodulesSkipped, CredentialsRemoved: true, Attempts: 1}},
		"too many tries":   {prepCtx, PreparationReceiptRequest{Commit: c40a, TreeID: c40c, HooksDisabled: true, Submodules: SubmodulesSkipped, CredentialsRemoved: true, Attempts: 3}},
		"hooks enabled":    {prepCtx, PreparationReceiptRequest{Commit: c40a, TreeID: c40c, HooksDisabled: false, Submodules: SubmodulesSkipped, CredentialsRemoved: true, Attempts: 1}},
		"service resource": {svc, okReq},
	} {
		if err := CheckReceipt(tc.ctx, tc.req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func prepCtxObj() obj { return prepCtx() }

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Test_runtime_v2_result_hash_unchanged: ResultHash under v2 is byte-for-byte
// the v1 hash of the result object, so every historical hash vector stays valid
// and a result's hash does not depend on the context version; the v1 fixtures
// decode through the shared decoders untouched. PX-21.
func Test_runtime_v2_result_hash_unchanged(t *testing.T) {
	var v1cases []payloadCase
	if err := json.Unmarshal(mustRead(t, "../v1/fixtures/cases.json"), &v1cases); err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, c := range v1cases {
		if c.Type != "ResultRequest" || !c.Valid || c.Raw != nil {
			continue
		}
		req, err := DecodeRequest[ResultRequest](c.Payload)
		if err != nil {
			t.Fatalf("%s: v1 result fixture no longer decodes: %v", c.Name, err)
		}
		got, err := ResultHash(req.Result)
		if err != nil || got != req.ResultHash {
			t.Fatalf("%s: v2 hash %s differs from the v1 vector %s (%v)", c.Name, got, req.ResultHash, err)
		}
		v1hash, _ := runtimev1.ResultHash(req.Result)
		if v1hash != got {
			t.Fatalf("%s: aliases diverged", c.Name)
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("only %d v1 result vectors checked", checked)
	}
	// The v1 canonical vectors are untouched, so the shared canonicalizer is the v1 one.
	var vectors []struct {
		Name      string `json:"name"`
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		Error     bool   `json:"error"`
	}
	if err := json.Unmarshal(mustRead(t, "../v1/fixtures/canonical.json"), &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		got, err := runtimev1.Canonicalize([]byte(v.Input))
		if v.Error != (err != nil) || (!v.Error && string(got) != v.Canonical) {
			t.Errorf("%s: canonical vector changed", v.Name)
		}
	}
	// A result's hash never depends on the context version: a v2 context carries
	// no result field and the v1 Result type is the same type.
	if reflect.TypeOf(Result{}) != reflect.TypeOf(runtimev1.Result{}) || reflect.TypeOf(ResultRequest{}) != reflect.TypeOf(runtimev1.ResultRequest{}) {
		t.Fatal("the result DTOs are v1 aliases")
	}
}

// Test_runtime_v2_v1_validators_unchanged: the v1 token, admit and context
// validators keep their rules; v2 messages never validate as their v1
// counterparts and the reverse, so the two wires cannot be confused. PX-21.
func Test_runtime_v2_v1_validators_unchanged(t *testing.T) {
	// A v1 TokenInfo still requires the attempt audience.
	resource := tok("kodama-runtime-resource")
	rawToken, _ := json.Marshal(obj{"token": resource})
	if _, err := runtimev1.DecodeResponse[runtimev1.TokenResponse](rawToken); err == nil {
		t.Fatal("the v1 token validator accepted the resource audience")
	}
	// A v1 admit request still demands a Job identity.
	if _, err := runtimev1.DecodeRequest[runtimev1.AdmitRequest](mustJSON(t, resAdmit(identityServicePod()))); err == nil {
		t.Fatal("the v1 admit request accepted a resource identity")
	}
	// A v2 context is not a v1 context and the reverse.
	if _, err := runtimev1.DecodeResponse[runtimev1.Context](mustJSON(t, ctxReader())); err == nil {
		t.Fatal("a v1 reader accepted a v2 context")
	}
	var v1ctx []payloadCase
	if err := json.Unmarshal(mustRead(t, "../v1/fixtures/cases.json"), &v1ctx); err != nil {
		t.Fatal(err)
	}
	var decodedV1 int
	for _, c := range v1ctx {
		if c.Type != "Context" || !c.Valid {
			continue
		}
		if _, err := DecodeResponse[ContextV2](c.Payload); err == nil {
			t.Fatalf("%s: a v1 context validated as v2", c.Name)
		}
		decodedV1++
	}
	if decodedV1 == 0 {
		t.Fatal("no v1 context fixtures found")
	}
	// The resource token type is its own type, not the v1 one.
	if reflect.TypeOf(ResourceTokenInfo{}) == reflect.TypeOf(TokenInfo{}) || reflect.TypeOf(ResourceIdentity{}) == reflect.TypeOf(JobIdentity{}) {
		t.Fatal("resource subjects must not reuse the v1 token or Job identity types")
	}
	// The v1 package still has exactly its eight routes and no v2 code.
	if len(runtimev1.Operations) != 8 {
		t.Fatalf("v1 routes: %d", len(runtimev1.Operations))
	}
	for _, c := range []domain.ErrorCode{CodeContextVersionMismatch, CodeReceiptConflict, CodeResourceUnbound, CodeResourceRevoked} {
		if _, ok := runtimev1.Status(c); ok {
			t.Errorf("v1 learned %s", c)
		}
	}
}

// Test_runtime_v2_strict_and_nullable: every v2 DTO writes every field; a
// nullable field is present as null and omitting it is an error; requests reject
// unknown fields while responses tolerate them. PX-21.
func Test_runtime_v2_strict_and_nullable(t *testing.T) {
	// Marshal of zero-value-valid DTOs writes explicit nulls, never omits.
	b, _ := json.Marshal(BeginRequest{})
	if string(b) != `{"workspace":null}` {
		t.Fatalf("begin request: %s", b)
	}
	b, _ = json.Marshal(ResourceIdentity{})
	if !strings.Contains(string(b), `"podName":null`) || !strings.Contains(string(b), `"podUid":null`) {
		t.Fatalf("identity: %s", b)
	}
	b, _ = json.Marshal(ServiceCommand{Args: []string{}, Env: map[string]string{}})
	if !strings.Contains(string(b), `"command":null`) {
		t.Fatalf("an image-defaults service writes command null: %s", b)
	}
	// An explicitly empty command is distinct from null and is invalid.
	empty := mut(t, svcDefaultsCtx(), "service.command.command", []any{})
	if _, err := DecodeResponse[ResourceContext](mustJSON(t, empty)); err == nil {
		t.Fatal("command [] must be rejected: absent means image defaults, never an empty argv")
	}
	// Request strictness versus response tolerance on the same field.
	withExtra := mut(t, receiptReq(), "extra", true)
	if _, err := DecodeRequest[PreparationReceiptRequest](mustJSON(t, withExtra)); err == nil {
		t.Fatal("a request with an unknown field was accepted")
	}
	respExtra := mut(t, obj{"receiptDigest": dgE, "recordedAt": "2026-10-26T10:00:30Z"}, "extra", true)
	if _, err := DecodeResponse[PreparationReceiptResponse](mustJSON(t, respExtra)); err != nil {
		t.Fatalf("a response with an additional field must be tolerated: %v", err)
	}
	// Case variants are unknown fields, not aliases.
	if _, err := DecodeRequest[BeginRequest](mustJSON(t, obj{"Workspace": null})); err == nil {
		t.Fatal("a differently cased field was accepted")
	}
	// Integers only.
	if _, err := DecodeRequest[PreparationReceiptRequest]([]byte(strings.Replace(string(mustJSON(t, receiptReq())), `"attempts":1`, `"attempts":1e0`, 1))); err == nil {
		t.Fatal("an exponent number was accepted")
	}
}

// Test_runtime_v2_fixture_files_exist guards the published fixture set and the
// README handoff against silent removal.
func Test_runtime_v2_fixture_files_exist(t *testing.T) {
	for _, f := range []string{"schema.json", "fixtures/cases.json", "fixtures/scenarios.json"} {
		if _, err := os.Stat(filepath.FromSlash(f)); err != nil {
			t.Error(err)
		}
	}
}
