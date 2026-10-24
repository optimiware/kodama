package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

var bg = context.Background()

func seed(t *testing.T) (*rtest.Stack, rtest.Attempt, runtimeauth.Credential, runtimev1.AdmitRequest) {
	t.Helper()
	s := rtest.New(t, rtest.Options{Conns: 32})
	c, frozen := s.HappyContext()
	a := s.SeedAttempt(c, rtest.AllActions())
	cred, _, err := s.Register(a, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	req := frozen
	req.AdmissionCredential = cred.Reveal()
	return s, a, cred, req
}

func consumptions(t *testing.T, s *rtest.Stack, attempt string) (n int, session string) {
	t.Helper()
	var ses *string
	if err := s.Owner.QueryRow(bg, `SELECT count(session_id), max(session_id) FROM runtime_grants WHERE attempt_id = $1`, attempt).Scan(&n, &ses); err != nil {
		t.Fatal(err)
	}
	if ses != nil {
		session = *ses
	}
	return n, session
}

// Test_admission_concurrency: simultaneous same-nonce exchanges converge on one
// consumption and one session; competing nonces produce exactly one winner.
func Test_admission_concurrency(t *testing.T) {
	t.Run("same nonce converges", func(t *testing.T) {
		s, _, _, req := seed(t)
		const n = 24
		resps := make([]runtimev1.AdmitResponse, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resps[i], errs[i] = s.Service.Admit(bg, "att_01", req)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("exchange %d: %v", i, err)
			}
			if resps[i].Token.MaxExpiresAt != resps[0].Token.MaxExpiresAt {
				t.Fatalf("exchange %d saw another maximum", i)
			}
			claims, err := s.Verifier.Verify(bg, resps[i].Token.Token)
			if err != nil {
				t.Fatal(err)
			}
			if i > 0 {
				first, _ := s.Verifier.Verify(bg, resps[0].Token.Token)
				if claims.Session != first.Session {
					t.Fatalf("exchange %d got another session", i)
				}
			}
		}
		if cnt, _ := consumptions(t, s, "att_01"); cnt != 1 {
			t.Fatalf("consumptions = %d", cnt)
		}
	})

	t.Run("competing nonces have one winner", func(t *testing.T) {
		s, _, _, req := seed(t)
		const n = 16
		results := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r := req
				r.Nonce = fmt.Sprintf("nonce-%02d-%s", i, "xxxxxxxxxxxxxxxx")
				_, results[i] = s.Service.Admit(bg, "att_01", r)
			}()
		}
		close(start)
		wg.Wait()
		wins, conflicts := 0, 0
		for i, err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, runtimeauth.ErrAdmissionConsumed):
				conflicts++
			default:
				t.Fatalf("exchange %d: unexpected %v", i, err)
			}
		}
		if wins != 1 || conflicts != n-1 {
			t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
		}
		if cnt, _ := consumptions(t, s, "att_01"); cnt != 1 {
			t.Fatalf("consumptions = %d", cnt)
		}
	})

	t.Run("concurrent registration converges", func(t *testing.T) {
		s := rtest.New(t, rtest.Options{Conns: 16})
		c, _ := s.HappyContext()
		a := s.SeedAttempt(c, rtest.AllActions())
		cred, err := runtimeauth.GenerateCredential(nil)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		created := make(chan bool, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reg, err := s.RegisterWith(a, cred, rtest.AllActions())
				if err != nil {
					t.Errorf("register: %v", err)
					return
				}
				created <- reg.Created
			}()
		}
		wg.Wait()
		close(created)
		n := 0
		for c := range created {
			if c {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d registrations claimed creation", n)
		}
	})
}

