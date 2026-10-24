package artifactshttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	artifactshttp "github.com/optimiware/kodama/internal/artifacts/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// bearerAuth authenticates "Authorization: Bearer <subject>"; the subject is
// the opaque principal both authorities key on.
type bearerAuth struct{}

func (bearerAuth) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	h := r.Header.Get("Authorization")
	subject, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || subject == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: subject}, nil
}

// allowAll leaves authorization to the module's authorities, as the router does
// in front of a policy that permits the route; tests replace it to prove the
// router's own denial.
type allowAll struct{ deny error }

func (a allowAll) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return a.deny
}

type stack struct {
	env *artifactstest.Env
	rt  *httptest.Server // /runtime/v1
	pub *httptest.Server // /api/v1
}

func router(t *testing.T, prefix string, authz httpapi.Authorizer) *httpapi.Router {
	t.Helper()
	r, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: bearerAuth{}, Authorizer: authz, RateLimiter: httpapi.Unlimited(), Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newStack(t *testing.T, env *artifactstest.Env, authz httpapi.Authorizer) *stack {
	t.Helper()
	if authz == nil {
		authz = allowAll{}
	}
	rtr, pubr := router(t, "/runtime/v1", authz), router(t, "/api/v1", authz)
	rh, err := artifactshttp.NewRuntime(env.Service, artifactshttp.DefaultActions())
	if err != nil {
		t.Fatal(err)
	}
	ph, err := artifactshttp.NewPublic(env.Service, artifactshttp.DefaultActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rh.RegisterRoutes(rtr); err != nil {
		t.Fatal(err)
	}
	if err := ph.RegisterRoutes(pubr); err != nil {
		t.Fatal(err)
	}
	s := &stack{env: env, rt: httptest.NewServer(rtr), pub: httptest.NewServer(pubr)}
	t.Cleanup(s.rt.Close)
	t.Cleanup(s.pub.Close)
	return s
}

type response struct {
	status int
	header http.Header
	raw    []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.raw, v); err != nil {
		t.Fatalf("response is not the expected JSON (%d): %s", r.status, r.raw)
	}
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	var e runtimev1.ErrorResponse
	r.json(t, &e)
	return string(e.Error.Code)
}

func call(t *testing.T, method, url, subject string, body any) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		var raw []byte
		switch b := body.(type) {
		case []byte:
			raw = b
		case string:
			raw = []byte(b)
		default:
			var err error
			if raw, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if subject != "" {
		req.Header.Set("Authorization", "Bearer "+subject)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, raw: raw}
}

func (s *stack) reserve(t *testing.T, attempt domain.AttemptID, subject string, req any) response {
	return call(t, http.MethodPost, s.rt.URL+"/runtime/v1/attempts/"+string(attempt)+"/artifacts", subject, req)
}

func (s *stack) complete(t *testing.T, attempt domain.AttemptID, id domain.ArtifactID, subject string, req any) response {
	return call(t, http.MethodPost, s.rt.URL+"/runtime/v1/attempts/"+string(attempt)+"/artifacts/"+string(id)+"/complete", subject, req)
}

func (s *stack) get(t *testing.T, scope domain.Scope, id domain.ArtifactID, subject string, query string) response {
	u := s.pub.URL + "/api/v1/tenants/" + string(scope.TenantID) + "/projects/" + string(scope.ProjectID) + "/artifacts/" + string(id)
	if query != "" {
		u += "?" + query
	}
	return call(t, http.MethodGet, u, subject, nil)
}

func reserveBody(name string, role artifacts.Role, data []byte) runtimev1.ArtifactReserveRequest {
	mt, size := "text/plain", int64(len(data))
	return runtimev1.ArtifactReserveRequest{Name: name, Kind: runtimev1.KindFile, Role: runtimev1.ArtifactRole(role), Digest: runtimev1.Digest(artifacts.DigestOf(data)), MediaType: &mt, SizeBytes: &size}
}

func completeBody(data []byte) runtimev1.ArtifactCompleteRequest {
	return runtimev1.ArtifactCompleteRequest{Digest: runtimev1.Digest(artifacts.DigestOf(data)), SizeBytes: int64(len(data))}
}

// reserveOK reserves through HTTP and returns the ID and the upload instruction.
func (s *stack) reserveOK(t *testing.T, attempt domain.AttemptID, subject, name string, role artifacts.Role, data []byte) (domain.ArtifactID, runtimev1.Upload) {
	t.Helper()
	r := s.reserve(t, attempt, subject, reserveBody(name, role, data))
	if r.status != http.StatusCreated {
		t.Fatalf("reserve %s: %d %s", name, r.status, r.raw)
	}
	var out runtimev1.ArtifactReserveResponse
	r.json(t, &out)
	if out.Upload == nil {
		t.Fatal("no upload")
	}
	return out.ArtifactID, *out.Upload
}

func (s *stack) upload(t *testing.T, up runtimev1.Upload, data []byte) {
	t.Helper()
	code, body := s.env.Upload(artifacts.Transfer{Method: up.Method, URL: up.URL, Headers: up.Headers}, data)
	if code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
}
