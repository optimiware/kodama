package results_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// TestAuthorityDenials: IT-06, IT-A05, PX-A06 (the authorization half of
// `cancel_finalization_race`). Every denial is decided from the durable grant
// and the attempt's execution state: wrong audience, path binding, retired
// signing generation, expired or revoked grant, an action the attempt does not
// allow and terminal non-result actions all fail closed; explicit revocation
// defeats receipt replay. Real PostgreSQL through the real adapters; no NATS,
// S3 or Git.
func TestAuthorityDenials(t *testing.T) {
	e := New(t, Options{})
	p := e.NewProject(ScopeA)
	a := e.Run(p, Job("build", readRun, "outputs: {label: {type: string}}"))
	b := e.Run(e.NewProject(ScopeA), Job("build", readRun, "outputs: {label: {type: string}}"))
	req := Request(t, Completed(outputsOf("v1")))
	attempted := func() int { return e.Commits.Count() }
	untouched := func(who *Attempt) {
		t.Helper()
		if e.Persisted(who).ResultHash != "" {
			t.Fatal("a denied request changed the attempt")
		}
	}
	status := func(att *Attempt, token string, route string, body []byte) (int, domain.ErrorCode) {
		code, resp := e.doToken(att.ID(), token, route, body)
		if code == http.StatusOK || code == http.StatusAccepted {
			return code, ""
		}
		return code, errCode(t, resp)
	}
	body := mustJSON(t, req)

	t.Run("wrong audience", func(t *testing.T) {
		tok := e.mint(a, rtest.MintClaims{Audience: "kodama-public"})
		if code, c := status(a, tok, "result", body); code != 401 || c != domain.CodeInvalidCredential {
			t.Fatalf("%d %s", code, c)
		}
		untouched(a)
	})
	t.Run("another attempt's token on this route", func(t *testing.T) {
		if code, c := status(a, b.Token, "result", body); code != 401 || c != domain.CodeInvalidCredential {
			t.Fatalf("%d %s", code, c)
		}
		if code, c := status(b, a.Token, "source", mustJSON(t, runtimev1.SourceProposalRequest{Commit: "1111111111111111111111111111111111111111", ExpectedTip: "1111111111111111111111111111111111111111", BundleArtifactID: "art_x"})); code != 401 {
			t.Fatalf("source: %d %s", code, c)
		}
		untouched(a)
		untouched(b)
	})
	t.Run("a token that is not signed by this installation", func(t *testing.T) {
		for _, tok := range []string{"", "not.a.jwt", a.Token + "x", a.Token[:len(a.Token)-4]} {
			if code, _ := status(a, tok, "result", body); code != 401 {
				t.Fatalf("token %q: %d", tok, code)
			}
		}
		if attempted() != 0 {
			t.Fatal("an unauthenticated request reached execution")
		}
	})
	t.Run("an action the attempt does not allow", func(t *testing.T) {
		// A reader attempt is never granted source.propose.
		src := mustJSON(t, runtimev1.SourceProposalRequest{Commit: "1111111111111111111111111111111111111111", ExpectedTip: "1111111111111111111111111111111111111111", BundleArtifactID: "art_x"})
		if code, c := status(a, a.Token, "source", src); code != 401 || c != domain.CodeInvalidCredential {
			t.Fatalf("%d %s", code, c)
		}
		if _, err := e.Results.ProposeSource(context.Background(), results.ProposeCommand{Attempt: a.ID(), Bearer: a.Token, Request: runtimev1.SourceProposalRequest{
			Commit: "1111111111111111111111111111111111111111", ExpectedTip: "1111111111111111111111111111111111111111", BundleArtifactID: "art_x"}}); !errors.Is(err, results.ErrForbidden) {
			t.Fatalf("the service classifies it forbidden: %v", err)
		}
	})
	t.Run("a changed binding denies the callback", func(t *testing.T) {
		c := e.Run(e.NewProject(ScopeA), Job("build", readRun))
		e.X.OwnerExec(`UPDATE step_attempts SET authority_revision = authority_revision + 1 WHERE id = $1`, string(c.ID()))
		if code, cd := status(c, c.Token, "result", mustJSON(t, Request(t, Completed(nil)))); code != 403 || cd != runtimev1.CodeAttemptRevoked {
			t.Fatalf("%d %s", code, cd)
		}
		untouched(c)
	})
	t.Run("a retired signing generation", func(t *testing.T) {
		d := e.Run(e.NewProject(ScopeA), Job("build", readRun))
		old := d.Token
		next, err := rtjwt.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Ring.Rotate(next); err != nil {
			t.Fatal(err)
		}
		if code, _ := status(d, old, "result", mustJSON(t, Request(t, Completed(nil)))); code != 401 {
			t.Fatalf("a token of the retired generation: %d", code)
		}
		untouched(d)
	})
	t.Run("an expired session", func(t *testing.T) {
		f := e.Run(e.NewProject(ScopeA), Job("build", readRun))
		e.X.Clock.Advance(3 * time.Hour) // beyond the token lifetime and the maximum session
		if code, c := status(f, f.Token, "result", mustJSON(t, Request(t, Completed(nil)))); code != 401 || c != domain.CodeInvalidCredential {
			t.Fatalf("%d %s", code, c)
		}
		untouched(f)
	})
}