// Test_cancel_admit_refresh_race: cancellation serializes with admission and
// refresh through the attempt lock. Whichever commits first decides; a
// cancellation that wins denies issuance, and one that follows denies every
// later use.
func Test_cancel_admit_refresh_race(t *testing.T) {
	t.Run("cancellation holding the lock blocks admission, which then fails", func(t *testing.T) {
		s, a, _, req := seed(t)
		held, release := make(chan struct{}), make(chan struct{})
		s.Authority.SetCancelPause(func() { close(held); <-release })
		cancelled := make(chan error, 1)
		go func() { cancelled <- s.Authority.Cancel(bg, a.Scope, "att_01", true) }()
		<-held // the cancel transaction holds the attempt row, uncommitted

		// The advisory read sees the still-active attempt, the Kubernetes
		// observation succeeds, and the locking read must then wait.
		done := make(chan error, 1)
		go func() { _, err := s.Service.Admit(bg, "att_01", req); done <- err }()
		select {
		case err := <-done:
			t.Fatalf("admission did not wait for the cancellation: %v", err)
		case <-time.After(400 * time.Millisecond):
		}
		close(release)
		if err := <-cancelled; err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
			t.Fatalf("admission after the cancellation committed: %v", err)
		}
		if n, _ := consumptions(t, s, "att_01"); n != 0 {
			t.Fatal("a cancelled attempt consumed its grant")
		}
	})

	t.Run("cancellation holding the lock blocks refresh, which then fails", func(t *testing.T) {
		s, a, _, req := seed(t)
		resp, err := s.Service.Admit(bg, "att_01", req)
		if err != nil {
			t.Fatal(err)
		}
		held, release := make(chan struct{}), make(chan struct{})
		s.Authority.SetCancelPause(func() { close(held); <-release })
		cancelled := make(chan error, 1)
		go func() { cancelled <- s.Authority.Cancel(bg, a.Scope, "att_01", true) }()
		<-held
		done := make(chan error, 1)
		go func() { _, err := s.Service.Renew(bg, "att_01", resp.Token.Token); done <- err }()
		select {
		case err := <-done:
			t.Fatalf("refresh did not wait for the cancellation: %v", err)
		case <-time.After(400 * time.Millisecond):
		}
		close(release)
		if err := <-cancelled; err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
			t.Fatalf("refresh after the cancellation committed: %v", err)
		}
	})

	t.Run("an in-flight admission holds the attempt against cancellation", func(t *testing.T) {
		s, a, _, req := seed(t)
		inside, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		s.Faults.SetAfterLock(func() { once.Do(func() { close(inside); <-release }) })
		done := make(chan error, 1)
		go func() { _, err := s.Service.Admit(bg, "att_01", req); done <- err }()
		<-inside // admission holds the attempt lock (FOR SHARE) and the grant lock
		cancelled := make(chan error, 1)
		go func() { cancelled <- s.Authority.Cancel(bg, a.Scope, "att_01", true) }()
		select {
		case err := <-cancelled:
			t.Fatalf("cancellation did not wait for the in-flight admission: %v", err)
		case <-time.After(400 * time.Millisecond):
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("the admission that won the ordering must succeed: %v", err)
		}
		if err := <-cancelled; err != nil {
			t.Fatal(err)
		}
		// Cancellation after issuance denies every later use.
		resp, err := s.Service.Admit(bg, "att_01", req)
		if !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
			t.Fatalf("replay after cancellation: %v %+v", err, resp.Token.ExpiresAt)
		}
	})

	t.Run("randomized races never leave usable authority after cancellation", func(t *testing.T) {
		s := rtest.New(t, rtest.Options{Conns: 32})
		base, _ := s.HappyContext()
		const rounds = 24
		for i := range rounds {
			attempt := domain.AttemptID(fmt.Sprintf("att_race_%02d", i))
			a := s.SeedAttempt(rtest.ContextFor(base, attempt), rtest.AllActions())
			cred, _, err := s.Register(a, rtest.AllActions())
			if err != nil {
				t.Fatal(err)
			}
			req := s.AdmitRequest(a, cred, "nonce-xxxxxxxxxxxxxxxxxxxxxx")

			var (
				wg          sync.WaitGroup
				admitted    runtimev1.AdmitResponse
				admitErr    error
				renewErr    error
				cancelErr   error
				renewTarget = make(chan string, 1)
			)
			wg.Add(3)
			go func() {
				defer wg.Done()
				admitted, admitErr = s.Service.Admit(bg, attempt, req)
				renewTarget <- admitted.Token.Token
			}()
			go func() { defer wg.Done(); cancelErr = s.Authority.Cancel(bg, a.Scope, attempt, i%2 == 0) }()
			go func() {
				defer wg.Done()
				if tok := <-renewTarget; tok != "" {
					_, renewErr = s.Service.Renew(bg, attempt, tok)
				}
			}()
			wg.Wait()
			if cancelErr != nil {
				t.Fatal(cancelErr)
			}
			n, _ := consumptions(t, s, string(attempt))
			switch {
			case admitErr == nil && n != 1:
				t.Fatalf("round %d: admitted without a durable consumption", i)
			case admitErr != nil && !errors.Is(admitErr, runtimeauth.ErrAttemptRevoked):
				t.Fatalf("round %d: unexpected admission error %v", i, admitErr)
			case errors.Is(admitErr, runtimeauth.ErrAttemptRevoked) && n != 0:
				t.Fatalf("round %d: a refused admission consumed the grant", i)
			}
			if renewErr != nil && !errors.Is(renewErr, runtimeauth.ErrAttemptRevoked) {
				t.Fatalf("round %d: unexpected renewal error %v", i, renewErr)
			}
			// Once the cancellation has committed, nothing works any more.
			if admitErr == nil {
				if _, err := s.Service.Renew(bg, attempt, admitted.Token.Token); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
					t.Fatalf("round %d: refresh after cancellation: %v", i, err)
				}
				if _, err := s.Service.Context(bg, attempt, admitted.Token.Token); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
					t.Fatalf("round %d: context after cancellation: %v", i, err)
				}
			}
			if _, err := s.Service.Admit(bg, attempt, req); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
				t.Fatalf("round %d: replay after cancellation: %v", i, err)
			}
		}
	})
}
