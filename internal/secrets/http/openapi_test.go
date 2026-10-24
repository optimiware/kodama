package secretshttp_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/secrets"
	secretshttp "github.com/optimiware/kodama/internal/secrets/http"
)

type doc map[string]any

func loadYAML(t *testing.T, path string) doc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return d
}

func asMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case doc:
		return m
	}
	return nil
}

func get(v any, path ...string) any {
	for _, p := range path {
		m := asMap(v)
		if m == nil {
			return nil
		}
		v = m[p]
	}
	return v
}

// specs holds the documents a reference may point into.
type specs struct {
	cfg, common doc
}

// resolve follows a $ref (local, or into common.yaml) and returns the target.
func (s specs) resolve(t *testing.T, owner doc, ref string) (any, doc) {
	t.Helper()
	file, frag, _ := strings.Cut(ref, "#")
	target := owner
	switch file {
	case "":
	case "common.yaml":
		target = s.common
	default:
		t.Fatalf("reference %q points to an unexpected file", ref)
	}
	var cur any = map[string]any(target)
	for _, seg := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
		m := asMap(cur)
		if m == nil || m[seg] == nil {
			t.Fatalf("reference %q does not resolve (missing %q)", ref, seg)
		}
		cur = m[seg]
	}
	return cur, target
}

// walk visits every node with its owning document, checking each $ref.
func (s specs) walk(t *testing.T, owner doc, v any, visit func(node map[string]any)) {
	t.Helper()
	if x := asMap(v); x != nil {
		if ref, ok := x["$ref"].(string); ok {
			s.resolve(t, owner, ref)
		}
		visit(x)
		for _, c := range x {
			s.walk(t, owner, c, visit)
		}
		return
	}
	if l, ok := v.([]any); ok {
		for _, c := range l {
			s.walk(t, owner, c, visit)
		}
	}
}

type operation struct {
	method, path string
	spec         map[string]any
}

func operations(t *testing.T, d doc) []operation {
	t.Helper()
	var ops []operation
	for path, item := range asMap(d["paths"]) {
		for method, op := range asMap(item) {
			switch method {
			case "get", "post", "patch", "put", "delete":
				ops = append(ops, operation{strings.ToUpper(method), path, asMap(op)})
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].path+ops[i].method < ops[j].path+ops[j].method
	})
	return ops
}