func (e *Env) mint(a *Attempt, c rtest.MintClaims) string {
	e.T.Helper()
	claims, err := e.Verifier.Verify(context.Background(), a.Token)
	must(e.T, err)
	c.Subject, c.Tenant = string(claims.Session), string(a.Scope.TenantID)
	c.IssuedAt = e.X.Clock.Now().Add(-time.Minute)
	c.ExpiresAt = e.X.Clock.Now().Add(4 * time.Minute)
	return rtest.MintWith(e.T, e.Ring.Current(), c)
}

// TestReceiptAndRevocation: the frozen `result.submit` exception. A retry after
// the attempt left active reaches execution (original 200 or 409), while an
// explicit revocation, a terminal non-result action or a cancelled attempt's
// late callback is denied.
func TestReceiptAndRevocation(t *testing.T) {
	e := New(t, Options{})
	a := e.Run(e.NewProject(ScopeA), Job("build", readRun, "outputs: {label: {type: string}}"))
	req := Request(t, Completed(outputsOf("v1")))
	first, err := e.Submit(a, req)
	if err != nil {
		t.Fatal(err)
	}
	if e.Persisted(a).Status.Terminal() == false {
		t.Fatal("not terminal")
	}

	// Terminal, grant intact: the receipt is still observable, a conflict still reported.
	if again, err := e.Submit(a, req); err != nil || again != first {
		t.Fatalf("receipt replay: %+v %v", again, err)
	}
	if _, err := e.Submit(a, Request(t, Completed(outputsOf("v2")))); !errors.Is(err, results.ErrResultConflict) {
		t.Fatalf("receipt conflict: %v", err)
	}
	// Other actions are not receipts: a terminal attempt may not propose source.
	if _, err := e.Results.ProposeSource(context.Background(), results.ProposeCommand{Attempt: a.ID(), Bearer: a.Token, Request: runtimev1.SourceProposalRequest{
		Commit: "1111111111111111111111111111111111111111", ExpectedTip: "1111111111111111111111111111111111111111", BundleArtifactID: "art_x"}}); err == nil {
		t.Fatal("a terminal attempt proposed source")
	}
	// Explicit revocation defeats receipt replay: the callback is denied.
	if err := e.Auth.Revoke(context.Background(), a.Scope, a.ID()); err != nil {
		t.Fatal(err)
	}
	code, body := e.SubmitHTTP(a, req)
	if code != 403 || errCode(t, body) != runtimev1.CodeAttemptRevoked {
		t.Fatalf("a replay after revocation: %d %s", code, body)
	}
	if e.Persisted(a).ResultHash != string(req.ResultHash) {
		t.Fatal("revocation changed the accepted result")
	}
}
