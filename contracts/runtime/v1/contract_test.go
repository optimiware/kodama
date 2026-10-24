package runtimev1

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/optimiware/kodama/internal/domain"
)

const schemaURL = "https://kodama.invalid/contracts/runtime/v1/schema.json"

type fixtureCase struct {
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Requirements []string        `json:"requirements"`
	Payload      json.RawMessage `json:"payload"`
	Raw          *string         `json:"raw"`
	SchemaValid  *bool           `json:"schemaValid"`
	Valid        bool            `json:"valid"`
	Code         string          `json:"code"`
}

func (c fixtureCase) bytes() []byte {
	if c.Raw != nil {
		return []byte(*c.Raw)
	}
	return c.Payload
}

// decoders maps every message type to its strict (request) or tolerant
// (response) decoder, so each fixture runs through the same code consumers use.
var decoders = map[string]func([]byte) error{
	"AdmitRequest":            func(b []byte) error { _, err := DecodeRequest[AdmitRequest](b); return err },
	"TokenRequest":            func(b []byte) error { _, err := DecodeRequest[TokenRequest](b); return err },
	"SecretResolveRequest":    func(b []byte) error { _, err := DecodeRequest[SecretResolveRequest](b); return err },
	"ArtifactReserveRequest":  func(b []byte) error { _, err := DecodeRequest[ArtifactReserveRequest](b); return err },
	"ArtifactCompleteRequest": func(b []byte) error { _, err := DecodeRequest[ArtifactCompleteRequest](b); return err },
	"SourceProposalRequest":   func(b []byte) error { _, err := DecodeRequest[SourceProposalRequest](b); return err },
	"ResultRequest":           func(b []byte) error { _, err := DecodeRequest[ResultRequest](b); return err },
	"Result":                  func(b []byte) error { _, err := DecodeRequest[Result](b); return err },
	"AdmitResponse":           func(b []byte) error { _, err := DecodeResponse[AdmitResponse](b); return err },
	"TokenResponse":           func(b []byte) error { _, err := DecodeResponse[TokenResponse](b); return err },
	"Context":                 func(b []byte) error { _, err := DecodeResponse[Context](b); return err },
	"SecretResolveResponse":   func(b []byte) error { _, err := DecodeResponse[SecretResolveResponse](b); return err },
	"ArtifactReserveResponse": func(b []byte) error { _, err := DecodeResponse[ArtifactReserveResponse](b); return err },
	"ArtifactCompleteResponse": func(b []byte) error {
		_, err := DecodeResponse[ArtifactCompleteResponse](b)
		return err
	},
	"SourceProposalResponse": func(b []byte) error { _, err := DecodeResponse[SourceProposalResponse](b); return err },
	"ResultResponse":         func(b []byte) error { _, err := DecodeResponse[ResultResponse](b); return err },
	"ErrorResponse":          func(b []byte) error { _, err := DecodeResponse[ErrorResponse](b); return err },
}

func loadSchemaDoc(t *testing.T) (map[string]any, *jsonschema.Compiler) {
	t.Helper()
	data, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaURL, instance); err != nil {
		t.Fatal(err)
	}
	return instance.(map[string]any), compiler
}

