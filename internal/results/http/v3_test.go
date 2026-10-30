package resultshttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
	resultshttp "github.com/optimiware/kodama/internal/results/http"
)

// The source-set result handler (T-150): the route table, the strict v3 body, the
// v3 error envelope within its operation's inventory and the status mapping.

type scriptedV3 struct {
	err   error
	seen  []results.SubmitV3Command
	reply runtimev1.ResultResponse
}

func (s *scriptedV3) SubmitV3(_ context.Context, c results.SubmitV3Command) (runtimev1.ResultResponse, error) {
	s.seen = append(s.seen, c)
	return s.reply, s.err
}

func v3Request(t *testing.T) []byte {
	t.Helper()
	zero := int64(0)
	res := runtimev3.ResultV3{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{},
		Commits: []runtimev3.CommitOutput{{RepositoryID: "repo_01", Ref: "refs/heads/kodama/work/wi_1042", Commit: runtimev3.GitCommit(strings.Repeat("3", 40)), PushID: "pu_07"}}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	h, err := runtimev3.ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(runtimev3.ResultRequestV3{ResultHash: h, Result: res})
	return raw
}

func post(h http.Handler, path, token string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestV3HandlerServesTheResultRouteWithTheV3Envelope(t *testing.T) {
	svc := &scriptedV3{reply: runtimev1.ResultResponse{AttemptID: "att_01", ResultHash: "sha256:" + runtimev3.Digest(strings.Repeat("a", 64)), AttemptStatus: runtimev1.AttemptSucceeded,
		AcceptedAt: runtimev1.Timestamp(time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))}}
	h, err := resultshttp.NewV3Handler(resultshttp.ConfigV3{Service: svc})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/runtime/v3/attempts/att_01/result"
	body := v3Request(t)

	w := post(h, path, "tok", body)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || len(svc.seen) != 1 || svc.seen[0].Attempt != "att_01" || svc.seen[0].Bearer != "tok" ||
		len(svc.seen[0].Request.Result.Commits) != 1 {
		t.Fatalf("result: %d %s %+v", w.Code, w.Body, svc.seen)
	}
	if _, err := runtimev3.DecodeResponse[runtimev3.ResultResponse](w.Body.Bytes()); err != nil {
		t.Fatalf("the response violates the contract: %v", err)
	}

	// Anything else is the contract's not_found envelope; the v1 routes are not ours.
	for _, c := range []struct{ method, path string }{
		{"POST", "/runtime/v1/attempts/att_01/result"}, {"POST", "/runtime/v2/attempts/att_01/result"}, {"POST", "/runtime/v3/attempts/att_01/source"},
		{"GET", path},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == 200 || len(svc.seen) != 1 {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}

	// A strict body: a v1 result, an unknown member and a missing bearer never
	// reach the service.
	zero := int64(0)
	v1raw, _ := json.Marshal(runtimev1.ResultRequest{ResultHash: "sha256:" + runtimev1.Digest(strings.Repeat("a", 64)), Result: runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}})
	for name, c := range map[string]struct {
		token string
		body  []byte
		code  int
	}{
		"a v1 body":      {"tok", v1raw, 400},
		"an unknown key": {"tok", []byte(strings.Replace(string(body), `{"resultHash"`, `{"extra":1,"resultHash"`, 1)), 400},
		"no bearer":      {"", body, 401},
	} {
		if w := post(h, path, c.token, c.body); w.Code != c.code || len(svc.seen) != 1 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := post(h, "/runtime/v3/attempts/bad%20id/result", "tok", body); w.Code != 400 && w.Code != 401 {
		t.Errorf("a malformed attempt: %d", w.Code)
	}
}

func TestV3HandlerErrorEnvelopeStaysInsideTheInventory(t *testing.T) {
	const path = "/runtime/v3/attempts/att_01/result"
	for name, c := range map[string]struct {
		err    error
		status int
		code   domain.ErrorCode
	}{
		"version mismatch":   {results.Fail(results.ErrContextVersionMismatch, nil, "x"), 409, runtimev3.CodeContextVersionMismatch},
		"output invalid":     {results.Fail(results.ErrOutputInvalid, nil, "x"), 422, runtimev1.CodeOutputInvalid},
		"hash mismatch":      {results.Fail(results.ErrResultHashMismatch, nil, "x"), 422, runtimev1.CodeResultHashMismatch},
		"conflict":           {results.Fail(results.ErrResultConflict, nil, "x"), 409, runtimev1.CodeResultConflict},
		"revoked":            {results.Fail(results.ErrAttemptRevoked, nil, "x"), 403, runtimev1.CodeAttemptRevoked},
		"unauthenticated":    {results.Fail(results.ErrUnauthenticated, nil, "x"), 401, domain.CodeInvalidCredential},
		"not found":          {results.Fail(results.ErrNotFound, nil, "x"), 404, domain.CodeNotFound},
		"incomplete":         {results.Fail(results.ErrArtifactIncomplete, nil, "x"), 409, runtimev1.CodeArtifactIncomplete},
		"forbidden degrades": {results.Fail(results.ErrForbidden, nil, "x"), 403, domain.CodeForbidden},
		"unavailable":        {results.Fail(results.ErrUnavailable, errors.New("pgx: password=hunter2"), "x"), 503, domain.CodeUnavailable},
		"unclassified":       {errors.New("driver exploded"), 503, domain.CodeUnavailable},
	} {
		svc := &scriptedV3{err: c.err}
		h, _ := resultshttp.NewV3Handler(resultshttp.ConfigV3{Service: svc})
		w := post(h, path, "tok", v3Request(t))
		var env struct {
			Error struct {
				Code    domain.ErrorCode `json:"code"`
				Message string           `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if w.Code != c.status || env.Error.Code != c.code {
			t.Errorf("%s: %d %s, want %d %s", name, w.Code, env.Error.Code, c.status, c.code)
		}
		if strings.Contains(w.Body.String(), "hunter2") || strings.Contains(w.Body.String(), "exploded") {
			t.Errorf("%s: a cause leaked: %s", name, w.Body)
		}
		if want, ok := runtimev3.Status(env.Error.Code); !ok || want != w.Code {
			t.Errorf("%s: a code outside the contract's statuses", name)
		}
		if c.status == 503 && w.Header().Get("Retry-After") == "" {
			t.Errorf("%s: no retry guidance", name)
		}
		if c.status == 401 && w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no challenge", name)
		}
	}
	if _, err := resultshttp.NewV3Handler(resultshttp.ConfigV3{}); err == nil {
		t.Fatal("a handler without a service")
	}
}