func newContractHandler(t *testing.T) *secretshttp.Handler {
	t.Helper()
	// The route table does not depend on storage: a service over an unused
	// store is enough to construct the handler without PostgreSQL.
	svc, err := secrets.NewService(secrets.ServiceConfig{InstallationID: "i", Store: unusedStore{}, Wrapper: unusedWrapper{}, IDs: secrets.RandomIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	cursors, err := httpapi.NewCursorCodec(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h, err := secretshttp.NewHandler(secretshttp.Config{Service: svc, Keyed: unusedKeyed{}, Cursors: cursors, HashKey: bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type unusedStore struct{ secrets.Store }
type unusedWrapper struct{ secrets.KeyWrapper }
type unusedKeyed struct{ secretshttp.Keyed }

// Test_configuration_contract: routes, generated DTOs, metadata-only
// responses, ETags, cursors, error mappings and runtime DTO translation match
// the executable contracts. No external service is used.
func Test_configuration_contract(t *testing.T) {
	s := specs{cfg: loadYAML(t, "../../../api/configuration.yaml"), common: loadYAML(t, "../../../api/common.yaml")}
	if s.cfg["openapi"] != "3.0.3" {
		t.Fatalf("openapi %v", s.cfg["openapi"])
	}
	ops := operations(t, s.cfg)

	t.Run("every reference resolves", func(t *testing.T) {
		n := 0
		s.walk(t, s.cfg, map[string]any(s.cfg), func(node map[string]any) {
			if _, ok := node["$ref"]; ok {
				n++
			}
		})
		if n < 50 {
			t.Fatalf("only %d references walked", n)
		}
	})

	t.Run("routes equal the handler's table", func(t *testing.T) {
		h := newContractHandler(t)
		want := map[string]string{}
		for _, r := range h.Routes() {
			want[r.Method+" "+r.Path] = r.Operation
		}
		got := map[string]string{}
		for _, op := range ops {
			got[op.method+" "+op.path] = fmt.Sprint(op.spec["operationId"])
		}
		if len(want) != 12 || len(got) != 12 {
			t.Fatalf("handler routes %d, documented operations %d", len(want), len(got))
		}
		for k, id := range want {
			if got[k] != id {
				t.Errorf("%s: documented as %q, handler registers %q", k, got[k], id)
			}
		}
		// The management routes of docs/contracts/http-api.md, with P = /tenants/{tenantId}/projects/{projectId}.
		const p = "/tenants/{tenantId}/projects/{projectId}"
		for _, k := range []string{
			"GET " + p + "/variables", "POST " + p + "/variables",
			"GET " + p + "/variables/{id}", "PATCH " + p + "/variables/{id}", "DELETE " + p + "/variables/{id}",
			"GET " + p + "/secrets", "POST " + p + "/secrets", "GET " + p + "/secrets/{id}", "DELETE " + p + "/secrets/{id}",
			"GET " + p + "/secrets/{id}/versions", "POST " + p + "/secrets/{id}/versions",
			"POST " + p + "/secrets/{id}/versions/{version}/revoke",
		} {
			if _, ok := got[k]; !ok {
				t.Errorf("contract route %s is not documented", k)
			}
		}
		// There is no plaintext-read, resolve or rewrap route in the management interface.
		for k := range got {
			for _, banned := range []string{"/value", "/resolve", "/rewrap", "/plaintext", "/reveal"} {
				if strings.Contains(k, banned) {
					t.Errorf("management route %s exposes %s", k, banned)
				}
			}
		}
	})

	t.Run("responses are metadata only", func(t *testing.T) {
		forbidden := map[string]bool{
			"value": true, "valueBase64": true, "plaintext": true, "ciphertext": true, "nonce": true, "wrappedKey": true,
			"kek": true, "key": true, "fingerprint": true, "digest": true, "hash": true, "checksum": true, "payload": true, "token": true,
		}
		for _, op := range ops {
			if !strings.Contains(op.path, "/secrets") {
				continue
			}
			names := map[string]bool{}
			for _, resp := range asMap(op.spec["responses"]) {
				s.walkSchema(t, resp, names)
			}
			if op.method == "GET" && !strings.HasSuffix(op.path, "}") && len(names) == 0 {
				t.Errorf("%s %s: no response properties were collected; the check would be vacuous", op.method, op.path)
			}
			for n := range names {
				if forbidden[n] {
					t.Errorf("%s %s: a response schema declares %q", op.method, op.path, n)
				}
			}
		}
		// Request values are write-only.
		for _, name := range []string{"CreateSecretRequest", "CreateSecretVersionRequest"} {
			v := asMap(get(s.cfg, "components", "schemas", name, "properties", "value"))
			if v["writeOnly"] != true || v["format"] != "byte" {
				t.Errorf("%s.value must be write-only base64: %v", name, v)
			}
		}
		// No response schema anywhere has a write-only property.
		for name, schema := range asMap(get(s.cfg, "components", "schemas")) {
			if strings.HasSuffix(name, "Request") {
				continue
			}
			for pn, p := range asMap(get(schema, "properties")) {
				if asMap(p)["writeOnly"] == true {
					t.Errorf("%s.%s is write-only in a non-request schema", name, pn)
				}
			}
		}
		// Secret and version metadata declare no member that could carry material.
		for _, name := range []string{"Secret", "SecretVersion", "RetentionReport", "ConsumerHandlingSummary"} {
			for pn := range asMap(get(s.cfg, "components", "schemas", name, "properties")) {
				if forbidden[pn] {
					t.Errorf("%s.%s", name, pn)
				}
			}
			if get(s.cfg, "components", "schemas", name, "additionalProperties") != false {
				t.Errorf("%s must close additionalProperties", name)
			}
		}
	})

	t.Run("conventions: ETag, If-Match, Idempotency-Key, cursors", func(t *testing.T) {
		paramNames := func(op operation, item any) map[string]bool {
			out := map[string]bool{}
			add := func(list any) {
				l, _ := list.([]any)
				for _, p := range l {
					m := asMap(p)
					ref, _ := m["$ref"].(string)
					if ref != "" {
						r, _ := s.resolve(t, s.cfg, ref)
						m = asMap(r)
					}
					out[fmt.Sprint(m["name"])] = true
				}
			}
			add(op.spec["parameters"])
			add(get(item, "parameters"))
			return out
		}
		for _, op := range ops {
			item := get(s.cfg, "paths", op.path)
			params := paramNames(op, item)
			id := fmt.Sprint(op.spec["operationId"])
			wantIfMatch := map[string]bool{"updateVariable": true, "deleteVariable": true, "deleteSecret": true, "createSecretVersion": true}[id]
			wantKey := map[string]bool{"createVariable": true, "createSecret": true, "createSecretVersion": true}[id]
			wantPaging := map[string]bool{"listVariables": true, "listSecrets": true, "listSecretVersions": true}[id]
			if params["If-Match"] != wantIfMatch {
				t.Errorf("%s: If-Match declared=%v want %v", id, params["If-Match"], wantIfMatch)
			}
			if params["Idempotency-Key"] != wantKey {
				t.Errorf("%s: Idempotency-Key declared=%v want %v", id, params["Idempotency-Key"], wantKey)
			}
			if params["limit"] != wantPaging || params["cursor"] != wantPaging {
				t.Errorf("%s: paging parameters declared=%v/%v want %v", id, params["limit"], params["cursor"], wantPaging)
			}
			responses := asMap(op.spec["responses"])
			// 2xx single-resource responses carry the ETag; lists do not.
			for code, r := range responses {
				if !strings.HasPrefix(code, "2") || code == "204" {
					continue
				}
				hasETag := get(r, "headers", "ETag") != nil
				if hasETag == wantPaging {
					t.Errorf("%s %s: ETag header declared=%v for a %s", id, code, hasETag, map[bool]string{true: "list", false: "resource"}[wantPaging])
				}
			}
			for _, code := range []string{"401", "403", "429", "500", "503"} {
				if _, ok := responses[code]; !ok && !(code == "403" && false) {
					t.Errorf("%s: response %s is not documented", id, code)
				}
			}
			if wantIfMatch {
				for _, code := range []string{"412", "428"} {
					if _, ok := responses[code]; !ok {
						t.Errorf("%s: response %s is not documented", id, code)
					}
				}
			}
			if wantKey {
				if _, ok := responses["409"]; !ok {
					t.Errorf("%s: 409 is not documented", id)
				}
			}
		}
		// The page schemas have the shared envelope shape with a nullable cursor.
		for _, name := range []string{"VariablePage", "SecretPage", "SecretVersionPage"} {
			if get(s.cfg, "components", "schemas", name, "properties", "nextCursor", "nullable") != true {
				t.Errorf("%s.nextCursor must be nullable", name)
			}
		}
	})

	t.Run("error mappings are documented", func(t *testing.T) {
		byID := map[string]map[string]any{}
		for _, op := range ops {
			byID[fmt.Sprint(op.spec["operationId"])] = asMap(op.spec["responses"])
		}
		descr := func(id, code string) string { return fmt.Sprint(get(byID[id][code], "description")) }
		type mapping struct {
			err    error
			status int
			code   domain.ErrorCode
		}
		cases := []mapping{
			{secrets.ErrNotFound, http.StatusNotFound, domain.CodeNotFound},
			{fmt.Errorf("x: %w", secrets.ErrNameTaken), http.StatusConflict, secretshttp.CodeNameConflict},
			{secrets.ErrDeleted, http.StatusConflict, secretshttp.CodeResourceDeleted},
			{&secrets.ConflictError{Resource: "secret", Expected: 1, Current: 2}, http.StatusPreconditionFailed, domain.CodeVersionConflict},
			{&secrets.ConflictError{Resource: "value version", Expected: 1, Current: 2}, http.StatusPreconditionFailed, domain.CodeVersionConflict},
			{fmt.Errorf("%w: rule", secrets.ErrInvalid), http.StatusBadRequest, domain.CodeInvalidRequest},
			{secrets.ErrBackpressure, http.StatusServiceUnavailable, domain.CodeUnavailable},
			{secrets.ErrUnavailable, http.StatusServiceUnavailable, domain.CodeUnavailable},
			{&secrets.KeyError{Class: secrets.KeyUnavailable}, http.StatusServiceUnavailable, domain.CodeUnavailable},
			{&secrets.KeyError{Class: secrets.KeyDenied}, http.StatusServiceUnavailable, domain.CodeUnavailable},
			{&secrets.KeyError{Class: secrets.KeyNotFound}, http.StatusServiceUnavailable, domain.CodeUnavailable},
			{&secrets.KeyError{Class: secrets.KeyCorrupt}, http.StatusServiceUnavailable, domain.CodeUnavailable},
			{fmt.Errorf("pq: password=hunter2"), http.StatusInternalServerError, httpapi.CodeInternal},
		}
		for _, c := range cases {
			api := httpapi.ResolveError(secretshttp.MapError(c.err))
			if api.Status != c.status || api.Code != c.code {
				t.Errorf("%v -> %d %s, want %d %s", c.err, api.Status, api.Code, c.status, c.code)
			}
			if strings.Contains(api.Message, "hunter2") {
				t.Errorf("%v: the message leaks a cause", c.err)
			}
		}
		// The module codes are described where they can occur.
		for _, id := range []string{"createVariable", "createSecret"} {
			if !strings.Contains(descr(id, "409"), "name_conflict") || !strings.Contains(descr(id, "409"), "idempotency_conflict") {
				t.Errorf("%s: 409 does not document name_conflict and idempotency_conflict: %q", id, descr(id, "409"))
			}
		}
		for _, id := range []string{"updateVariable", "createSecretVersion"} {
			if !strings.Contains(descr(id, "409"), "resource_deleted") {
				t.Errorf("%s: 409 does not document resource_deleted", id)
			}
		}
		// Key provider details are part of the documented 503.
		if !strings.Contains(fmt.Sprint(get(s.cfg, "paths", "/tenants/{tenantId}/projects/{projectId}/secrets", "post", "description")), "retryable") {
			t.Error("createSecret does not document the key provider failure details")
		}
		// Every status the mapping can produce for an operation is documented there.
		for _, id := range []string{"createSecret", "createSecretVersion", "revokeSecretVersion", "deleteSecret", "createVariable", "updateVariable"} {
			for _, code := range []string{"400", "404", "503"} {
				if id == "revokeSecretVersion" && code == "400" {
					continue
				}
				if _, ok := byID[id][code]; !ok {
					t.Errorf("%s: %s is not documented", id, code)
				}
			}
		}
	})

	t.Run("generated DTOs match the schemas", func(t *testing.T) {
		types := map[string]reflect.Type{
			"Variable": reflect.TypeOf(secretshttp.Variable{}), "CreateVariableRequest": reflect.TypeOf(secretshttp.CreateVariableRequest{}),
			"UpdateVariableRequest": reflect.TypeOf(secretshttp.UpdateVariableRequest{}), "VariablePage": reflect.TypeOf(secretshttp.VariablePage{}),
			"Secret": reflect.TypeOf(secretshttp.Secret{}), "SecretPage": reflect.TypeOf(secretshttp.SecretPage{}),
			"SecretVersion": reflect.TypeOf(secretshttp.SecretVersion{}), "SecretVersionPage": reflect.TypeOf(secretshttp.SecretVersionPage{}),
			"CreateSecretRequest": reflect.TypeOf(secretshttp.CreateSecretRequest{}), "CreateSecretVersionRequest": reflect.TypeOf(secretshttp.CreateSecretVersionRequest{}),
			"SecretVersionResult": reflect.TypeOf(secretshttp.SecretVersionResult{}), "RevokeSecretVersionRequest": reflect.TypeOf(secretshttp.RevokeSecretVersionRequest{}),
			"RevokeSecretVersionResult": reflect.TypeOf(secretshttp.RevokeSecretVersionResult{}), "ConsumerHandlingSummary": reflect.TypeOf(secretshttp.ConsumerHandlingSummary{}),
			"DeleteSecretResult": reflect.TypeOf(secretshttp.DeleteSecretResult{}), "RetentionReport": reflect.TypeOf(secretshttp.RetentionReport{}),
		}
		for name, typ := range types {
			schema := asMap(get(s.cfg, "components", "schemas", name))
			if schema == nil {
				t.Errorf("%s is not a documented schema", name)
				continue
			}
			props := asMap(schema["properties"])
			required := map[string]bool{}
			if rs, ok := schema["required"].([]any); ok {
				for _, r := range rs {
					required[fmt.Sprint(r)] = true
				}
			}
			fields := map[string]string{}
			for i := 0; i < typ.NumField(); i++ {
				tag := typ.Field(i).Tag.Get("json")
				n, opts, _ := strings.Cut(tag, ",")
				fields[n] = opts
			}
			for pn := range props {
				opts, ok := fields[pn]
				if !ok {
					t.Errorf("%s: documented property %q has no generated field", name, pn)
					continue
				}
				writeOnly := asMap(props[pn])["writeOnly"] == true
				if required[pn] && !writeOnly && strings.Contains(opts, "omitempty") {
					t.Errorf("%s.%s is required but optional in the DTO", name, pn)
				}
			}
			for fn := range fields {
				if _, ok := props[fn]; !ok {
					t.Errorf("%s: generated field %q is not documented", name, fn)
				}
			}
		}
		// Every documented object schema has a generated type (none is silently missing).
		for name, schema := range asMap(get(s.cfg, "components", "schemas")) {
			if asMap(schema)["type"] == "object" && types[name] == nil {
				t.Errorf("schema %s has no generated DTO in the comparison table", name)
			}
		}
	})

	t.Run("runtime resolution is the runtime contract's, not this document's", func(t *testing.T) {
		rt := loadYAML(t, "../../../api/runtime.yaml")
		if get(rt, "paths", "/runtime/v1/attempts/{attemptId}/secrets/resolve", "post") == nil {
			t.Fatal("api/runtime.yaml has no secret resolve operation")
		}
		var found bool
		for _, o := range runtimev1.Operations {
			if o.ID == "resolveSecret" && o.Method == "POST" && o.Request == "SecretResolveRequest" && o.Responses[200] == "SecretResolveResponse" {
				found = true
			}
		}
		if !found {
			t.Fatal("the executable runtime contract changed its secret resolution operation")
		}
		for _, op := range ops {
			if strings.Contains(op.path, "resolve") || strings.Contains(op.path, "attempts") {
				t.Errorf("the management document describes %s", op.path)
			}
		}
	})
}

// walkSchema collects every property name reachable from a response object.
func (s specs) walkSchema(t *testing.T, node any, names map[string]bool) {
	t.Helper()
	seen := map[string]bool{}
	var visit func(owner doc, v any)
	visit = func(owner doc, v any) {
		if x := asMap(v); x != nil {
			if ref, ok := x["$ref"].(string); ok {
				if seen[ref] {
					return
				}
				seen[ref] = true
				target, targetOwner := s.resolve(t, owner, ref)
				visit(targetOwner, target)
				return
			}
			for k, c := range x {
				if k == "properties" {
					for pn, p := range asMap(c) {
						names[pn] = true
						visit(owner, p)
					}
					continue
				}
				visit(owner, c)
			}
			return
		}
		if l, ok := v.([]any); ok {
			for _, c := range l {
				visit(owner, c)
			}
		}
	}
	visit(s.cfg, node)
}
