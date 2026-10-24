package artifactshttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	artifactshttp "github.com/optimiware/kodama/internal/artifacts/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// fakeRuntime is a programmable RuntimeService. It stands in for the module so
// the wire contract is verified without any external service.
type fakeRuntime struct {
	mu       sync.Mutex
	reserve  func(artifacts.Caller, domain.AttemptID, artifacts.ReserveInput) (artifacts.ReserveOutput, error)
	complete func(artifacts.Caller, domain.AttemptID, domain.ArtifactID, artifacts.CompleteInput) (artifacts.Completion, error)
	calls    int
	last     struct {
		caller  artifacts.Caller
		attempt domain.AttemptID
		in      artifacts.ReserveInput
		reqID   string
	}
}

func (f *fakeRuntime) Reserve(_ context.Context, c artifacts.Caller, a domain.AttemptID, in artifacts.ReserveInput, reqID string) (artifacts.ReserveOutput, error) {
	f.mu.Lock()
	f.calls++
	f.last.caller, f.last.attempt, f.last.in, f.last.reqID = c, a, in, reqID
	h := f.reserve
	f.mu.Unlock()
	return h(c, a, in)
}

func (f *fakeRuntime) Complete(_ context.Context, c artifacts.Caller, a domain.AttemptID, id domain.ArtifactID, in artifacts.CompleteInput, _ string) (artifacts.Completion, error) {
	f.mu.Lock()
	f.calls++
	h := f.complete
	f.mu.Unlock()
	return h(c, a, id, in)
}

func (f *fakeRuntime) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type authzRecorder struct {
	mu   sync.Mutex
	seen []httpapi.Target
	acts []httpapi.Action
	deny error
}

func (a *authzRecorder) Authorize(_ context.Context, _ httpapi.Principal, act httpapi.Action, t httpapi.Target) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, t)
	a.acts = append(a.acts, act)
	return a.deny
}

