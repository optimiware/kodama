package runtimehttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
)

const (
	secretCredential = "adm_SECRETCREDENTIALVALUE0123456789abcdefg"
	secretNonce      = "SECRETNONCEVALUE012345"
	secretToken      = "eyJSECRET.TOKENVALUE.sig0123456789abcdef"
	secretCause      = "SECRET-PROVIDER-PAYLOAD"
)

type fake struct {
	mu       sync.Mutex
	admitErr error
	admit    runtimev1.AdmitResponse
	calls    []string
	gotAdmit runtimev1.AdmitRequest
	gotRoute domain.AttemptID
	gotToken string
}

func (f *fake) Admit(_ context.Context, route domain.AttemptID, req runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.gotAdmit, f.gotRoute = append(f.calls, "admit"), req, route
	return f.admit, f.admitErr
}

func (f *fake) Renew(_ context.Context, route domain.AttemptID, bearer string) (runtimev1.TokenResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.gotRoute, f.gotToken = append(f.calls, "renew"), route, bearer
	return runtimev1.TokenResponse{Token: f.admit.Token}, f.admitErr
}

func (f *fake) Context(_ context.Context, route domain.AttemptID, bearer string) (runtimev1.Context, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.gotRoute, f.gotToken = append(f.calls, "context"), route, bearer
	return f.admit.Context, f.admitErr
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func frozenAdmit(t *testing.T) (runtimev1.AdmitRequest, runtimev1.AdmitResponse, []byte) {
	t.Helper()
	data, err := readFixture()
	if err != nil {
		t.Fatal(err)
	}
	var all []struct {
		Name  string `json:"name"`
		Steps []struct {
			Request json.RawMessage `json:"request"`
			Body    json.RawMessage `json:"body"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.Name == "happy_path" {
			req, err := runtimev1.DecodeRequest[runtimev1.AdmitRequest](s.Steps[0].Request)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := runtimev1.DecodeResponse[runtimev1.AdmitResponse](s.Steps[0].Body)
			if err != nil {
				t.Fatal(err)
			}
			return req, resp, s.Steps[0].Request
		}
	}
	t.Fatal("no happy_path")
	return runtimev1.AdmitRequest{}, runtimev1.AdmitResponse{}, nil
}

func setup(t *testing.T) (http.Handler, *fake, *logBuf) {
	t.Helper()
	logs := &logBuf{}
	f := &fake{}
	_, f.admit, _ = frozenAdmit(t)
	h, err := runtimehttp.NewHandler(runtimehttp.Config{Service: f, Logger: newLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	return h, f, logs
}

func do(h http.Handler, method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) runtimev1.ErrorBody {
	t.Helper()
	e, err := runtimev1.DecodeResponse[runtimev1.ErrorResponse](rec.Body.Bytes())
	if err != nil {
		t.Fatalf("not a contract error envelope (%d): %s", rec.Code, rec.Body)
	}
	if want, ok := runtimev1.Status(e.Error.Code); !ok || want != rec.Code {
		t.Fatalf("code %s is not served with status %d", e.Error.Code, rec.Code)
	}
	if rec.Header().Get("X-Request-ID") != e.Error.RequestID {
		t.Fatal("the request ID header and body differ")
	}
	return e.Error
}

const base = "/runtime/v1/attempts/att_01"

// Test_http_contract_and_safe_logging: strict decoding, explicit response
// fields, bounded bodies, contracted errors, and no sensitive value in a
// response, error or log.
func Test_http_contract_and_safe_logging(t *testing.T) {
	req, _, raw := frozenAdmit(t)

	t.Run("admit success is a valid explicit-null contract response", func(t *testing.T) {
		h, f, _ := setup(t)
		rec := do(h, "POST", base+"/admit", raw, nil)
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		got, err := runtimev1.DecodeResponse[runtimev1.AdmitResponse](rec.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if got.Token.Audience != "kodama-runtime" || got.Token.TokenType != "Bearer" {
			t.Fatalf("token = %+v", got.Token)
		}
		for _, field := range []string{`"designBaseline":null`, `"profile":null`, `"environment":null`, `"branch":null`, `"expectedTip":null`} {
			if !strings.Contains(rec.Body.String(), field) {
				t.Errorf("missing explicit field %s", field)
			}
		}
		if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("headers = %v", rec.Header())
		}
		if f.gotRoute != "att_01" || f.gotAdmit != req {
			t.Fatalf("the service received %+v on %s", f.gotAdmit, f.gotRoute)
		}
	})

	t.Run("strict decoding", func(t *testing.T) {
		h, f, _ := setup(t)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		with := func(mutate func(map[string]any)) []byte {
			var c map[string]any
			_ = json.Unmarshal(raw, &c)
			mutate(c)
			b, _ := json.Marshal(c)
			return b
		}
		bodies := map[string][]byte{
			"unknown field":     with(func(c map[string]any) { c["extra"] = 1 }),
			"missing field":     with(func(c map[string]any) { delete(c, "nonce") }),
			"differently cased": with(func(c map[string]any) { c["Nonce"] = c["nonce"]; delete(c, "nonce") }),
			"bad nonce":         with(func(c map[string]any) { c["nonce"] = "short" }),
			"bad job uid":       with(func(c map[string]any) { c["job"].(map[string]any)["uid"] = "nope" }),
			"nested unknown":    with(func(c map[string]any) { c["job"].(map[string]any)["x"] = 1 }),
			"duplicate keys":    []byte(strings.Replace(string(raw), `"nonce"`, `"nonce":"nnnnnnnnnnnnnnnnnnnnnn","nonce"`, 1)),
			"trailing data":     append(append([]byte{}, raw...), []byte(` {}`)...),
			"not an object":     []byte(`[]`),
			"empty":             {},
			"float":             []byte(`{"admissionCredential": 1.5}`),
		}
		for name, body := range bodies {
			rec := do(h, "POST", base+"/admit", body, nil)
			if rec.Code != 400 || errorOf(t, rec).Code != domain.CodeInvalidRequest {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			}
		}
		if len(f.calls) != 0 {
			t.Fatalf("an invalid request reached the service: %v", f.calls)
		}
		rec := do(h, "POST", base+"/admit", raw, map[string]string{"Content-Type": "text/plain"})
		if rec.Code != 400 {
			t.Errorf("wrong content type: %d", rec.Code)
		}
	})

	t.Run("bounded bodies", func(t *testing.T) {
		h, f, _ := setup(t)
		huge := append([]byte(`{"pad":"`), bytes.Repeat([]byte("a"), runtimehttp.MaxBodyBytes+10)...)
		huge = append(huge, []byte(`"}`)...)
		for path, body := range map[string][]byte{"/admit": huge, "/token": huge} {
			rec := do(h, "POST", base+path, body, map[string]string{"Authorization": "Bearer " + secretToken})
			if rec.Code != 400 || errorOf(t, rec).Code != domain.CodeInvalidRequest {
				t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
			}
		}
		if len(f.calls) != 0 {
			t.Fatal("an oversized body reached the service")
		}
	})

	t.Run("the attempt in the path is validated", func(t *testing.T) {
		h, f, _ := setup(t)
		rec := do(h, "POST", "/runtime/v1/attempts/bad%20id/admit", raw, nil)
		if rec.Code != 400 || errorOf(t, rec).Code != domain.CodeInvalidRequest {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		// On routes whose frozen inventory has no invalid_request it degrades to 401.
		rec = do(h, "GET", "/runtime/v1/attempts/bad%20id/context", nil, map[string]string{"Authorization": "Bearer " + secretToken})
		if rec.Code != 401 || errorOf(t, rec).Code != domain.CodeInvalidCredential {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if len(f.calls) != 0 {
			t.Fatalf("calls = %v", f.calls)
		}
	})

	t.Run("bearer handling", func(t *testing.T) {
		h, f, _ := setup(t)
		bad := map[string]map[string]string{
			"missing":      nil,
			"wrong scheme": {"Authorization": "Basic abc"},
			"no token":     {"Authorization": "Bearer "},
			"extra part":   {"Authorization": "Bearer a b"},
			"oversized":    {"Authorization": "Bearer " + strings.Repeat("a", 2000)},
			"token only":   {"Authorization": secretToken},
		}
		for name, hdr := range bad {
			for _, path := range []struct{ method, p string }{{"GET", "/context"}, {"POST", "/token"}} {
				body := []byte(nil)
				if path.method == "POST" {
					body = []byte(`{}`)
				}
				rec := do(h, path.method, base+path.p, body, hdr)
				if rec.Code != 401 || errorOf(t, rec).Code != domain.CodeInvalidCredential || rec.Header().Get("WWW-Authenticate") == "" {
					t.Errorf("%s %s: %d %s", name, path.p, rec.Code, rec.Body)
				}
			}
		}
		if len(f.calls) != 0 {
			t.Fatalf("an unauthenticated request reached the service: %v", f.calls)
		}
		rec := do(h, "GET", base+"/context", nil, map[string]string{"Authorization": "bearer " + secretToken})
		if rec.Code != 200 || f.gotToken != secretToken {
			t.Fatalf("a valid bearer: %d token=%q", rec.Code, f.gotToken)
		}
		if rec := do(h, "POST", base+"/token", []byte(`{"x":1}`), map[string]string{"Authorization": "Bearer " + secretToken}); rec.Code != 400 {
			t.Errorf("TokenRequest must be an empty object: %d", rec.Code)
		}
		if rec := do(h, "POST", base+"/token", []byte(`{}`), map[string]string{"Authorization": "Bearer " + secretToken}); rec.Code != 200 {
			t.Errorf("renewal: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("error mapping follows the frozen inventories", func(t *testing.T) {
		cases := []struct {
			err   error
			admit [2]any // status, code
			ctx   [2]any
		}{
			{runtimeauth.ErrInvalidCredential, [2]any{401, domain.CodeInvalidCredential}, [2]any{401, domain.CodeInvalidCredential}},
			{runtimeauth.ErrNotFound, [2]any{401, domain.CodeInvalidCredential}, [2]any{401, domain.CodeInvalidCredential}},
			{runtimeauth.ErrForbidden, [2]any{403, domain.CodeForbidden}, [2]any{401, domain.CodeInvalidCredential}},
			{runtimeauth.ErrAttemptRevoked, [2]any{403, runtimev1.CodeAttemptRevoked}, [2]any{403, runtimev1.CodeAttemptRevoked}},
			{runtimeauth.ErrAdmissionConsumed, [2]any{409, runtimev1.CodeAdmissionConsumed}, [2]any{503, domain.CodeUnavailable}},
			{runtimeauth.ErrProfileUnsupported, [2]any{422, runtimev1.CodeProfileUnsupported}, [2]any{503, domain.CodeUnavailable}},
			{runtimeauth.ErrInvalidRequest, [2]any{400, domain.CodeInvalidRequest}, [2]any{401, domain.CodeInvalidCredential}},
			{runtimeauth.ErrUnavailable, [2]any{503, domain.CodeUnavailable}, [2]any{503, domain.CodeUnavailable}},
			{runtimeauth.ErrConflict, [2]any{503, domain.CodeUnavailable}, [2]any{503, domain.CodeUnavailable}},
			{errors.New("unclassified " + secretCause), [2]any{503, domain.CodeUnavailable}, [2]any{503, domain.CodeUnavailable}},
		}
		for _, c := range cases {
			h, f, logs := setup(t)
			f.admitErr = errors.Join(c.err, errors.New(secretCause))
			rec := do(h, "POST", base+"/admit", raw, nil)
			e := errorOf(t, rec)
			if rec.Code != c.admit[0] || e.Code != c.admit[1] {
				t.Errorf("admit %v: %d %s", c.err, rec.Code, e.Code)
			}
			rec2 := do(h, "GET", base+"/context", nil, map[string]string{"Authorization": "Bearer " + secretToken})
			e2 := errorOf(t, rec2)
			if rec2.Code != c.ctx[0] || e2.Code != c.ctx[1] {
				t.Errorf("context %v: %d %s", c.err, rec2.Code, e2.Code)
			}
			for _, out := range []string{rec.Body.String(), rec2.Body.String(), logs.String()} {
				if strings.Contains(out, secretCause) {
					t.Errorf("the cause leaked for %v: %s", c.err, out)
				}
			}
			if e.Details == nil || len(e.Details) != 0 {
				t.Errorf("details must be an explicit empty object: %v", e.Details)
			}
			if rec.Code == 503 && rec.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After on 503")
			}
			if rec.Code == 401 && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("no WWW-Authenticate on 401")
			}
		}
		// Every error the handler can emit for each owned route is in its frozen inventory.
		for _, op := range runtimev1.Operations {
			if op.ID != "admit" && op.ID != "renewToken" && op.ID != "getContext" {
				continue
			}
			for _, c := range cases {
				h, f, _ := setup(t)
				f.admitErr = c.err
				var rec *httptest.ResponseRecorder
				switch op.ID {
				case "admit":
					rec = do(h, "POST", base+"/admit", raw, nil)
				case "renewToken":
					rec = do(h, "POST", base+"/token", []byte(`{}`), map[string]string{"Authorization": "Bearer " + secretToken})
				default:
					rec = do(h, "GET", base+"/context", nil, map[string]string{"Authorization": "Bearer " + secretToken})
				}
				code := errorOf(t, rec).Code
				found := false
				for _, allowed := range op.Errors {
					found = found || allowed == code
				}
				if !found {
					t.Errorf("%s answered %v with %s, outside its frozen inventory", op.ID, c.err, code)
				}
			}
		}
	})

	t.Run("safe logging", func(t *testing.T) {
		h, f, logs := setup(t)
		body, _ := json.Marshal(runtimev1.AdmitRequest{AdmissionCredential: secretCredential, Nonce: secretNonce, ClientVersion: "1.4.0", Job: req.Job})
		f.admitErr = nil
		rec := do(h, "POST", base+"/admit", body, nil)
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		do(h, "GET", base+"/context", nil, map[string]string{"Authorization": "Bearer " + secretToken})
		f.admitErr = runtimeauth.ErrAttemptRevoked
		do(h, "POST", base+"/admit", body, nil)
		do(h, "POST", base+"/token", []byte(`{}`), map[string]string{"Authorization": "Bearer " + secretToken})
		do(h, "POST", base+"/admit", []byte(`{"admissionCredential":"`+secretCredential+`","nonce":"`+secretNonce+`","bogus":1}`), nil)
		out := logs.String()
		if out == "" {
			t.Fatal("nothing was logged")
		}
		for _, secret := range []string{secretCredential, secretNonce, secretToken, "SECRETCREDENTIAL", "tokenValue", f.admit.Context.Command.Image, f.admit.Context.WorkItem.Spec, f.admit.Token.Token, req.Job.UID, req.Job.PodUID} {
			if strings.Contains(out, secret) {
				t.Errorf("the log contains %q:\n%s", secret, out)
			}
		}
		for _, want := range []string{"op=admit", "op=renewToken", "op=getContext", "attemptId=att_01", "code=attempt_revoked", "requestId=req_"} {
			if !strings.Contains(out, want) {
				t.Errorf("the log lacks %s", want)
			}
		}
	})

	t.Run("only the owned routes exist", func(t *testing.T) {
		h, _, _ := setup(t)
		for _, p := range []struct{ method, path string }{
			{"POST", "/secrets/resolve"}, {"POST", "/artifacts"}, {"POST", "/artifacts/art_1/complete"}, {"POST", "/source"}, {"POST", "/result"},
		} {
			rec := do(h, p.method, base+p.path, []byte(`{}`), map[string]string{"Authorization": "Bearer " + secretToken})
			if rec.Code != 404 || errorOf(t, rec).Code != domain.CodeNotFound {
				t.Errorf("%s: %d %s", p.path, rec.Code, rec.Body)
			}
		}
		if rec := do(h, "GET", "/healthz", nil, nil); rec.Code != 404 {
			t.Errorf("unexpected route: %d", rec.Code)
		}
	})

	t.Run("construction requires a service", func(t *testing.T) {
		if _, err := runtimehttp.NewHandler(runtimehttp.Config{}); err == nil {
			t.Fatal("a handler without a service was built")
		}
		s := &http.Server{}
		runtimehttp.ApplyServerLimits(s)
		if s.MaxHeaderBytes != runtimehttp.MaxHeaderBytes || s.ReadHeaderTimeout == 0 {
			t.Fatalf("limits = %d %v", s.MaxHeaderBytes, s.ReadHeaderTimeout)
		}
	})
}