func schemaVerdict(t *testing.T, compiler *jsonschema.Compiler, typeName string, payload []byte) bool {
	t.Helper()
	schema, err := compiler.Compile(schemaURL + "#/$defs/" + typeName)
	if err != nil {
		t.Fatalf("compile %s: %v", typeName, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	return schema.Validate(instance) == nil
}

func codeOf(err error) string {
	if ce, ok := err.(*ContractError); ok {
		return string(ce.Code)
	}
	return "?"
}

// TestRuntimeContractFixtures is the T-003 acceptance check
// (runtime_contract_fixtures): positive and negative payloads agree between the
// JSON Schema and the Go contract, and the semantic rules hold.
func TestRuntimeContractFixtures(t *testing.T) {
	_, compiler := loadSchemaDoc(t)
	var cases []fixtureCase
	readFixture(t, "cases.json", &cases)

	t.Run("payloads", func(t *testing.T) {
		valid, invalid := map[string]bool{}, map[string]bool{}
		for _, c := range cases {
			t.Run(c.Name, func(t *testing.T) {
				if len(c.Requirements) == 0 {
					t.Fatal("fixture cites no requirement")
				}
				decode := decoders[c.Type]
				if decode == nil {
					t.Fatalf("no decoder for %s", c.Type)
				}
				err := decode(c.bytes())
				if (err == nil) != c.Valid {
					t.Fatalf("Go contract: valid=%v, fixture expects %v (%v)", err == nil, c.Valid, err)
				}
				if err != nil && codeOf(err) != c.Code {
					t.Fatalf("code %q, want %q (%v)", codeOf(err), c.Code, err)
				}
				if c.Raw == nil {
					got := schemaVerdict(t, compiler, c.Type, c.bytes())
					if c.SchemaValid == nil || got != *c.SchemaValid {
						t.Fatalf("schema valid=%v, fixture expects %v", got, c.SchemaValid)
					}
					if c.Valid && !got {
						t.Fatal("Go accepts a payload the schema rejects")
					}
				}
			})
			if c.Valid {
				valid[c.Type] = true
			} else {
				invalid[c.Type] = true
			}
		}
		for _, op := range Operations {
			for _, typeName := range append([]string{op.Request}, values(op.Responses)...) {
				if typeName != "" && !valid[typeName] {
					t.Errorf("no positive fixture for %s", typeName)
				}
			}
			if op.Request != "" && !invalid[op.Request] {
				t.Errorf("no negative fixture for %s", op.Request)
			}
		}
	})

	t.Run("semantic_checks", func(t *testing.T) { runChecks(t) })
	t.Run("scenarios", func(t *testing.T) { runScenarios(t) })
	t.Run("traceability", func(t *testing.T) {
		seen := map[string]bool{}
		var checks []checkCase
		var scenarios []scenario
		readFixture(t, "checks.json", &checks)
		readFixture(t, "scenarios.json", &scenarios)
		for _, c := range cases {
			mark(seen, c.Requirements)
		}
		for _, c := range checks {
			mark(seen, c.Requirements)
		}
		for _, s := range scenarios {
			mark(seen, s.Requirements)
		}
		for _, id := range []string{"PX-10", "AR-06", "AC-12", "AR-13", "IT-06", "IT-11"} {
			if !seen[id] {
				t.Errorf("no fixture covers %s", id)
			}
		}
	})
}

func values(m map[int]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func mark(seen map[string]bool, ids []string) {
	for _, id := range ids {
		seen[id] = true
	}
}

type checkCase struct {
	Name      string          `json:"name"`
	Context   json.RawMessage `json:"context"`
	Result    json.RawMessage `json:"result"`
	Artifacts []struct {
		ID        domain.ArtifactID `json:"id"`
		TenantID  domain.TenantID   `json:"tenantId"`
		ProjectID domain.ProjectID  `json:"projectId"`
		AttemptID domain.AttemptID  `json:"attemptId"`
		Kind      ArtifactKind      `json:"kind"`
		Usable    bool              `json:"usable"`
	} `json:"artifacts"`
	Code         string   `json:"code"`
	Requirements []string `json:"requirements"`
}

func runChecks(t *testing.T) {
	var checks []checkCase
	readFixture(t, "checks.json", &checks)
	for _, c := range checks {
		t.Run(c.Name, func(t *testing.T) {
			ctx, err := DecodeResponse[Context](c.Context)
			if err != nil {
				t.Fatal(err)
			}
			result, err := DecodeRequest[Result](c.Result)
			if err != nil {
				t.Fatal(err)
			}
			refs := map[domain.ArtifactID]ArtifactRef{}
			for _, a := range c.Artifacts {
				refs[a.ID] = ArtifactRef{ID: a.ID, Scope: domain.Scope{TenantID: a.TenantID, ProjectID: a.ProjectID}, Attempt: a.AttemptID, Kind: a.Kind, Usable: a.Usable}
			}
			err = CheckResult(result, ctx, refs)
			if c.Code == "" && err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if c.Code != "" && codeOf(err) != c.Code {
				t.Fatalf("code %q, want %q (%v)", codeOf(err), c.Code, err)
			}
			if err != nil && strings.Contains(err.Error(), "t_other") {
				t.Fatalf("error discloses foreign data: %v", err)
			}
		})
	}
}

type step struct {
	Op         string          `json:"op"`
	Request    json.RawMessage `json:"request"`
	Status     int             `json:"status"`
	Body       json.RawMessage `json:"body"`
	ArtifactID string          `json:"artifactId"`
}

type scenario struct {
	Name         string   `json:"name"`
	Requirements []string `json:"requirements"`
	Steps        []step   `json:"steps"`
}

// runScenarios replays each recorded exchange against the operation table and
// applies the two stateful rules the server owns, so the fixtures are
// consistent protocol examples rather than loose JSON.
func runScenarios(t *testing.T) {
	var scenarios []scenario
	readFixture(t, "scenarios.json", &scenarios)
	ops := map[string]Operation{}
	for _, op := range Operations {
		ops[op.ID] = op
	}
	covered := map[string]bool{}
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			acceptedResult, admissions := "", map[string]string{}
			for n, st := range s.Steps {
				op, ok := ops[st.Op]
				if !ok {
					t.Fatalf("step %d: unknown operation %s", n, st.Op)
				}
				covered[op.ID] = true
				if op.Request != "" {
					if err := decoders[op.Request](st.Request); err != nil {
						t.Fatalf("step %d request: %v", n, err)
					}
				} else if string(st.Request) != "null" {
					t.Fatalf("step %d: %s takes no body", n, op.ID)
				}
				if typeName, success := op.Responses[st.Status]; success {
					if err := decoders[typeName](st.Body); err != nil {
						t.Fatalf("step %d response: %v", n, err)
					}
				} else {
					er, err := DecodeResponse[ErrorResponse](st.Body)
					if err != nil {
						t.Fatalf("step %d error body: %v", n, err)
					}
					if status, _ := Status(er.Error.Code); status != st.Status {
						t.Fatalf("step %d: code %s maps to %d, recorded %d", n, er.Error.Code, status, st.Status)
					}
					if !containsCode(op.Errors, er.Error.Code) {
						t.Fatalf("step %d: %s may not return %s", n, op.ID, er.Error.Code)
					}
				}
				switch st.Op {
				case "submitResult":
					req, _ := DecodeRequest[ResultRequest](st.Request)
					if st.Status == 200 {
						if acceptedResult != "" && acceptedResult != string(req.ResultHash) {
							t.Fatalf("step %d: a different result was acknowledged", n)
						}
						acceptedResult = string(req.ResultHash)
					} else if st.Status == 409 && (acceptedResult == "" || acceptedResult == string(req.ResultHash)) {
						t.Fatalf("step %d: result_conflict requires a different, earlier accepted hash", n)
					}
				case "admit":
					req, _ := DecodeRequest[AdmitRequest](st.Request)
					if st.Status == 200 {
						admissions[req.Nonce] = "ok"
					} else if st.Status == 409 && admissions[req.Nonce] != "" {
						t.Fatalf("step %d: a repeated nonce must be replayed, not rejected", n)
					}
				}
			}
		})
	}
	for id := range ops {
		if !covered[id] {
			t.Errorf("no scenario exercises %s", id)
		}
	}
}

func containsCode(list []domain.ErrorCode, code domain.ErrorCode) bool {
	for _, c := range list {
		if c == code {
			return true
		}
	}
	return false
}

// The Go validators and the schema must spell every string syntax identically.
func TestSchemaPatternsMatchGo(t *testing.T) {
	doc, _ := loadSchemaDoc(t)
	defs := doc["$defs"].(map[string]any)
	for name, pattern := range Patterns {
		def, ok := defs[name].(map[string]any)
		if !ok || def["pattern"] != pattern {
			t.Errorf("%s: schema pattern %v differs from Go %q", name, def["pattern"], pattern)
		}
	}
}

func TestErrorStatuses(t *testing.T) {
	for _, op := range Operations {
		for _, code := range op.Errors {
			if _, ok := Status(code); !ok {
				t.Errorf("%s lists code %s with no status", op.ID, code)
			}
		}
	}
	if reflect.DeepEqual(statuses[CodeAttemptRevoked], statuses[domain.CodeInvalidCredential]) {
		t.Error("revoked authority must be distinguishable from an invalid credential")
	}
}
