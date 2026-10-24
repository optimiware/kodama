package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

const commonYAML = "../../api/common.yaml"

type commonDoc struct {
	raw map[string]any
	sch *jsonschema.Schema // unused root placeholder
	c   *jsonschema.Compiler
}

func loadCommon(t *testing.T) *commonDoc {
	t.Helper()
	data, err := os.ReadFile(commonYAML)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	// OpenAPI 3.0 spells nullability as `nullable: true`; JSON Schema as a type
	// union. Translate so the document can be used as a validator.
	var fix func(any)
	fix = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if nb, _ := v["nullable"].(bool); nb {
				if ty, ok := v["type"].(string); ok {
					v["type"] = []any{ty, "null"}
				}
			}
			delete(v, "nullable")
			for _, c := range v {
				fix(c)
			}
		case []any:
			for _, c := range v {
				fix(c)
			}
		}
	}
	fix(raw)
	js, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("common.json", doc); err != nil {
		t.Fatal(err)
	}
	return &commonDoc{raw: raw, c: c}
}

func (d *commonDoc) schema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	s, err := d.c.Compile("common.json#/components/schemas/" + name)
	if err != nil {
		t.Fatalf("schema %s: %v", name, err)
	}
	return s
}

func (d *commonDoc) valid(t *testing.T, schema string, doc []byte) error {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return d.schema(t, schema).Validate(v)
}

func (d *commonDoc) validString(t *testing.T, schema, s string) bool {
	t.Helper()
	b, _ := json.Marshal(s)
	return d.valid(t, schema, b) == nil
}

func dig(t *testing.T, n any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := n.(map[string]any)
		if !ok {
			t.Fatalf("%v: not an object at %q", path, p)
		}
		if n, ok = m[p]; !ok {
			t.Fatalf("missing %v", path)
		}
	}
	return n
}

