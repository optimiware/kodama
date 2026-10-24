package resultshttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
	resultshttp "github.com/optimiware/kodama/internal/results/http"
)

type step struct {
	Op      string          `json:"op"`
	Request json.RawMessage `json:"request"`
	Status  int             `json:"status"`
	Body    json.RawMessage `json:"body"`
}

type scenario struct {
	Name  string `json:"name"`
	Steps []step `json:"steps"`
}

func scenarios(t *testing.T) []scenario {
	t.Helper()
	data, err := os.ReadFile("../../../contracts/runtime/v1/fixtures/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var all []scenario
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	return all
}

// replay is a Service that answers each call from the frozen scenario: a
// successful body is returned as the service result, an error body is turned
// into the service error class that must map back to the same status and code.
type replay struct {
	t       *testing.T
	step    step
	gotAtt  domain.AttemptID
	gotBear string
	calls   int
}

func (r *replay) Submit(_ context.Context, c results.SubmitCommand) (runtimev1.ResultResponse, error) {
	r.calls++
	r.gotAtt, r.gotBear = c.Attempt, c.Bearer
	if r.step.Status != http.StatusOK {
		return runtimev1.ResultResponse{}, r.err()
	}
	var resp runtimev1.ResultResponse
	if err := json.Unmarshal(r.step.Body, &resp); err != nil {
		r.t.Fatal(err)
	}
	return resp, nil
}

func (r *replay) ProposeSource(_ context.Context, c results.ProposeCommand) (results.SourceOutcome, error) {
	r.calls++
	r.gotAtt, r.gotBear = c.Attempt, c.Bearer
	if r.step.Status != 200 && r.step.Status != 202 {
		return results.SourceOutcome{}, r.err()
	}
	var resp runtimev1.SourceProposalResponse
	if err := json.Unmarshal(r.step.Body, &resp); err != nil {
		r.t.Fatal(err)
	}
	return results.SourceOutcome{HTTPStatus: r.step.Status, Response: resp}, nil
}

func (r *replay) err() error {
	var body runtimev1.ErrorResponse
	if err := json.Unmarshal(r.step.Body, &body); err != nil {
		r.t.Fatal(err)
	}
	switch body.Error.Code {
	case runtimev1.CodeAttemptRevoked:
		return results.Fail(results.ErrAttemptRevoked, nil, "x")
	case domain.CodeNotFound:
		return results.Fail(results.ErrNotFound, nil, "x")
	case runtimev1.CodeOutputInvalid:
		return results.Fail(results.ErrOutputInvalid, nil, "x")
	case runtimev1.CodeResultConflict:
		return results.Fail(results.ErrResultConflict, nil, "x")
	case runtimev1.CodePublicationConflict:
		tip, _ := body.Error.Details["observedTip"].(string)
		return &results.PublicationConflict{Reason: "tip_moved", ObservedTip: tip}
	}
	r.t.Fatalf("scenario error %s has no mapping in the test", body.Error.Code)
	return nil
}

func serve(t *testing.T, svc resultshttp.Service) (http.Handler, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	h, err := resultshttp.NewHandler(resultshttp.Config{Service: svc, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return h, logs
}

func do(h http.Handler, method, path string, body []byte, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// TestRuntimeContractFixtures replays the frozen runtime scenarios for the two
// routes this module owns (PX-10, PX-12, WS-04, WS-06, IT-06): duplicate and
// conflicting results, late result, foreign and incomplete artifacts, missing
// output, source conflict and response loss. Status, code and body must match
// the fixture exactly; behavior under real state is the integration tests'.
func TestRuntimeContractFixtures(t *testing.T) {
	routes := map[string]string{"submitResult": "result", "proposeSource": "source"}
	seen := map[string]bool{}
	for _, sc := range scenarios(t) {
		for i, st := range sc.Steps {
			suffix, ok := routes[st.Op]
			if !ok {
				continue
			}
			seen[sc.Name] = true
			t.Run(sc.Name+"/"+st.Op+"/"+string(rune('0'+i)), func(t *testing.T) {
				rp := &replay{t: t, step: st}
				h, logs := serve(t, rp)
				rec := do(h, http.MethodPost, "/runtime/v1/attempts/att_01/"+suffix, st.Request, "rt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				if rec.Code != st.Status {
					t.Fatalf("status %d, want %d: %s", rec.Code, st.Status, rec.Body)
				}
				if rp.gotAtt != "att_01" || rp.gotBear != "rt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
					t.Fatalf("the handler passed %q / bearer %q", rp.gotAtt, rp.gotBear)
				}
				if st.Status < 300 {
					if canonical(t, rec.Body.Bytes()) != canonical(t, st.Body) {
						t.Fatalf("body\n got %s\nwant %s", rec.Body, st.Body)
					}
				} else {
					var got, want runtimev1.ErrorResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(st.Body, &want); err != nil {
						t.Fatal(err)
					}
					if got.Error.Code != want.Error.Code || got.Error.RequestID == "" || got.Error.Details == nil {
						t.Fatalf("error %+v, want code %s", got.Error, want.Error.Code)
					}
					if tip, ok := want.Error.Details["observedTip"]; ok && got.Error.Details["observedTip"] != tip {
						t.Fatalf("details %v, want observedTip %v", got.Error.Details, tip)
					}
					if err := got.Validate(); err != nil {
						t.Fatalf("the error envelope violates the contract: %v", err)
					}
				}
				if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Request-ID") == "" {
					t.Fatalf("headers %v", rec.Header())
				}
				if strings.Contains(logs.String(), "rt_aaaa") {
					t.Fatal("a bearer token reached the log")
				}
			})
		}
	}
	for _, name := range []string{"duplicate_result", "conflicting_result", "late_result_after_revocation", "foreign_artifact_in_result", "missing_output", "publication_conflict", "publication_response_loss"} {
		if !seen[name] {
			t.Errorf("scenario %s was not replayed", name)
		}
	}
}

type fixedService struct{ err error }

func (f fixedService) Submit(context.Context, results.SubmitCommand) (runtimev1.ResultResponse, error) {
	return runtimev1.ResultResponse{}, f.err
}
func (f fixedService) ProposeSource(context.Context, results.ProposeCommand) (results.SourceOutcome, error) {
	return results.SourceOutcome{}, f.err
}

// goodResult is a valid failed-result request with its real canonical hash.
func goodResult(t *testing.T) string {
	t.Helper()
	code, reason := int64(1), "command_failed"
	res := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: &code, Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	h, err := runtimev1.ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	return `{"resultHash":"` + string(h) + `","result":{"outcome":"failed","exitCode":1,"reason":"command_failed","question":null,"sourceCommit":null,"outputs":{},"diagnosticArtifactIds":[]}}`
}

func TestHashMismatchIsRejectedBeforeTheService(t *testing.T) {
	var calls int
	h, _ := serve(t, &countingService{n: &calls})
	good := goodResult(t)
	i := strings.Index(good, "sha256:")
	body := good[:i] + "sha256:" + strings.Repeat("0", 64) + good[i+len("sha256:")+64:]
	rec := do(h, http.MethodPost, "/runtime/v1/attempts/att_01/result", []byte(body), "tok")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "result_hash_mismatch") || calls != 0 {
		t.Fatalf("%d %s (%d calls)", rec.Code, rec.Body, calls)
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   domain.ErrorCode
	}{
		{results.Fail(results.ErrUnauthenticated, nil, "x"), 401, domain.CodeInvalidCredential},
		{results.Fail(results.ErrForbidden, nil, "x"), 401, domain.CodeInvalidCredential}, // `forbidden` is not in these routes' inventory
		{results.Fail(results.ErrAttemptRevoked, nil, "x"), 403, runtimev1.CodeAttemptRevoked},
		{results.Fail(results.ErrNotFound, nil, "x"), 404, domain.CodeNotFound},
		{results.Fail(results.ErrArtifactIncomplete, nil, "x"), 409, runtimev1.CodeArtifactIncomplete},
		{results.Fail(results.ErrResultConflict, nil, "x"), 409, runtimev1.CodeResultConflict},
		{results.Fail(results.ErrOutputInvalid, nil, "x"), 422, runtimev1.CodeOutputInvalid},
		{results.Fail(results.ErrResultHashMismatch, nil, "x"), 422, runtimev1.CodeResultHashMismatch},
		{results.Fail(results.ErrInvalidRequest, nil, "x"), 400, domain.CodeInvalidRequest},
		{results.Fail(results.ErrUnavailable, nil, "x"), 503, domain.CodeUnavailable},
		{errors.New("driver: password=hunter2 at 10.0.0.7"), 503, domain.CodeUnavailable},
	}
	body := []byte(goodResult(t))
	for _, c := range cases {
		h, _ := serve(t, fixedService{err: c.err})
		rec := do(h, http.MethodPost, "/runtime/v1/attempts/att_01/result", body, "tok")
		var got runtimev1.ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != c.status || got.Error.Code != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.err, rec.Code, got.Error.Code, c.status, c.code)
		}
		if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "10.0.0.7") {
			t.Errorf("the dependency's error text reached the client: %s", rec.Body)
		}
		if c.status == 503 && rec.Header().Get("Retry-After") == "" {
			t.Error("unavailable without Retry-After")
		}
		if c.status == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("401 without WWW-Authenticate")
		}
	}
}

