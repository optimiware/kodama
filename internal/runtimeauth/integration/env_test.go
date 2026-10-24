package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

var bg = context.Background()

// world is one stack with the frozen happy-path attempt registered.
type world struct {
	*rtest.Stack
	Attempt rtest.Attempt
	Cred    runtimeauth.Credential
	Req     runtimev1.AdmitRequest // the frozen admit request, with the real credential
}

const nonce = "nnnnnnnnnnnnnnnnnnnnnn"

func newWorld(t *testing.T) *world {
	t.Helper()
	s := rtest.New(t, rtest.Options{})
	c, frozen := s.HappyContext()
	a := s.SeedAttempt(c, rtest.AllActions())
	cred, _, err := s.Register(a, rtest.AllActions())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	req := frozen
	req.AdmissionCredential = cred.Reveal()
	return &world{Stack: s, Attempt: a, Cred: cred, Req: req}
}

type reply struct {
	Status int
	Body   []byte
	Header http.Header
}

func (r reply) errorCode(t *testing.T) domain.ErrorCode {
	t.Helper()
	e, err := runtimev1.DecodeResponse[runtimev1.ErrorResponse](r.Body)
	if err != nil {
		t.Fatalf("not a contract error (%d): %s", r.Status, r.Body)
	}
	if want, ok := runtimev1.Status(e.Error.Code); !ok || want != r.Status {
		t.Fatalf("code %s with status %d", e.Error.Code, r.Status)
	}
	return e.Error.Code
}

func (w *world) call(method, attempt, path string, body any, bearer string) reply {
	w.T.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			w.T.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/runtime/v1/attempts/"+attempt+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	w.Handler.ServeHTTP(rec, req)
	return reply{Status: rec.Code, Body: rec.Body.Bytes(), Header: rec.Header()}
}

func (w *world) admit(req runtimev1.AdmitRequest) reply {
	w.T.Helper()
	return w.call("POST", "att_01", "/admit", req, "")
}

func (w *world) mustAdmit(req runtimev1.AdmitRequest) runtimev1.AdmitResponse {
	w.T.Helper()
	r := w.admit(req)
	if r.Status != 200 {
		w.T.Fatalf("admit: %d %s", r.Status, r.Body)
	}
	resp, err := runtimev1.DecodeResponse[runtimev1.AdmitResponse](r.Body)
	if err != nil {
		w.T.Fatal(err)
	}
	return resp
}

func (w *world) getContext(token string) reply {
	return w.call("GET", "att_01", "/context", nil, token)
}
func (w *world) renew(token string) reply {
	return w.call("POST", "att_01", "/token", runtimev1.TokenRequest{}, token)
}

// grantRow reads the stored consumption state as the owner.
type grantRow struct {
	Consumed bool
	Revoked  bool
	Session  string
}

func (w *world) grantRow(attempt string) grantRow {
	w.T.Helper()
	var g grantRow
	var session *string
	err := w.Owner.QueryRow(bg, `SELECT session_id, revoked_at IS NOT NULL FROM runtime_grants WHERE attempt_id = $1`, attempt).Scan(&session, &g.Revoked)
	if err != nil {
		w.T.Fatal(err)
	}
	if session != nil {
		g.Consumed, g.Session = true, *session
	}
	return g
}

func scenarioError(t *testing.T, name string) (int, domain.ErrorCode) {
	t.Helper()
	sc := rtest.LoadScenario(t, name)
	for _, st := range sc.Steps {
		if st.Status >= 400 {
			e, err := runtimev1.DecodeResponse[runtimev1.ErrorResponse](st.Body)
			if err != nil {
				t.Fatal(err)
			}
			return st.Status, e.Error.Code
		}
	}
	t.Fatalf("scenario %s has no error step", name)
	return 0, ""
}

func contains(s []byte, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(string(s), sub) {
			return true
		}
	}
	return false
}

func newKey() (*ecdsa.PrivateKey, error) { return jwt.GenerateKey() }
