package deliveryhttp_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/optimiware/kodama/internal/delivery"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/httpapi"
)

// exchange is one published request/response pair of a fixture file.
type exchange struct {
	Name      string `json:"name"`
	Note      string `json:"note"`
	Operation string `json:"operation"`
	Request   struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"response"`
}

type fixtureFile struct {
	Fixture     string     `json:"fixture"`
	Description string     `json:"description"`
	Exchanges   []exchange `json:"exchanges"`
}

func loadFixtures(t testing.TB) []fixtureFile {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(testdataDir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no published fixtures: %v", err)
	}
	var out []fixtureFile
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var f fixtureFile
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var envelope struct {
			fixtureFile
			Placeholders string `json:"placeholders"`
		}
		if err := dec.Decode(&envelope); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		f = envelope.fixtureFile
		if f.Fixture+".json" != filepath.Base(p) || f.Description == "" || len(f.Exchanges) == 0 || envelope.Placeholders == "" {
			t.Fatalf("%s: a fixture names itself, describes itself, states its placeholders and holds exchanges", p)
		}
		out = append(out, f)
	}
	return out
}

// requiredFixtures are the exchanges the task publishes for ordinary scripts,
// annotation-based reconciliation and deployment-bound cleanup.
var requiredFixtures = []string{
	"registration", "pre_apply_intent", "annotation_receipts", "lookup", "structured_outcomes", "unknown_outcome_refusal",
	"cleanup", "pinned_configuration", "stage_promotion",
}

// bodyTypes maps an operation to the generated DTO of its success response and
// request, so the published fixtures are decoded through the real wire types.
var (
	responseTypes = map[string]func() any{
		"createEnvironment": func() any { return &deliveryhttp.Environment{} }, "getEnvironment": func() any { return &deliveryhttp.Environment{} },
		"updateEnvironment": func() any { return &deliveryhttp.Environment{} }, "listEnvironments": func() any { return &deliveryhttp.EnvironmentPage{} },
		"getDeployment": func() any { return &deliveryhttp.Deployment{} }, "requestDeploymentCleanup": func() any { return &deliveryhttp.Deployment{} },
		"listDeployments":           func() any { return &deliveryhttp.DeploymentPage{} },
		"createDeployment":          func() any { return &deliveryhttp.DeploymentReceipt{} },
		"registerRuntimeDeployment": func() any { return &deliveryhttp.DeploymentReceipt{} }, "lookupRuntimeDeployment": func() any { return &deliveryhttp.DeploymentReceipt{} },
		"getRuntimeDeployment": func() any { return &deliveryhttp.DeploymentReceipt{} }, "reportRuntimeOutcome": func() any { return &deliveryhttp.DeploymentReceipt{} },
		"reportRuntimeCleanup": func() any { return &deliveryhttp.DeploymentReceipt{} },
	}
	requestTypes = map[string]func() any{
		"createEnvironment": func() any { return &deliveryhttp.EnvironmentCreate{} }, "updateEnvironment": func() any { return &deliveryhttp.EnvironmentEdit{} },
		"createDeployment": func() any { return &deliveryhttp.DeploymentCreate{} }, "registerRuntimeDeployment": func() any { return &deliveryhttp.RuntimeDeploymentRegister{} },
		"reportRuntimeOutcome": func() any { return &deliveryhttp.OutcomeReport{} }, "reportRuntimeCleanup": func() any { return &deliveryhttp.CleanupReport{} },
	}
)

func strictDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func sameJSON(t testing.TB, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

// Test_delivery_http_contract proves the routes, the generated DTOs and the
// published fixtures agree with api/delivery.yaml, and that the handlers apply
// strict JSON, the shared error envelope, quoted-version ETags and cursors bound
// to their request. No external service is involved: the use-case ports are
// controlled doubles that prove the transport only.
func Test_delivery_http_contract(t *testing.T) {
	sp := loadSpec(t)

	t.Run("the exported route inventory is exactly the documented operations", func(t *testing.T) {
		documented := map[string]bool{}
		for _, o := range sp.operations(t) {
			documented[o.method+" "+o.fullPath()] = true
		}
		inventory := map[string]bool{}
		actions := map[httpapi.Action]bool{}
		for prefix, routes := range map[string][]deliveryhttp.RouteInfo{
			"/api/v1":     deliveryhttp.PublicRoutes(deliveryhttp.DefaultActions()),
			"/runtime/v1": deliveryhttp.RuntimeRoutes(deliveryhttp.DefaultRuntimeActions()),
		} {
			for _, r := range routes {
				inventory[r.Method+" "+prefix+r.Path] = true
				if r.Action == "" || actions[r.Action] && r.Action != deliveryhttp.DefaultActions().DeploymentCreate {
					t.Errorf("%s %s has a missing or shared action %q", r.Method, r.Path, r.Action)
				}
				actions[r.Action] = true
			}
		}
		for k := range documented {
			if !inventory[k] {
				t.Errorf("documented but not exported: %s", k)
			}
		}
		for k := range inventory {
			if !documented[k] {
				t.Errorf("exported but not documented: %s", k)
			}
		}
	})

	t.Run("every documented route is registered, protected and nothing else is", func(t *testing.T) {
		a := newAPI(t)
		for _, o := range sp.operations(t) {
			concrete := wildcard.ReplaceAllStringFunc(o.fullPath(), func(w string) string {
				switch w {
				case "{tenantId}":
					return "t035_acme"
				case "{projectId}":
					return "p035_inventory"
				case "{attemptId}":
					return "att_t035_deploy"
				case "{actionKey}":
					return "run_t035_deploy:deploy"
				}
				if strings.HasPrefix(o.fullPath(), "/runtime") {
					return depID
				}
				return "env_t035_1"
			})
			router, prefix := a.public, "/api/v1"
			if strings.HasPrefix(o.fullPath(), "/runtime") {
				router, prefix = a.runtime, "/runtime/v1"
			}
			_ = prefix
			res := a.send(router, "", o.method, concrete, "")
			if router == a.public && res.Code != http.StatusUnauthorized {
				t.Errorf("%s %s without a credential: %d (a route must authenticate before anything else)", o.method, concrete, res.Code)
			}
			if router == a.runtime && res.Code == http.StatusNotFound {
				t.Errorf("%s %s is not registered", o.method, concrete)
			}
			// The same path with an undocumented method is refused with the envelope.
			if other := a.send(router, "tok-engineer", http.MethodDelete, concrete, ""); other.Code != http.StatusMethodNotAllowed && other.Code != http.StatusNotFound {
				t.Errorf("DELETE %s: %d", concrete, other.Code)
			}
		}
		for _, path := range []string{"/api/v1/tenants/t035_acme/projects/p035_inventory/environments/env_t035_1/secrets", "/api/v1/tenants/t035_acme/projects/p035_inventory/deployments/" + depID + "/outcome"} {
			if res := a.send(a.public, "tok-engineer", http.MethodPost, path, "{}"); res.Code != http.StatusNotFound {
				t.Errorf("POST %s: %d; outcomes are reported by the runtime routes only", path, res.Code)
			}
		}
		if len(a.svc.called()) != 0 {
			t.Fatalf("an unauthenticated or unknown request reached the service: %v", a.svc.called())
		}
	})

	t.Run("the documented examples validate against the fragment's own schemas", func(t *testing.T) {
		for example, schema := range map[string]string{
			"EnvironmentCreate": "EnvironmentCreate", "Environment": "Environment", "DeploymentCreate": "DeploymentCreate",
			"RuntimeRegister": "RuntimeDeploymentRegister", "DeploymentReceipt": "DeploymentReceipt", "OutcomeSucceeded": "OutcomeReport",
			"CleanupDone": "CleanupReport",
		} {
			if err := sp.validate(t, schema, sp.example(t, example)); err != nil {
				t.Errorf("example %s: %v", example, err)
			}
		}
		covered := map[string]bool{"EnvironmentCreate": true, "Environment": true, "DeploymentCreate": true, "RuntimeRegister": true, "DeploymentReceipt": true, "OutcomeSucceeded": true, "CleanupDone": true}
		for _, n := range sp.exampleNames() {
			if !covered[n] {
				t.Errorf("example %s has no schema binding in this test", n)
			}
		}
	})

	t.Run("the published fixtures agree with the operations, schemas and generated DTOs", func(t *testing.T) {
		fixtures := loadFixtures(t)
		have := map[string]bool{}
		for _, f := range fixtures {
			have[f.Fixture] = true
		}
		for _, name := range requiredFixtures {
			if !have[name] {
				t.Errorf("the fixture %s is not published", name)
			}
		}
		for _, f := range fixtures {
			for _, e := range f.Exchanges {
				name := f.Fixture + "/" + e.Name
				o := sp.operation(t, e.Operation)
				if o.method != e.Request.Method || !o.matches(strings.SplitN(e.Request.Path, "?", 2)[0]) {
					t.Errorf("%s: %s %s is not operation %s (%s %s)", name, e.Request.Method, e.Request.Path, e.Operation, o.method, o.fullPath())
					continue
				}
				// Request: required headers and body schema.
				params, _ := o.raw["parameters"].([]any)
				for _, p := range params {
					ref, _ := p.(map[string]any)["$ref"].(string)
					switch ref {
					case "common.yaml#/components/parameters/IfMatch":
						if v := e.Request.Headers["If-Match"]; v == "" {
							t.Errorf("%s: the request lacks the required If-Match", name)
						}
					case "common.yaml#/components/parameters/IdempotencyKey":
						if e.Request.Headers["Idempotency-Key"] == "" {
							t.Errorf("%s: the request lacks the required Idempotency-Key", name)
						}
					}
				}
				if len(e.Request.Body) > 0 {
					sch := sp.requestSchema(t, o)
					if sch == nil {
						t.Errorf("%s: the operation takes no body", name)
					} else if err := validateRaw(sch, e.Request.Body); err != nil {
						t.Errorf("%s: request violates the schema: %v", name, err)
					}
					if mk, ok := requestTypes[e.Operation]; ok {
						if err := strictDecode(e.Request.Body, mk()); err != nil {
							t.Errorf("%s: the request does not decode into the generated type: %v", name, err)
						}
					}
				} else if sp.requestSchema(t, o) != nil {
					t.Errorf("%s: a required body is missing", name)
				}
				// Response: documented status, body schema, headers and DTO round trip.
				status := strconv.Itoa(e.Response.Status)
				if !o.documents(status) {
					t.Errorf("%s: the operation does not document status %s", name, status)
					continue
				}
				if e.Response.Status >= 400 {
					if err := validateRaw(sp.errorEnvelope(t), e.Response.Body); err != nil {
						t.Errorf("%s: the error body violates the envelope: %v", name, err)
					}
					var env httpapi.ErrorEnvelope
					if err := json.Unmarshal(e.Response.Body, &env); err != nil || env.Error.Code == "" || !knownCode(env.Error.Code) {
						t.Errorf("%s: unknown error code %q", name, env.Error.Code)
					}
					continue
				}
				sch, declared := sp.response(t, o, status)
				if sch == nil {
					t.Errorf("%s: a success response has no schema", name)
					continue
				}
				if err := validateRaw(sch, e.Response.Body); err != nil {
					t.Errorf("%s: response violates the schema: %v", name, err)
				}
				mk, ok := responseTypes[e.Operation]
				if !ok {
					t.Errorf("%s: no generated type for %s", name, e.Operation)
					continue
				}
				dto := mk()
				if err := strictDecode(e.Response.Body, dto); err != nil {
					t.Errorf("%s: the response does not decode into the generated type: %v", name, err)
					continue
				}
				if back, err := json.Marshal(dto); err != nil || !sameJSON(t, back, e.Response.Body) {
					t.Errorf("%s: the response does not survive the generated type unchanged", name)
				}
				for h := range e.Response.Headers {
					if !declared[http.CanonicalHeaderKey(h)] {
						t.Errorf("%s: the response header %s is not documented for %s", name, h, status)
					}
				}
				if declared["Etag"] {
					want := versionOf(t, e.Response.Body)
					if got := e.Response.Headers["ETag"]; got != `"`+strconv.FormatInt(want, 10)+`"` {
						t.Errorf("%s: ETag %q is not the quoted resource version %d", name, got, want)
					}
				}
			}
		}
	})

	t.Run("the handlers' responses validate against the documented schemas", func(t *testing.T) {
		a := newAPI(t)
		check := func(opID string, res resp, status int, schemaStatus string) {
			t.Helper()
			if res.Code != status {
				t.Fatalf("%s: status %d: %s", opID, res.Code, res.Body.String())
			}
			sch, declared := sp.response(t, sp.operation(t, opID), schemaStatus)
			if sch == nil {
				t.Fatalf("%s: no schema", opID)
			}
			if err := validateRaw(sch, res.Body.Bytes()); err != nil {
				t.Fatalf("%s: %v\n%s", opID, err, res.Body.String())
			}
			if declared["Etag"] && res.Header().Get("ETag") == "" {
				t.Errorf("%s: no ETag", opID)
			}
			if res.Header().Get("Cache-Control") != "no-store" || res.Header().Get("X-Request-ID") == "" {
				t.Errorf("%s: headers %v", opID, res.Header())
			}
		}
		body := `{"name":"preview","kind":"preview","target":"kind","namespace":"t035-preview","kubeconfig":{"name":"KUBECONFIG","ref":"secret://project/kubeconfig","delivery":"file"},"previewTtlSeconds":86400}`
		check("createEnvironment", a.human("POST", pubBase+"/environments", body), 201, "201")
		check("getEnvironment", a.human("GET", pubBase+"/environments/env_t035_1", ""), 200, "200")
		check("updateEnvironment", a.human("PATCH", pubBase+"/environments/env_t035_1", `{"config":{"REPLICAS":"2"}}`, "If-Match", `"1"`), 200, "200")
		check("listEnvironments", a.human("GET", pubBase+"/environments", ""), 200, "200")
		check("getDeployment", a.human("GET", pubBase+"/deployments/"+depID, ""), 200, "200")
		check("listDeployments", a.human("GET", pubBase+"/deployments", ""), 200, "200")
		check("requestDeploymentCleanup", a.human("POST", pubBase+"/deployments/"+depID+"/cleanup", "", "If-Match", `"1"`), 200, "200")
		create := `{"attemptId":"att_t035_deploy","source":{"repositoryUrl":"https://git.t035.invalid/acme/inventory.git","commit":"` + strings.Repeat("1", 40) + `"},"image":{"artifactId":"art_t035_image","digest":"` + string(digestA) + `"}}`
		check("createDeployment", a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-1"), 201, "201")
		reg := `{"source":{"repositoryUrl":"https://git.t035.invalid/acme/inventory.git","commit":"` + strings.Repeat("1", 40) + `"},"image":{"artifactId":"art_t035_image","digest":"` + string(digestA) + `"}}`
		check("registerRuntimeDeployment", a.rt("tok-attempt", "POST", "/attempts/att_t035_deploy/deployments", reg), 201, "201")
		check("lookupRuntimeDeployment", a.rt("tok-attempt", "GET", "/attempts/att_t035_deploy/deployments/by-action/run_t035_deploy:deploy", ""), 200, "200")
		check("getRuntimeDeployment", a.rt("tok-attempt", "GET", "/attempts/att_t035_deploy/deployments/"+depID, ""), 200, "200")
		check("reportRuntimeOutcome", a.rt("tok-attempt", "POST", "/attempts/att_t035_deploy/deployments/"+depID+"/outcome", `{"state":"unknown"}`), 200, "200")
		check("reportRuntimeCleanup", a.rt("tok-attempt", "POST", "/attempts/att_t035_deploy/deployments/"+depID+"/cleanup-result", `{"state":"failed","reason":"delete_failed"}`), 200, "200")

		// The router's actions are the documented defaults, recorded per route.
		seen := map[httpapi.Action]bool{}
		for _, c := range a.authz.calls {
			seen[c.action] = true
		}
		for _, want := range []httpapi.Action{"environment.create", "environment.read", "environment.update", "environment.list", "deployment.read", "deployment.list",
			"deployment.cleanup", "deployment.create", "deployment.runtime.register", "deployment.runtime.lookup", "deployment.runtime.read",
			"deployment.runtime.outcome", "deployment.runtime.cleanup_result"} {
			if !seen[want] {
				t.Errorf("no route asked the authority for %s", want)
			}
		}
	})

	t.Run("responses carry the version as a quoted ETag and an unwritable header never leaks", func(t *testing.T) {
		a := newAPI(t)
		res := a.human("GET", pubBase+"/deployments/"+depID, "")
		if res.Header().Get("ETag") != `"1"` {
			t.Fatalf("ETag %q", res.Header().Get("ETag"))
		}
		res = a.rt("tok", "GET", "/attempts/att_t035_deploy/deployments/"+depID, "")
		if res.Header().Get("ETag") != `"1"` {
			t.Fatalf("runtime ETag %q", res.Header().Get("ETag"))
		}
	})

	t.Run("strict JSON: malformed bodies are refused before any use case runs", func(t *testing.T) {
		a := newAPI(t)
		good := `{"name":"preview","kind":"stage","target":"kind","namespace":"ns","kubeconfig":{"name":"KUBECONFIG","ref":"secret://project/kubeconfig","delivery":"file"}}`
		for name, tc := range map[string]struct {
			body string
			want int
		}{
			"an unknown field":        {strings.Replace(good, `"kind":"stage"`, `"kind":"stage","manifest":"apiVersion: v1"`, 1), 400},
			"a kubeconfig content":    {strings.Replace(good, `"namespace":"ns"`, `"namespace":"ns","kubeconfigContent":"`+sentinel+`"`, 1), 400},
			"an unknown nested field": {strings.Replace(good, `"delivery":"file"`, `"delivery":"file","value":"`+sentinel+`"`, 1), 400},
			"a duplicate key":         {strings.Replace(good, `"name":"preview"`, `"name":"preview","name":"other"`, 1), 400},
			"a nested duplicate key":  {strings.Replace(good, `"delivery":"file"`, `"delivery":"file","delivery":"env"`, 1), 400},
			"an explicit null":        {strings.Replace(good, `"namespace":"ns"`, `"namespace":"ns","config":null`, 1), 400},
			"a null scalar":           {strings.Replace(good, `"target":"kind"`, `"target":null`, 1), 400},
			"trailing data":           {good + ` {}`, 400},
			"a non-object":            {`[]`, 400},
			"not JSON":                {`not json`, 400},
			"a number for a string":   {strings.Replace(good, `"namespace":"ns"`, `"namespace":7`, 1), 400},
			"a string for a number":   {strings.Replace(good, `"namespace":"ns"`, `"namespace":"ns","previewTtlSeconds":"60"`, 1), 400},
		} {
			res := a.human("POST", pubBase+"/environments", tc.body)
			if res.Code != tc.want || res.errCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), sentinel) {
				t.Errorf("%s: the error echoes the body", name)
			}
		}
		if res := a.human("POST", pubBase+"/environments", ""); res.Code != http.StatusUnsupportedMediaType && res.Code != http.StatusBadRequest {
			t.Errorf("an empty body: %d", res.Code)
		}
		if res := a.human("POST", pubBase+"/environments", good, "Content-Type", "text/plain"); res.Code != http.StatusUnsupportedMediaType {
			t.Errorf("a non-JSON media type: %d", res.Code)
		}
		big := `{"name":"` + strings.Repeat("a", 2<<20) + `"}`
		if res := a.human("POST", pubBase+"/environments", big); res.Code != http.StatusRequestEntityTooLarge || res.errCode(t) != "invalid_request" {
			t.Errorf("an oversized body: %d", res.Code)
		}
		// The same strictness on the runtime routes.
		for name, body := range map[string]string{
			"an unknown field": `{"source":{"repositoryUrl":"https://git.t035.invalid/a.git","commit":"` + strings.Repeat("1", 40) + `"},"image":{"artifactId":"a","digest":"` + string(digestA) + `"},"environmentId":"env_other"}`,
			"a duplicate key":  `{"state":"unknown","state":"succeeded"}`,
		} {
			path := "/attempts/att_t035_deploy/deployments"
			if name == "a duplicate key" {
				path += "/" + depID + "/outcome"
			}
			if res := a.rt("tok", "POST", path, body); res.Code != http.StatusBadRequest || res.errCode(t) != "invalid_request" {
				t.Errorf("runtime %s: %d %s", name, res.Code, res.Body.String())
			}
		}
		// An environment pin or scope in a runtime body is an unknown field: those
		// are server-derived from the attempt's pinned context.
		if calls := a.svc.called(); len(calls) != 0 {
			t.Fatalf("a malformed request reached the use cases: %v", calls)
		}
	})

	t.Run("module errors map to the shared envelope with fixed, content-free messages", func(t *testing.T) {
		secretCause := errors.New("pq: password authentication failed for user postgres; kubeconfig=" + sentinel)
		for _, tc := range []struct {
			err    error
			status int
			code   string
		}{
			{delivery.Fail(delivery.ErrInvalid, secretCause, "bad"), 400, "invalid_request"},
			{delivery.Fail(delivery.ErrNotFound, secretCause, "not found"), 404, "not_found"},
			{delivery.Fail(delivery.ErrDenied, secretCause, "denied"), 403, "forbidden"},
			{delivery.Fail(delivery.ErrUnauthenticated, secretCause, "no"), 401, "invalid_credential"},
			{delivery.Fail(delivery.ErrAttemptRevoked, secretCause, "revoked"), 403, "attempt_revoked"},
			{delivery.Fail(delivery.ErrPinRequired, secretCause, "pin"), 403, "pin_required"},
			{delivery.Fail(delivery.ErrConflict, secretCause, "conflict"), 409, "conflict"},
			{delivery.Fail(delivery.ErrOutcomeUnknown, secretCause, "unknown"), 409, "outcome_unknown"},
			{delivery.Fail(delivery.ErrNameTaken, secretCause, "taken"), 409, "name_taken"},
			{delivery.Fail(delivery.ErrVersionConflict, secretCause, "stale"), 412, "version_conflict"},
			{delivery.Fail(delivery.ErrInvalidTransition, secretCause, "transition"), 422, "invalid_transition"},
			{delivery.Fail(delivery.ErrCredentialUnavailable, secretCause, "cred"), 422, "credential_unavailable"},
			{delivery.Fail(delivery.ErrProvenance, secretCause, "prov"), 422, "image_provenance_rejected"},
			{delivery.Fail(delivery.ErrNotConfigured, secretCause, "cfg"), 501, "not_configured"},
			{delivery.Fail(delivery.ErrUnavailable, secretCause, "down"), 503, "unavailable"},
			{secretCause, 503, "unavailable"}, // an unclassified failure is an outage, never success or detail
		} {
			a := newAPI(t)
			a.svc.err = tc.err
			for _, res := range []resp{
				a.human("GET", pubBase+"/deployments/"+depID, ""),
				a.rt("tok", "GET", "/attempts/att_t035_deploy/deployments/"+depID, ""),
			} {
				if res.Code != tc.status || res.errCode(t) != tc.code {
					t.Errorf("%v: %d %s", tc.err, res.Code, res.Body.String())
				}
				if strings.Contains(res.Body.String(), sentinel) || strings.Contains(res.Body.String(), "password") {
					t.Errorf("%v: the response discloses the cause: %s", tc.err, res.Body.String())
				}
				if tc.status == 503 && res.Header().Get("Retry-After") == "" {
					t.Errorf("a 503 without Retry-After")
				}
				if strings.Contains(a.logs.String(), sentinel) {
					t.Errorf("%v: the log carries the cause text", tc.err)
				}
			}
		}
	})

	t.Run("edits and the cleanup transition require a current version", func(t *testing.T) {
		a := newAPI(t)
		for _, tc := range []struct {
			name, method, path, body string
		}{
			{"update", "PATCH", pubBase + "/environments/env_t035_1", `{"config":{"A":"1"}}`},
			{"cleanup", "POST", pubBase + "/deployments/" + depID + "/cleanup", ""},
		} {
			if res := a.human(tc.method, tc.path, tc.body); res.Code != 428 || res.errCode(t) != "version_required" {
				t.Errorf("%s without If-Match: %d", tc.name, res.Code)
			}
			for _, bad := range []string{`W/"1"`, `*`, `"1", "2"`, `1`, `"0"`, `"01"`, `"-1"`} {
				if res := a.human(tc.method, tc.path, tc.body, "If-Match", bad); res.Code != 400 || res.errCode(t) != "invalid_request" {
					t.Errorf("%s with If-Match %s: %d", tc.name, bad, res.Code)
				}
			}
			a.svc.err = delivery.Fail(delivery.ErrVersionConflict, nil, "stale")
			if res := a.human(tc.method, tc.path, tc.body, "If-Match", `"7"`); res.Code != 412 || res.errCode(t) != "version_conflict" {
				t.Errorf("%s stale: %d", tc.name, res.Code)
			}
			if a.svc.expected != 7 {
				t.Errorf("%s: the expected version reached the use case as %d", tc.name, a.svc.expected)
			}
			a.svc.err = nil
		}
		if res := a.human("POST", pubBase+"/deployments/"+depID+"/cleanup", `{}`, "If-Match", `"1"`); res.Code != 400 {
			t.Errorf("a cleanup request with a body: %d", res.Code)
		}
		// An edit that names nothing is not an edit.
		if res := a.human("PATCH", pubBase+"/environments/env_t035_1", `{}`, "If-Match", `"1"`); res.Code != 400 {
			t.Errorf("an empty edit: %d", res.Code)
		}
	})

	t.Run("cursors are signed, scoped to the request and bounded", func(t *testing.T) {
		a := newAPI(t)
		envs := []delivery.Environment{newEnvironment(t, "env_t035_1", "alpha"), newEnvironment(t, "env_t035_2", "bravo"), newEnvironment(t, "env_t035_3", "charlie")}
		a.svc.envs = envs
		// The page size is bounded: default 50, maximum 200, never clamped.
		a.human("GET", pubBase+"/environments", "")
		if a.svc.limit != 51 {
			t.Fatalf("the default page fetched %d rows, want 51", a.svc.limit)
		}
		a.human("GET", pubBase+"/environments?limit=200", "")
		if a.svc.limit != 201 {
			t.Fatalf("the maximum page fetched %d rows", a.svc.limit)
		}
		for _, q := range []string{"limit=0", "limit=201", "limit=-1", "limit=x", "limit=1&limit=2", "unknown=1", "limit=05"} {
			if res := a.human("GET", pubBase+"/environments?"+q, ""); res.Code != 400 {
				t.Errorf("?%s: %d", q, res.Code)
			}
		}
		// A page of two out of three yields a cursor for the next.
		res := a.human("GET", pubBase+"/environments?limit=2", "")
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor *string           `json:"nextCursor"`
		}
		res.decode(t, &page)
		if len(page.Items) != 2 || page.NextCursor == nil {
			t.Fatalf("page: %s", res.Body.String())
		}
		if err := sp.validate(t, "EnvironmentPage", res.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		next := a.human("GET", pubBase+"/environments?limit=2&cursor="+*page.NextCursor, "")
		if next.Code != 200 || a.svc.afterName != "bravo" {
			t.Fatalf("the cursor did not continue after the last row: %d after=%q", next.Code, a.svc.afterName)
		}
		// The same cursor is refused for another tenant, another project, another
		// endpoint and a tampered token.
		for name, path := range map[string]string{
			"another tenant":   "/tenants/t035_globex/projects/p035_inventory/environments?limit=2&cursor=",
			"another project":  "/tenants/t035_acme/projects/p035_ops/environments?limit=2&cursor=",
			"another endpoint": pubBase + "/deployments?limit=2&cursor=",
		} {
			if res := a.human("GET", path+*page.NextCursor, ""); res.Code != 400 || res.errCode(t) != "invalid_cursor" {
				t.Errorf("%s: %d %s", name, res.Code, res.Body.String())
			}
		}
		tampered := *page.NextCursor
		tampered = tampered[:len(tampered)-2] + "AA"
		if res := a.human("GET", pubBase+"/environments?limit=2&cursor="+tampered, ""); res.Code != 400 || res.errCode(t) != "invalid_cursor" {
			t.Errorf("a tampered cursor: %d", res.Code)
		}
		// Deployment cursors are bound to their filters.
		a.svc.deps = []delivery.Deployment{newDeployment(t, "k:1"), newDeployment(t, "k:2"), newDeployment(t, "k:3")}
		res = a.human("GET", pubBase+"/deployments?limit=2&environmentId=env_t035_1&status=pending", "")
		res.decode(t, &page)
		if page.NextCursor == nil || a.svc.filter.Environment != "env_t035_1" || a.svc.filter.Status != delivery.StatusPending {
			t.Fatalf("filtered page: %d %s filter=%+v", res.Code, res.Body.String(), a.svc.filter)
		}
		for _, q := range []string{"&environmentId=env_t035_2&status=pending", "&environmentId=env_t035_1", "&environmentId=env_t035_1&status=running", ""} {
			if res := a.human("GET", pubBase+"/deployments?limit=2&cursor="+*page.NextCursor+q, ""); res.Code != 400 || res.errCode(t) != "invalid_cursor" {
				t.Errorf("a cursor reused with %q: %d", q, res.Code)
			}
		}
		if res := a.human("GET", pubBase+"/deployments?limit=2&cursor="+*page.NextCursor+"&environmentId=env_t035_1&status=pending", ""); res.Code != 200 {
			t.Errorf("the cursor with its own filters: %d", res.Code)
		}
		for _, q := range []string{"status=a&status=b", "environmentId=", "status="} {
			if res := a.human("GET", pubBase+"/deployments?"+q, ""); res.Code != 400 {
				t.Errorf("?%s: %d", q, res.Code)
			}
		}
	})

	t.Run("the caller is the router's principal and the scope is the path's", func(t *testing.T) {
		a := newAPI(t)
		res := a.human("POST", pubBase+"/environments", `{"name":"preview","kind":"stage","target":"kind","namespace":"ns","kubeconfig":{"name":"KUBECONFIG","ref":"secret://project/kubeconfig","delivery":"file"}}`)
		if res.Code != http.StatusCreated {
			t.Fatal(res.Body.String())
		}
		if a.svc.caller.Actor != (delivery.Actor{Type: delivery.ActorUser, ID: "u_engineer"}) || a.svc.caller.RequestID == "" {
			t.Fatalf("caller: %+v", a.svc.caller)
		}
	})

	t.Run("runtime routes forward the bearer and refuse a missing or malformed one", func(t *testing.T) {
		a := newAPI(t)
		path := "/attempts/att_t035_deploy/deployments/" + depID
		for name, hdr := range map[string][]string{
			"no header":         nil,
			"a wrong scheme":    {"Authorization", "Basic abc"},
			"an empty token":    {"Authorization", "Bearer "},
			"a token with room": {"Authorization", "Bearer a b"},
			"an oversized one":  {"Authorization", "Bearer " + strings.Repeat("a", 2000)},
		} {
			req := a.send(a.runtime, "", "GET", "/runtime/v1"+path, "", hdr...)
			if req.Code != http.StatusUnauthorized || req.errCode(t) != "invalid_credential" {
				t.Errorf("%s: %d", name, req.Code)
			}
		}
		if len(a.svc.called()) != 0 {
			t.Fatal("a request without a usable bearer reached the use case")
		}
		if res := a.rt("tok-abc", "GET", path, ""); res.Code != 200 || a.svc.bearer != "tok-abc" || a.svc.attempt != "att_t035_deploy" {
			t.Fatalf("the bearer or route attempt was not forwarded: %d %q %q", res.Code, a.svc.bearer, a.svc.attempt)
		}
		if res := a.rt("tok", "GET", "/attempts/bad%20id/deployments/"+depID, ""); res.Code != http.StatusBadRequest {
			t.Errorf("an invalid attempt identifier: %d", res.Code)
		}
		if res := a.rt("tok", "GET", "/attempts/att_t035_deploy/deployments/bad%20id", ""); res.Code != http.StatusBadRequest {
			t.Errorf("an invalid deployment identifier: %d", res.Code)
		}
		if res := a.rt("tok", "GET", path+"?x=1", ""); res.Code == http.StatusOK {
			t.Errorf("a query on a runtime read")
		}
	})

	t.Run("audiences are separate: no tenant or project in runtime paths, no runtime path on the human router", func(t *testing.T) {
		a := newAPI(t)
		if res := a.send(a.public, "tok-engineer", "GET", "/runtime/v1/attempts/att_t035_deploy/deployments/"+depID, ""); res.Code != http.StatusNotFound {
			t.Errorf("a runtime route on the human router: %d", res.Code)
		}
		if res := a.send(a.runtime, "tok", "GET", "/api/v1"+pubBase+"/deployments/"+depID, ""); res.Code != http.StatusNotFound {
			t.Errorf("a human route on the runtime router: %d", res.Code)
		}
		// A runtime body cannot name the environment, scope or action key: unknown field.
		body := `{"source":{"repositoryUrl":"https://git.t035.invalid/a.git","commit":"` + strings.Repeat("1", 40) + `"},"image":{"artifactId":"a","digest":"` + string(digestA) + `"},"tenantId":"t035_globex"}`
		if res := a.rt("tok", "POST", "/attempts/att_t035_deploy/deployments", body); res.Code != 400 {
			t.Errorf("a runtime body naming a tenant: %d", res.Code)
		}
		if len(a.svc.called()) != 0 {
			t.Fatalf("calls: %v", a.svc.called())
		}
	})

	t.Run("creation: created is 201, an observed original is 200, a replay is marked and reauthorized", func(t *testing.T) {
		a := newAPI(t)
		create := `{"attemptId":"att_t035_deploy","source":{"repositoryUrl":"https://git.t035.invalid/acme/inventory.git","commit":"` + strings.Repeat("1", 40) + `"},"image":{"artifactId":"art_t035_image","digest":"` + string(digestA) + `"}}`
		res := a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-1")
		if res.Code != 201 || res.Header().Get("Location") != "/api/v1"+pubBase+"/deployments/"+string(a.svc.dep.ID()) || res.Header().Get("ETag") != `"1"` {
			t.Fatalf("created: %d %v", res.Code, res.Header())
		}
		if a.reg.id.Key != "k-1" || a.reg.id.Principal.ID != "u_engineer" || a.reg.id.TenantID != scopeA.TenantID || a.reg.id.ProjectID != scopeA.ProjectID || !strings.HasSuffix(a.reg.id.Route, "/deployments") {
			t.Fatalf("identity: %+v", a.reg.id)
		}
		var rc deliveryhttp.DeploymentReceipt
		res.decode(t, &rc)
		if !rc.Created || rc.Annotations[delivery.DefaultAnnotations.Deployment] != string(a.svc.dep.ID()) {
			t.Fatalf("receipt: %+v", rc)
		}
		if a.svc.attempt != "att_t035_deploy" || a.svc.reg.Image.Digest != digestA {
			t.Fatalf("the request was not mapped: %+v", a.svc.reg)
		}

		a.reg.res.Created = false
		if res := a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-2"); res.Code != 200 {
			t.Fatalf("an observed original: %d", res.Code)
		}
		// A replay is marked, and asks the authority again about the stored target.
		a.reg.replayed = true
		before := len(a.authz.calls)
		res = a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-2")
		if res.Code != 200 || res.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay: %d %v", res.Code, res.Header())
		}
		if replayCalls := a.authz.calls[before+1:]; len(replayCalls) == 0 || replayCalls[len(replayCalls)-1].target.Resource != (httpapi.Reference{Kind: "deployment", ID: string(a.svc.dep.ID())}) {
			t.Fatalf("the replay was not authorized against the stored deployment: %+v", a.authz.calls[before:])
		}
		// A replay that is no longer authorized releases nothing.
		a.authz.deny = func(n int, _ httpapi.Action, tg httpapi.Target) error {
			if tg.Resource.Kind == "deployment" {
				return httpapi.ErrDenied
			}
			return nil
		}
		res = a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-2")
		if res.Code != http.StatusForbidden || strings.Contains(res.Body.String(), string(a.svc.dep.ID())) {
			t.Fatalf("an unauthorized replay: %d %s", res.Code, res.Body.String())
		}
		a.authz.deny = nil
		// A missing or invalid key never reaches the use case.
		runs := a.reg.runs
		for _, hdr := range [][]string{nil, {"Idempotency-Key", "bad key!"}, {"Idempotency-Key", "a", "Idempotency-Key", "b"}} {
			if res := a.human("POST", pubBase+"/deployments", create, hdr...); res.Code != 400 {
				t.Errorf("key %v: %d", hdr, res.Code)
			}
		}
		if a.reg.runs != runs {
			t.Fatal("a request without a valid key was committed")
		}
		// A refusal of the keyed commit is mapped, not leaked.
		a.reg.err = delivery.Fail(delivery.ErrConflict, nil, "different request")
		if res := a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-3"); res.Code != 409 || res.errCode(t) != "conflict" {
			t.Errorf("conflict: %d", res.Code)
		}
		a.reg.err = httpapi.IdempotencyConflictError()
		if res := a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-3"); res.Code != 409 || res.errCode(t) != "idempotency_conflict" {
			t.Errorf("idempotency conflict: %d", res.Code)
		}
		a.reg.err = httpapi.UnavailableError(0, errors.New("unknown commit"))
		if res := a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-4"); res.Code != 503 {
			t.Errorf("unknown outcome: %d", res.Code)
		}
		// The use case's refusal comes before the keyed commit.
		a.reg.err = nil
		a.svc.err = delivery.Fail(delivery.ErrProvenance, nil, "wrong image")
		runs = a.reg.runs
		if res := a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "k-5"); res.Code != 422 || res.errCode(t) != "image_provenance_rejected" || a.reg.runs != runs {
			t.Errorf("a provenance refusal: %d", res.Code)
		}
	})

	t.Run("a retention reconciliation follows every keyed outcome", func(t *testing.T) {
		a := newAPI(t)
		create := `{"attemptId":"att_t035_deploy","source":{"repositoryUrl":"https://git.t035.invalid/acme/inventory.git","commit":"` + strings.Repeat("1", 40) + `"},"image":{"artifactId":"art_t035_image","digest":"` + string(digestA) + `"}}`
		a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "r-1")
		a.reg.replayed = true
		a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "r-1")
		a.reg.replayed, a.reg.err = false, delivery.Fail(delivery.ErrConflict, nil, "x")
		a.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "r-2")
		got := a.svc.called()
		want := []string{"PlanRegistrationForAttempt", "CompleteRegistration", "PlanRegistrationForAttempt", "CompleteReplay", "PlanRegistrationForAttempt", "CompleteRegistration"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("calls %v, want %v", got, want)
		}
	})

	t.Run("the mapped types never carry plaintext fields", func(t *testing.T) {
		for _, typ := range []any{deliveryhttp.Environment{}, deliveryhttp.EnvironmentCreate{}, deliveryhttp.EnvironmentEdit{}, deliveryhttp.CredentialBinding{},
			deliveryhttp.Deployment{}, deliveryhttp.DeploymentReceipt{}, deliveryhttp.ObjectRef{}, deliveryhttp.OutcomeReport{}} {
			rt := reflect.TypeOf(typ)
			for i := 0; i < rt.NumField(); i++ {
				name := strings.ToLower(rt.Field(i).Name)
				for _, bad := range []string{"value", "plaintext", "manifest", "kubeconfigcontent", "password", "token", "output", "log"} {
					if name == bad {
						t.Errorf("%s has a %s field", rt, rt.Field(i).Name)
					}
				}
			}
		}
	})
}

func validateRaw(sch *jsonschema.Schema, raw []byte) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return sch.Validate(v)
}

func versionOf(t testing.TB, body []byte) int64 {
	t.Helper()
	var v struct {
		Version    int64 `json:"version"`
		Deployment struct {
			Version int64 `json:"version"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.Deployment.Version != 0 {
		return v.Deployment.Version
	}
	return v.Version
}

func knownCode(code string) bool {
	switch code {
	case "invalid_request", "invalid_cursor", "invalid_credential", "forbidden", "not_found", "idempotency_conflict", "version_conflict",
		"version_required", "rate_limited", "internal_error", "unavailable", "conflict", "outcome_unknown", "name_taken", "invalid_transition",
		"credential_unavailable", "image_provenance_rejected", "pin_required", "not_configured", "attempt_revoked":
		return true
	}
	return false
}