func TestStrictRequestsAndBounds(t *testing.T) {
	var calls int
	svc := &countingService{n: &calls}
	h, _ := serve(t, svc)
	good := goodResult(t)
	bad := map[string]string{
		"unknown field":  strings.Replace(good, `"resultHash"`, `"extra":1,"resultHash"`, 1),
		"omitted null":   strings.Replace(good, `"question":null,`, ``, 1),
		"duplicate key":  strings.Replace(good, `"outcome":"failed"`, `"outcome":"failed","outcome":"failed"`, 1),
		"cased field":    strings.Replace(good, `"resultHash"`, `"ResultHash"`, 1),
		"float exitCode": strings.Replace(good, `"exitCode":1`, `"exitCode":1.0`, 1),
		"trailing data":  good + " {}",
		"empty":          ``,
		"oversized":      `{"x":"` + strings.Repeat("a", resultshttp.MaxResultBodyBytes) + `"}`,
	}
	for name, body := range bad {
		rec := do(h, http.MethodPost, "/runtime/v1/attempts/att_01/result", []byte(body), "tok")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if calls != 0 {
		t.Fatalf("a malformed request reached the service %d times", calls)
	}
	src := `{"commit":"3333333333333333333333333333333333333333","expectedTip":"1111111111111111111111111111111111111111","bundleArtifactId":"art_b"}`
	if rec := do(h, http.MethodPost, "/runtime/v1/attempts/att_01/source", []byte(strings.Replace(src, `"commit"`, `"extra":1,"commit"`, 1)), "tok"); rec.Code != 400 {
		t.Errorf("source unknown field: %d", rec.Code)
	}
	// A request without a usable bearer never reads or parses the body.
	for name, hdr := range map[string][]string{"none": nil, "basic": {"Basic abc"}, "two": {"Bearer a", "Bearer b"}, "spaced": {"Bearer a b"}} {
		req := httptest.NewRequest(http.MethodPost, "/runtime/v1/attempts/att_01/result", strings.NewReader(good))
		for _, v := range hdr {
			req.Header.Add("Authorization", v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s bearer: %d", name, rec.Code)
		}
	}
	// Routes and methods this module does not own are a contract not_found.
	for _, c := range []struct{ method, path string }{
		{"GET", "/runtime/v1/attempts/att_01/result"}, {"POST", "/runtime/v1/attempts/att_01/admit"}, {"POST", "/other"},
	} {
		rec := do(h, c.method, c.path, nil, "tok")
		if rec.Code != 404 && rec.Code != 405 {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	// An invalid attempt identifier is rejected before the service.
	if rec := do(h, http.MethodPost, "/runtime/v1/attempts/not%20valid/result", []byte(good), "tok"); rec.Code == 200 || calls != 0 {
		t.Errorf("invalid attempt: %d (%d calls)", rec.Code, calls)
	}
}

type countingService struct{ n *int }

func (c *countingService) Submit(context.Context, results.SubmitCommand) (runtimev1.ResultResponse, error) {
	*c.n++
	return runtimev1.ResultResponse{}, errors.New("unreachable")
}
func (c *countingService) ProposeSource(context.Context, results.ProposeCommand) (results.SourceOutcome, error) {
	*c.n++
	return results.SourceOutcome{}, errors.New("unreachable")
}

func TestNewHandlerRequiresService(t *testing.T) {
	if _, err := resultshttp.NewHandler(resultshttp.Config{}); err == nil {
		t.Fatal("a handler without a service was constructed")
	}
	_ = io.Discard
}