func runtimeServer(t *testing.T, svc artifactshttp.RuntimeService, authz httpapi.Authorizer) *httptest.Server {
	t.Helper()
	if authz == nil {
		authz = allowAll{}
	}
	r := router(t, "/runtime/v1", authz)
	h, err := artifactshttp.NewRuntime(svc, artifactshttp.DefaultActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(r); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

const (
	good  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fileJ = `{"name":"tests.json","kind":"file","role":"output","digest":"` + good + `","mediaType":"application/json","sizeBytes":2048,"registry":null}`
	ociJ  = `{"name":"image","kind":"oci","role":"output","digest":"` + good + `","mediaType":null,"sizeBytes":null,"registry":"registry.example.test/team/app"}`
)

func okReserve(created bool, withUpload bool) func(artifacts.Caller, domain.AttemptID, artifacts.ReserveInput) (artifacts.ReserveOutput, error) {
	return func(artifacts.Caller, domain.AttemptID, artifacts.ReserveInput) (artifacts.ReserveOutput, error) {
		out := artifacts.ReserveOutput{ID: "art_tests", State: artifacts.StateRegistered, Created: created}
		if withUpload {
			out.State = artifacts.StateReserved
			out.Upload = &artifacts.Transfer{Method: "PUT", URL: "https://s3.example.invalid/bucket/key?sig=x", Headers: nil, ExpiresAt: time.Date(2026, 10, 19, 10, 15, 0, 0, time.UTC)}
		}
		return out, nil
	}
}

// TestRuntimeContractCompatibility: strict decoding, explicit nulls, fixed
// response states and statuses, duplicate replay, revoked authority and the
// frozen runtime fixtures. No external services.
func TestRuntimeContractCompatibility(t *testing.T) {
	const path = "/runtime/v1/attempts/att_1/artifacts"

	t.Run("responses use the frozen statuses and shapes", func(t *testing.T) {
		svc := &fakeRuntime{}
		srv := runtimeServer(t, svc, nil)
		svc.reserve = okReserve(true, true)
		r := call(t, "POST", srv.URL+path, "tok", fileJ)
		if r.status != http.StatusCreated {
			t.Fatalf("new reservation: %d %s", r.status, r.raw)
		}
		resp, err := runtimev1.DecodeResponse[runtimev1.ArtifactReserveResponse](r.raw)
		if err != nil || resp.State != runtimev1.StateReserved || resp.Upload == nil || resp.Upload.Method != "PUT" || resp.Upload.Headers == nil {
			t.Fatalf("response: %+v %v (%s)", resp, err, r.raw)
		}
		if !strings.Contains(string(r.raw), `"headers":{}`) || resp.Upload.ExpiresAt != "2026-10-19T10:15:00Z" {
			t.Fatalf("headers must be an explicit object and the expiry UTC: %s", r.raw)
		}
		if r.header.Get("Cache-Control") != "no-store" || r.header.Get("X-Request-ID") == "" || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
			t.Fatalf("headers: %v", r.header)
		}

		svc.reserve = okReserve(false, true)
		if r := call(t, "POST", srv.URL+path, "tok", fileJ); r.status != http.StatusOK {
			t.Fatalf("identical repeat: %d %s", r.status, r.raw)
		}

		svc.reserve = okReserve(true, false)
		r = call(t, "POST", srv.URL+path, "tok", ociJ)
		resp, err = runtimev1.DecodeResponse[runtimev1.ArtifactReserveResponse](r.raw)
		if r.status != http.StatusCreated || err != nil || resp.State != runtimev1.StateRegistered || resp.Upload != nil || !strings.Contains(string(r.raw), `"upload":null`) {
			t.Fatalf("OCI registration: %d %s %v", r.status, r.raw, err)
		}

		svc.complete = func(artifacts.Caller, domain.AttemptID, domain.ArtifactID, artifacts.CompleteInput) (artifacts.Completion, error) {
			return artifacts.Completion{ID: "art_tests", State: artifacts.StateUsable, Digest: good, Size: 2048}, nil
		}
		r = call(t, "POST", srv.URL+path+"/art_tests/complete", "tok", `{"digest":"`+good+`","sizeBytes":2048}`)
		done, err := runtimev1.DecodeResponse[runtimev1.ArtifactCompleteResponse](r.raw)
		if r.status != http.StatusOK || err != nil || done.State != runtimev1.StateUsable || done.SizeBytes != 2048 {
			t.Fatalf("completion: %d %s %v", r.status, r.raw, err)
		}
	})

	t.Run("a module result that would violate the contract is never delivered", func(t *testing.T) {
		svc := &fakeRuntime{reserve: func(artifacts.Caller, domain.AttemptID, artifacts.ReserveInput) (artifacts.ReserveOutput, error) {
			// A file reservation without an instruction.
			return artifacts.ReserveOutput{ID: "art_x", State: artifacts.StateReserved, Created: true}, nil
		}}
		srv := runtimeServer(t, svc, nil)
		if r := call(t, "POST", srv.URL+path, "tok", fileJ); r.status != http.StatusInternalServerError || r.errorCode(t) != "internal_error" {
			t.Fatalf("%d %s", r.status, r.raw)
		}
		svc.reserve = func(artifacts.Caller, domain.AttemptID, artifacts.ReserveInput) (artifacts.ReserveOutput, error) {
			return artifacts.ReserveOutput{ID: "art_x", State: artifacts.StateReserved, Created: true, Upload: &artifacts.Transfer{Method: "POST", URL: "https://x.example/y", ExpiresAt: time.Now()}}, nil
		}
		if r := call(t, "POST", srv.URL+path, "tok", fileJ); r.status != http.StatusInternalServerError {
			t.Fatalf("a non-PUT instruction: %d %s", r.status, r.raw)
		}
	})

	t.Run("strict decoding rejects what the contract rejects", func(t *testing.T) {
		svc := &fakeRuntime{reserve: okReserve(true, true)}
		srv := runtimeServer(t, svc, nil)
		for name, body := range map[string]string{
			"unknown field":            strings.Replace(fileJ, `"registry":null`, `"registry":null,"tenantId":"t_other"`, 1),
			"scope in the body":        strings.Replace(fileJ, `"name"`, `"projectId":"p_x","name"`, 1),
			"miscased field":           strings.Replace(fileJ, `"name"`, `"Name"`, 1),
			"duplicate field":          strings.Replace(fileJ, `"name":"tests.json"`, `"name":"a","name":"tests.json"`, 1),
			"omitted nullable":         strings.Replace(fileJ, `,"registry":null`, ``, 1),
			"omitted media type":       strings.Replace(ociJ, `"mediaType":null,`, ``, 1),
			"file without size":        strings.Replace(fileJ, `"sizeBytes":2048`, `"sizeBytes":null`, 1),
			"negative size":            strings.Replace(fileJ, `2048`, `-1`, 1),
			"fractional size":          strings.Replace(fileJ, `2048`, `2048.5`, 1),
			"uppercase digest":         strings.Replace(fileJ, good, "sha256:"+strings.Repeat("A", 64), 1),
			"short digest":             strings.Replace(fileJ, good, "sha256:abc", 1),
			"oci with a size":          strings.Replace(ociJ, `"sizeBytes":null`, `"sizeBytes":1`, 1),
			"oci diagnostic":           strings.Replace(ociJ, `"role":"output"`, `"role":"diagnostic"`, 1),
			"unknown role":             strings.Replace(fileJ, `"output"`, `"secret"`, 1),
			"unknown kind":             strings.Replace(fileJ, `"file"`, `"directory"`, 1),
			"path traversal in name":   strings.Replace(fileJ, `tests.json`, `../x`, 1),
			"trailing document":        fileJ + `{}`,
			"not an object":            `[]`,
			"empty":                    ``,
			"nan":                      strings.Replace(fileJ, `2048`, `NaN`, 1),
			"credentials in registry":  strings.Replace(ociJ, `registry.example.test/team/app`, `https://u:p@registry.example.test/x`, 1),
			"integer beyond the range": strings.Replace(fileJ, `2048`, `9007199254740993`, 1),
		} {
			before := svc.count()
			r := call(t, "POST", srv.URL+path, "tok", body)
			if r.status != http.StatusBadRequest || r.errorCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
			if svc.count() != before {
				t.Errorf("%s: the service was called", name)
			}
		}
		if r := call(t, "POST", srv.URL+path+"/art_1/complete", "tok", `{"digest":"`+good+`"}`); r.status != http.StatusBadRequest {
			t.Errorf("completion without a size: %d", r.status)
		}
		// Explicit nulls are accepted exactly where the contract allows them.
		if r := call(t, "POST", srv.URL+path, "tok", fileJ); r.status != http.StatusCreated {
			t.Errorf("a valid request: %d %s", r.status, r.raw)
		}
		// Media type and size limits of the transport.
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(fileJ))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("a non-JSON content type: %d", resp.StatusCode)
		}
		if r := call(t, "POST", srv.URL+path, "tok", strings.Repeat(" ", 2<<20)+fileJ); r.status != http.StatusRequestEntityTooLarge {
			t.Errorf("an oversized body: %d", r.status)
		}
	})

	t.Run("the request reaches the module with the credential's identity and the path's attempt only", func(t *testing.T) {
		svc := &fakeRuntime{reserve: okReserve(true, true)}
		az := &authzRecorder{}
		srv := runtimeServer(t, svc, az)
		if r := call(t, "POST", srv.URL+"/runtime/v1/attempts/att_77/artifacts", "attempt:att_77", fileJ); r.status != http.StatusCreated {
			t.Fatal(string(r.raw))
		}
		if svc.last.caller.Subject != "attempt:att_77" || svc.last.attempt != "att_77" || svc.last.in.Name != "tests.json" || !strings.HasPrefix(svc.last.reqID, "req_") {
			t.Fatalf("module input: %+v", svc.last)
		}
		if svc.last.in.MediaType == nil || *svc.last.in.MediaType != "application/json" || svc.last.in.Size == nil || *svc.last.in.Size != 2048 || svc.last.in.Registry != nil {
			t.Fatalf("explicit null/typed fields were not preserved: %+v", svc.last.in)
		}
		// The router target has no tenant or project: scope is not taken from the generic target.
		if len(az.seen) != 1 || az.seen[0].TenantID != "" || az.seen[0].ProjectID != "" || az.seen[0].Resource != (httpapi.Reference{Kind: "attempt", ID: "att_77"}) || az.acts[0] != "artifact.reserve" {
			t.Fatalf("authorization target: %+v %v", az.seen, az.acts)
		}
	})

	t.Run("revoked authority, duplicate replay and error mapping", func(t *testing.T) {
		svc := &fakeRuntime{}
		srv := runtimeServer(t, svc, nil)
		table := []struct {
			err    error
			status int
			code   string
		}{
			{artifacts.ErrAttemptInactive, 403, "attempt_revoked"},
			{artifacts.ErrAuthorityMissing, 401, "invalid_credential"},
			{artifacts.ErrNotFound, 404, "not_found"},
			{artifacts.ErrIdempotencyConflict, 409, "idempotency_conflict"},
			{artifacts.ErrDigestMismatch, 422, "digest_mismatch"},
			{artifacts.ErrIncomplete, 409, "artifact_incomplete"},
			{artifacts.ErrOutputInvalid, 422, "output_invalid"},
			{artifacts.ErrLimitExceeded, 422, "output_invalid"},
			{artifacts.ErrRegistryDenied, 422, "output_invalid"},
			{artifacts.ErrProvenanceMissing, 422, "output_invalid"},
			{artifacts.ErrRegistryContent, 422, "output_invalid"},
			{artifacts.ErrInvalid, 400, "invalid_request"},
			{artifacts.ErrUnavailable, 503, "unavailable"},
			{artifacts.ErrInProgress, 503, "unavailable"},
			{artifacts.ErrRegistryUnavailable, 503, "unavailable"},
			{errors.New("pq: password authentication failed for kodama_runtime at 10.0.0.5"), 500, "internal_error"},
		}
		for _, tc := range table {
			err := fmt.Errorf("%w: secret detail s3://bucket/artifacts/t_acme/key", tc.err)
			svc.reserve = func(artifacts.Caller, domain.AttemptID, artifacts.ReserveInput) (artifacts.ReserveOutput, error) {
				return artifacts.ReserveOutput{}, err
			}
			svc.complete = func(artifacts.Caller, domain.AttemptID, domain.ArtifactID, artifacts.CompleteInput) (artifacts.Completion, error) {
				return artifacts.Completion{}, err
			}
			for _, r := range []response{
				call(t, "POST", srv.URL+path, "tok", fileJ),
				call(t, "POST", srv.URL+path+"/art_1/complete", "tok", `{"digest":"`+good+`","sizeBytes":1}`),
			} {
				want, known := runtimev1.Status(domain.ErrorCode(tc.code))
				if known && want != tc.status {
					t.Fatalf("the test table disagrees with the frozen status of %s", tc.code)
				}
				if r.status != tc.status || r.errorCode(t) != tc.code {
					t.Errorf("%v: %d %s", tc.err, r.status, r.raw)
				}
				if strings.Contains(string(r.raw), "secret detail") || strings.Contains(string(r.raw), "bucket") || strings.Contains(string(r.raw), "password") {
					t.Errorf("%v: the body discloses internals: %s", tc.err, r.raw)
				}
				var env runtimev1.ErrorResponse
				r.json(t, &env)
				if env.Error.Details == nil || !strings.HasPrefix(env.Error.RequestID, "req_") {
					t.Errorf("%v: the error envelope is incomplete: %s", tc.err, r.raw)
				}
				if tc.status == 503 && r.header.Get("Retry-After") == "" {
					t.Errorf("%v: unavailable without Retry-After", tc.err)
				}
			}
		}
	})

	t.Run("only the two runtime routes exist", func(t *testing.T) {
		svc := &fakeRuntime{reserve: okReserve(true, true)}
		srv := runtimeServer(t, svc, nil)
		for _, tc := range []struct{ method, path string }{
			{"GET", path}, {"PUT", path}, {"DELETE", path + "/art_1"}, {"GET", path + "/art_1/complete"}, {"POST", path + "/art_1"},
			{"POST", "/runtime/v1/artifacts"}, {"POST", "/runtime/v1/attempts/att_1/uploads"},
		} {
			r := call(t, tc.method, srv.URL+tc.path, "tok", nil)
			if r.status != 404 && r.status != 405 {
				t.Errorf("%s %s: %d", tc.method, tc.path, r.status)
			}
		}
		if r := call(t, "POST", srv.URL+path, "", fileJ); r.status != http.StatusUnauthorized {
			t.Errorf("anonymous: %d", r.status)
		}
		az := &authzRecorder{deny: httpapi.ErrDenied}
		srv2 := runtimeServer(t, svc, az)
		before := svc.count()
		if r := call(t, "POST", srv2.URL+path, "tok", fileJ); r.status != http.StatusForbidden || svc.count() != before {
			t.Errorf("a refused route reached the module: %d", r.status)
		}
		if _, err := artifactshttp.NewRuntime(nil, artifactshttp.DefaultActions()); err == nil {
			t.Error("a runtime handler without a service")
		}
		if _, err := artifactshttp.NewRuntime(svc, artifactshttp.Actions{}); err == nil {
			t.Error("a runtime handler without actions")
		}
	})

	t.Run("frozen fixtures", func(t *testing.T) {
		raw, err := os.ReadFile("../../../contracts/runtime/v1/fixtures/cases.json")
		if err != nil {
			t.Fatal(err)
		}
		var cases []struct {
			Name    string          `json:"name"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
			Valid   bool            `json:"valid"`
			Code    *string         `json:"code"`
		}
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatal(err)
		}
		svc := &fakeRuntime{reserve: okReserve(true, true)}
		svc.complete = func(artifacts.Caller, domain.AttemptID, domain.ArtifactID, artifacts.CompleteInput) (artifacts.Completion, error) {
			return artifacts.Completion{ID: "art_tests", State: artifacts.StateUsable, Digest: good, Size: 2048}, nil
		}
		srv := runtimeServer(t, svc, nil)
		n := 0
		for _, c := range cases {
			var target string
			switch c.Type {
			case "ArtifactReserveRequest":
				target = srv.URL + path
			case "ArtifactCompleteRequest":
				target = srv.URL + path + "/art_tests/complete"
			default:
				continue
			}
			n++
			r := call(t, "POST", target, "tok", []byte(c.Payload))
			if c.Valid && r.status != 200 && r.status != 201 {
				t.Errorf("%s: a valid fixture was refused: %d %s", c.Name, r.status, r.raw)
			}
			if !c.Valid && (r.status != 400 || r.errorCode(t) != "invalid_request") {
				t.Errorf("%s: an invalid fixture was accepted: %d %s", c.Name, r.status, r.raw)
			}
		}
		if n < 7 {
			t.Fatalf("only %d artifact fixtures exercised", n)
		}
	})

	t.Run("frozen scenarios", func(t *testing.T) {
		raw, err := os.ReadFile("../../../contracts/runtime/v1/fixtures/scenarios.json")
		if err != nil {
			t.Fatal(err)
		}
		var scenarios []struct {
			Name  string `json:"name"`
			Steps []struct {
				Op      string          `json:"op"`
				Request json.RawMessage `json:"request"`
				Status  int             `json:"status"`
				Body    json.RawMessage `json:"body"`
				ID      string          `json:"artifactId"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(raw, &scenarios); err != nil {
			t.Fatal(err)
		}
		byCode := map[string]error{
			"not_found": artifacts.ErrNotFound, "artifact_incomplete": artifacts.ErrIncomplete, "digest_mismatch": artifacts.ErrDigestMismatch,
			"output_invalid": artifacts.ErrOutputInvalid, "idempotency_conflict": artifacts.ErrIdempotencyConflict, "attempt_revoked": artifacts.ErrAttemptInactive,
		}
		checked := 0
		for _, sc := range scenarios {
			for i, st := range sc.Steps {
				if st.Op != "reserveArtifact" && st.Op != "completeArtifact" {
					continue
				}
				var env struct {
					Error *struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				_ = json.Unmarshal(st.Body, &env)
				svc := &fakeRuntime{}
				var failure error
				if env.Error != nil {
					var ok bool
					if failure, ok = byCode[env.Error.Code]; !ok {
						continue // a code owned by another route (result, source)
					}
				}
				svc.reserve = func(artifacts.Caller, domain.AttemptID, artifacts.ReserveInput) (artifacts.ReserveOutput, error) {
					if failure != nil {
						return artifacts.ReserveOutput{}, failure
					}
					return artifacts.ReserveOutput{ID: "art_tests", State: artifacts.StateReserved, Created: st.Status == 201,
						Upload: &artifacts.Transfer{Method: "PUT", URL: "https://s3.example.invalid/bucket/key?sig=x", ExpiresAt: time.Date(2026, 10, 19, 10, 15, 0, 0, time.UTC)}}, nil
				}
				svc.complete = func(artifacts.Caller, domain.AttemptID, domain.ArtifactID, artifacts.CompleteInput) (artifacts.Completion, error) {
					if failure != nil {
						return artifacts.Completion{}, failure
					}
					return artifacts.Completion{ID: "art_tests", State: artifacts.StateUsable, Digest: good, Size: 2048}, nil
				}
				srv := runtimeServer(t, svc, nil)
				target := srv.URL + path
				if st.Op == "completeArtifact" {
					id := st.ID
					if id == "" {
						id = "art_tests"
					}
					target += "/" + id + "/complete"
				}
				r := call(t, "POST", target, "tok", []byte(st.Request))
				if r.status != st.Status {
					t.Errorf("%s step %d (%s): status %d, fixture %d (%s)", sc.Name, i, st.Op, r.status, st.Status, r.raw)
				}
				if env.Error != nil && r.errorCode(t) != env.Error.Code {
					t.Errorf("%s step %d: code %s, fixture %s", sc.Name, i, r.errorCode(t), env.Error.Code)
				}
				checked++
			}
		}
		if checked < 4 {
			t.Fatalf("only %d scenario steps replayed", checked)
		}
	})
}