func Test_common_contract(t *testing.T) {
	d := loadCommon(t)

	t.Run("fragment declares no product routes and bearer security", func(t *testing.T) {
		if paths := dig(t, d.raw, "paths").(map[string]any); len(paths) != 0 {
			t.Fatalf("common.yaml declares routes: %v", paths)
		}
		if dig(t, d.raw, "components", "securitySchemes", "bearerAuth", "scheme") != "bearer" {
			t.Fatal("bearer security scheme missing")
		}
		if !strings.HasPrefix(dig(t, d.raw, "openapi").(string), "3.0.") {
			t.Fatal("generator-compatible OpenAPI 3.0 expected")
		}
	})

	t.Run("fixtures validate and mutations fail", func(t *testing.T) {
		for file, schema := range map[string]string{
			"error_envelope.json": "ErrorEnvelope", "error_envelope_details.json": "ErrorEnvelope",
			"page_next.json": "Page", "page_last.json": "Page",
		} {
			b, err := os.ReadFile("testdata/" + file)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.valid(t, schema, b); err != nil {
				t.Errorf("%s: %v", file, err)
			}
		}
		for name, doc := range map[string]string{
			"envelope without details":   `{"error":{"code":"x","message":"m","requestId":"req_0123456789abcdef0123456789abcdef"}}`,
			"envelope extra field":       `{"error":{"code":"x","message":"m","requestId":"req_0123456789abcdef0123456789abcdef","details":{},"stack":"s"}}`,
			"envelope bad request id":    `{"error":{"code":"x","message":"m","requestId":"abc","details":{}}}`,
			"envelope uppercase code":    `{"error":{"code":"Bad","message":"m","requestId":"req_0123456789abcdef0123456789abcdef","details":{}}}`,
			"envelope null details":      `{"error":{"code":"x","message":"m","requestId":"req_0123456789abcdef0123456789abcdef","details":null}}`,
			"error outside the envelope": `{"code":"x"}`,
		} {
			if err := d.valid(t, "ErrorEnvelope", []byte(doc)); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		for name, doc := range map[string]string{
			"page without cursor key": `{"items":[]}`,
			"page with null items":    `{"items":null,"nextCursor":null}`,
			"page with bad cursor":    `{"items":[],"nextCursor":"nodot"}`,
			"page too many items":     `{"items":[` + strings.TrimSuffix(strings.Repeat("{},", 201), ",") + `],"nextCursor":null}`,
		} {
			if err := d.valid(t, "Page", []byte(doc)); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})

	t.Run("generated envelope matches what the server writes", func(t *testing.T) {
		h := newHarness(t, nil)
		h.handle(httpapi.Route{Method: "GET", Path: "/e", Action: "x", Handler: func(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
			httpapi.WriteError(w, r, httpapi.NewError(409, "stale_gate_subject", "changed").WithDetails(map[string]any{"subject": "sha256:x"}))
		}})
		for _, p := range []string{"/api/v1/e", "/api/v1/missing"} {
			rec := h.do("GET", p, "", bearer("tok-alice")...)
			if err := d.valid(t, "ErrorEnvelope", rec.Body.Bytes()); err != nil {
				t.Errorf("%s: %v: %s", p, err, rec.Body)
			}
			env := rec.envelope(t) // strict decode into the generated type
			if env.Error.Details == nil || env.Error.RequestId != rec.Header().Get("X-Request-ID") {
				t.Errorf("%s: %+v", p, env)
			}
		}
		if err := d.valid(t, "ErrorEnvelope", mustJSON(t, httpapi.ErrorEnvelope{Error: httpapi.Error{Code: "x", Message: "m", RequestId: "req_0123456789abcdef0123456789abcdef", Details: map[string]any{}}})); err != nil {
			t.Error(err)
		}
	})

	t.Run("list envelopes", func(t *testing.T) {
		codec, _ := httpapi.NewCursorCodec(testKey)
		pos := func(s string) ([]byte, error) { return []byte(s), nil }
		next, _ := httpapi.BuildPage(codec, baseScope(), httpapi.PageRequest{Limit: 1}, []string{"a", "b"}, pos)
		last, _ := httpapi.BuildPage[string](codec, baseScope(), httpapi.PageRequest{Limit: 1}, nil, pos)
		obj, _ := httpapi.BuildPage(codec, baseScope(), httpapi.PageRequest{Limit: 1}, []map[string]string{{"id": "a"}}, func(map[string]string) ([]byte, error) { return []byte("a"), nil })
		for name, v := range map[string]any{"next": next, "last": last} {
			b := mustJSON(t, v)
			// Items are strings in this fixture; the schema requires objects, so
			// check the envelope keys and the cursor separately.
			var m map[string]json.RawMessage
			_ = json.Unmarshal(b, &m)
			if len(m) != 2 || m["items"] == nil || m["nextCursor"] == nil {
				t.Errorf("%s: %s", name, b)
			}
		}
		if err := d.valid(t, "Page", mustJSON(t, obj)); err != nil {
			t.Errorf("object page: %v", err)
		}
		if next.NextCursor == nil || !d.validString(t, "Cursor", *next.NextCursor) {
			t.Errorf("issued cursor does not satisfy Cursor schema: %v", next.NextCursor)
		}
		max, _ := codec.Encode(baseScope(), bytes.Repeat([]byte{'a'}, httpapi.MaxCursorPositionBytes))
		if !d.validString(t, "Cursor", max) {
			t.Error("largest cursor violates schema")
		}
	})

	t.Run("status codes follow the error schema", func(t *testing.T) {
		byStatus := map[int]string{
			400: "BadRequest", 401: "Unauthorized", 403: "Forbidden", 404: "NotFound", 409: "Conflict",
			412: "PreconditionFailed", 413: "PayloadTooLarge", 415: "UnsupportedMediaType", 428: "PreconditionRequired",
			429: "TooManyRequests", 500: "InternalError", 503: "Unavailable",
		}
		responses := dig(t, d.raw, "components", "responses").(map[string]any)
		for status, name := range byStatus {
			r, ok := responses[name]
			if !ok {
				t.Errorf("response %s (%d) missing", name, status)
				continue
			}
			if dig(t, r, "content", "application/json", "schema", "$ref") != "#/components/schemas/ErrorEnvelope" {
				t.Errorf("%s does not use the envelope", name)
			}
			if dig(t, r, "headers", "X-Request-ID", "$ref") != "#/components/headers/RequestId" {
				t.Errorf("%s lacks X-Request-ID", name)
			}
		}
		for _, name := range []string{"TooManyRequests", "Unavailable"} {
			if dig(t, responses[name], "headers", "Retry-After", "$ref") != "#/components/headers/RetryAfter" {
				t.Errorf("%s lacks Retry-After", name)
			}
		}
		codes := []domain.ErrorCode{
			domain.CodeInvalidRequest, httpapi.CodeInvalidCursor, domain.CodeInvalidCredential, domain.CodeForbidden,
			domain.CodeNotFound, domain.CodeIdempotencyConflict, domain.CodeVersionConflict, domain.CodeVersionRequired,
			domain.CodeRateLimited, domain.CodeUnavailable, httpapi.CodeInternal,
		}
		errorCodeDoc := dig(t, d.raw, "components", "schemas", "ErrorCode", "description").(string)
		for _, c := range codes {
			status, ok := httpapi.StatusFor(c)
			if !ok {
				t.Errorf("no status for %s", c)
				continue
			}
			name, ok := byStatus[status]
			if !ok {
				t.Errorf("%s maps to undeclared status %d", c, status)
				continue
			}
			desc := dig(t, responses[name], "description").(string)
			if !strings.Contains(desc, string(c)) && !strings.Contains(errorCodeDoc, string(c)) {
				t.Errorf("code %s (%d) undocumented", c, status)
			}
			if !strings.Contains(errorCodeDoc, string(c)) {
				t.Errorf("code %s missing from the ErrorCode description", c)
			}
			if !d.validString(t, "ErrorCode", string(c)) {
				t.Errorf("code %s violates the ErrorCode pattern", c)
			}
		}
	})

	t.Run("schemas agree with the shared primitives", func(t *testing.T) {
		for _, s := range []string{"a", "A_b-9", strings.Repeat("a", 128), "wi_1042"} {
			if !d.validString(t, "ID", s) || domain.TenantID(s).Validate() != nil {
				t.Errorf("ID %q", s)
			}
		}
		for _, s := range []string{"", "_x", "a b", "a/b", strings.Repeat("a", 129), "é"} {
			if d.validString(t, "ID", s) || domain.TenantID(s).Validate() == nil {
				t.Errorf("ID %q accepted by schema %v or domain %v", s, d.validString(t, "ID", s), domain.TenantID(s).Validate() == nil)
			}
		}
		if got := dig(t, d.raw, "components", "schemas", "Version", "maximum"); fmt.Sprint(got) != strconv.FormatInt(domain.MaxSafeInteger, 10) {
			t.Errorf("Version maximum = %v", got)
		}
		for _, v := range []domain.Version{1, 7, domain.MaxSafeInteger} {
			b, _ := json.Marshal(int64(v))
			tag, _ := httpapi.FormatETag(v)
			if d.valid(t, "Version", b) != nil || !d.validString(t, "ETagValue", tag) {
				t.Errorf("version %d / tag %s", v, tag)
			}
		}
		for _, v := range []int64{0, -1, domain.MaxSafeInteger + 1} {
			b, _ := json.Marshal(v)
			if d.valid(t, "Version", b) == nil {
				t.Errorf("version %d accepted by schema", v)
			}
		}
		for _, tag := range []string{`"7"`, `7`, `W/"7"`, `*`, `"0"`, `"07"`, `"-1"`, `""`, `"7", "8"`} {
			_, perr := httpapi.ParseETag(tag)
			if (perr == nil) != d.validString(t, "ETagValue", tag) {
				t.Errorf("ETag %q: parser ok=%v schema ok=%v", tag, perr == nil, d.validString(t, "ETagValue", tag))
			}
		}
		// The pattern cannot express the numeric range; the parser enforces it.
		if _, err := httpapi.ParseETag(`"9007199254740992"`); err == nil {
			t.Error("version above the safe integer accepted")
		}
		for _, k := range []string{"a", "k-1_2.3~4:5", strings.Repeat("k", 128), "", "a b", strings.Repeat("k", 129), "a/b"} {
			if (httpapi.ClientKey(k).Validate() == nil) != d.validString(t, "IdempotencyKeyValue", k) {
				t.Errorf("idempotency key %q disagrees", k)
			}
		}
	})

	t.Run("pagination and header parameters", func(t *testing.T) {
		lim := dig(t, d.raw, "components", "parameters", "Limit", "schema")
		if fmt.Sprint(dig(t, lim, "minimum")) != "1" || fmt.Sprint(dig(t, lim, "maximum")) != strconv.Itoa(httpapi.MaxLimit) ||
			fmt.Sprint(dig(t, lim, "default")) != strconv.Itoa(httpapi.DefaultLimit) {
			t.Errorf("limit schema = %v", lim)
		}
		if fmt.Sprint(dig(t, d.raw, "components", "schemas", "Cursor", "maxLength")) != strconv.Itoa(httpapi.MaxCursorBytes) {
			t.Error("cursor maxLength differs from MaxCursorBytes")
		}
		for _, p := range []string{"IfMatch", "IdempotencyKey"} {
			if dig(t, d.raw, "components", "parameters", p, "required") != true || dig(t, d.raw, "components", "parameters", p, "in") != "header" {
				t.Errorf("%s parameter", p)
			}
		}
		// Request IDs generated by the router satisfy the header schema.
		h := newHarness(t, nil)
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/x", nil))
		pattern := dig(t, d.raw, "components", "headers", "RequestId", "schema", "pattern").(string)
		if pattern != requestIDPattern.String() || !requestIDPattern.MatchString(rec.Header().Get("X-Request-ID")) {
			t.Errorf("request id %q vs %q", rec.Header().Get("X-Request-ID"), pattern)
		}
	})

	t.Run("generated types cover every schema", func(t *testing.T) {
		gen, err := os.ReadFile("common.gen.go")
		if err != nil {
			t.Fatal(err)
		}
		for name := range dig(t, d.raw, "components", "schemas").(map[string]any) {
			if !bytes.Contains(gen, []byte("type "+name+" ")) {
				t.Errorf("common.gen.go lacks type %s (run go generate ./internal/httpapi/...)", name)
			}
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
