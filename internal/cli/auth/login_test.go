package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/root"
)

func exitOf(t *testing.T, err error) (root.ExitCode, string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	p := root.Classify(err)
	return p.Exit, p.Code
}

func Test_pkce_state(t *testing.T) {
	t.Run("S256 exchange binds state and verifier and keeps only the access token", func(t *testing.T) {
		p := newProvider(t)
		var authQuery url.Values
		env := newLoginEnv(t, p, nil)
		env.svc.cfg.Browser = BrowserFunc(func(_ context.Context, authURL string) error {
			authQuery = p.record(authURL)
			go callback(t, authQuery.Get("redirect_uri"), url.Values{"state": {authQuery.Get("state")}, "code": {"code-t017"}})
			return nil
		})
		res, err := env.svc.Login(context.Background(), env.ec)
		if err != nil {
			t.Fatal(err)
		}
		if authQuery.Get("code_challenge_method") != "S256" || authQuery.Get("response_type") != "code" ||
			authQuery.Get("client_id") != "kodama-cli" || authQuery.Get("code_challenge") == "" {
			t.Fatalf("authorization request is not a PKCE S256 code request: %v", authQuery)
		}
		if strings.Join(strings.Fields(authQuery.Get("scope")), ",") != "openid,kodama" {
			t.Fatalf("scope = %q", authQuery.Get("scope"))
		}
		verifier := p.lastForm.Get("code_verifier")
		if verifier == "" || verifier == authQuery.Get("state") || len(verifier) < 43 {
			t.Fatalf("verifier %q must be independent of state and at least 43 characters", verifier)
		}
		if p.lastForm.Get("client_secret") != "" {
			t.Fatal("a public client must not send a secret")
		}
		got, err := env.store.Load(context.Background(), env.ec.CredentialKey())
		if err != nil {
			t.Fatal(err)
		}
		if got.AccessToken != "access-token-t017" || strings.Contains(got.AccessToken, "id-token") {
			t.Fatalf("stored credential is not the access token: %+v", got)
		}
		if res.Factory != env.ec.Factory || res.ExpiresAt == "" {
			t.Fatalf("result = %+v", res)
		}
		out, _ := root.EncodeJSON(res)
		if strings.Contains(string(out), "access-token") || strings.Contains(string(out), "id-token") {
			t.Fatalf("result leaks a token: %s", out)
		}
	})

	t.Run("state and verifier differ between attempts", func(t *testing.T) {
		seen := map[string]bool{}
		for i := 0; i < 2; i++ {
			p := newProvider(t)
			env := newLoginEnv(t, p, nil)
			env.svc.cfg.Browser = approving(p, func(redirect, state string) {
				callback(t, redirect, url.Values{"state": {state}, "code": {"code-t017"}})
			})
			if _, err := env.svc.Login(context.Background(), env.ec); err != nil {
				t.Fatal(err)
			}
			v := p.lastForm.Get("code_verifier")
			if seen[v] {
				t.Fatal("verifier reused")
			}
			seen[v] = true
		}
	})

	t.Run("bad callbacks are rejected before any exchange", func(t *testing.T) {
		p := newProvider(t)
		env := newLoginEnv(t, p, nil)
		statuses := make(chan []int, 1)
		env.svc.cfg.Browser = approving(p, func(redirect, state string) {
			u, _ := url.Parse(redirect)
			var got []int
			do := func(method, path, host string, q string) {
				req, _ := http.NewRequest(method, "http://"+u.Host+path+"?"+q, nil)
				if host != "" {
					req.Host = host
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					got = append(got, -1)
					return
				}
				res.Body.Close()
				got = append(got, res.StatusCode)
			}
			do("GET", "/callback", "", "state=wrong&code=c")                           // mismatched state
			do("GET", "/callback", "", "code=c")                                       // missing state
			do("GET", "/callback", "", "state="+state+"&state="+state+"&code=c")       // duplicate state
			do("GET", "/callback", "", "state="+state)                                 // no code
			do("GET", "/callback", "", "state="+state+"&code=a&code=b")                // two codes
			do("GET", "/callback", "", "state="+state+"&code=a&error=access_denied")   // both
			do("GET", "/other", "", "state="+state+"&code=c")                          // wrong path
			do("POST", "/callback", "", "state="+state+"&code=c")                      // wrong method
			do("GET", "/callback", "rebind.example.invalid", "state="+state+"&code=c") // wrong host
			statuses <- got
			callback(t, redirect, url.Values{"state": {state}, "code": {"code-t017"}})
		})
		if _, err := env.svc.Login(context.Background(), env.ec); err != nil {
			t.Fatal(err)
		}
		want := []int{400, 400, 400, 400, 400, 400, 404, 405, 400}
		got := <-statuses
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("rejection %d = %d, want %d (all %v)", i, got[i], want[i], got)
			}
		}
		if n := p.exchanges.Load(); n != 1 {
			t.Fatalf("exchanges = %d; only the one valid callback may exchange", n)
		}
	})

	t.Run("success is reported only after persistence", func(t *testing.T) {
		p := newProvider(t)
		env := newLoginEnv(t, p, nil)
		env.store.saveErr = storeErr(ErrPersistenceFailed, nil, "disk full")
		env.svc.cfg.Browser = approving(p, func(redirect, state string) {
			callback(t, redirect, url.Values{"state": {state}, "code": {"code-t017"}})
		})
		_, err := env.svc.Login(context.Background(), env.ec)
		if exit, code := exitOf(t, err); exit != root.ExitFailure || code != "credential_persistence_failed" {
			t.Fatalf("exit/code = %d/%s", exit, code)
		}
		if env.store.count() != 0 {
			t.Fatal("a failed save must leave nothing usable")
		}
		if !portIsFree(env.port) {
			t.Fatal("listener leaked")
		}
	})

	t.Run("attempt transitions are single use", func(t *testing.T) {
		var a attempt
		if err := a.advance(stateListening, stateCallbackAccepted); !errors.Is(err, errBadTransition) {
			t.Fatalf("skipping a state must fail: %v", err)
		}
		for _, step := range [][2]attemptState{
			{stateCreated, stateListening}, {stateListening, stateCallbackAccepted},
			{stateCallbackAccepted, stateExchanged}, {stateExchanged, statePersisted},
		} {
			if err := a.advance(step[0], step[1]); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.advance(stateCallbackAccepted, stateExchanged); !errors.Is(err, errBadTransition) {
			t.Fatalf("a repeated transition must fail: %v", err)
		}
		var b attempt
		b.fail()
		if err := b.advance(stateCreated, stateListening); !errors.Is(err, errBadTransition) {
			t.Fatalf("a failed attempt must stay failed: %v", err)
		}
	})

	t.Run("configuration is validated before listening", func(t *testing.T) {
		p := newProvider(t)
		called := false
		env := newLoginEnv(t, p, BrowserFunc(func(context.Context, string) error { called = true; return nil }))
		bad := func(mod func(*root.EffectiveContext)) error {
			ec := env.ec
			ec.Auth.Scopes = append([]string(nil), ec.Auth.Scopes...)
			mod(&ec)
			_, err := env.svc.Login(context.Background(), ec)
			return err
		}
		for name, mod := range map[string]func(*root.EffectiveContext){
			"non-interactive":      func(ec *root.EffectiveContext) { ec.NonInteractive = true },
			"missing client":       func(ec *root.EffectiveContext) { ec.Auth.ClientID = "" },
			"localhost redirect":   func(ec *root.EffectiveContext) { ec.Auth.RedirectURI = "http://localhost:18999/callback" },
			"remote redirect":      func(ec *root.EffectiveContext) { ec.Auth.RedirectURI = "http://10.1.2.3:18999/callback" },
			"https redirect":       func(ec *root.EffectiveContext) { ec.Auth.RedirectURI = "https://127.0.0.1:18999/callback" },
			"no redirect path":     func(ec *root.EffectiveContext) { ec.Auth.RedirectURI = "http://127.0.0.1:18999/" },
			"no factory":           func(ec *root.EffectiveContext) { ec.Factory = "" },
			"unknown store":        func(ec *root.EffectiveContext) { ec.CredentialStore = "memory" },
			"plain http endpoint":  func(ec *root.EffectiveContext) { ec.Auth.TokenEndpoint = "http://idp.example.invalid/token" },
			"plain http authorize": func(ec *root.EffectiveContext) { ec.Auth.AuthorizationEndpoint = "http://idp.example.invalid/a" },
		} {
			err := bad(mod)
			if exit, _ := exitOf(t, err); exit != root.ExitUsage {
				t.Errorf("%s: exit = %d, want usage (%v)", name, exit, err)
			}
		}
		if called {
			t.Fatal("the browser must never open for an invalid or non-interactive login")
		}
	})

	t.Run("plain http provider endpoints need the injected development exception", func(t *testing.T) {
		p := newProvider(t)
		env := newLoginEnv(t, p, BrowserFunc(func(context.Context, string) error { return errors.New("unreachable") }),
			func(c *ServiceConfig) { c.AllowInsecureLoopbackEndpoints = false })
		_, err := env.svc.Login(context.Background(), env.ec)
		if exit, _ := exitOf(t, err); exit != root.ExitUsage {
			t.Fatalf("exit = %d, want usage", exit)
		}
	})
}

func Test_login_callback_concurrency(t *testing.T) {
	p := newProvider(t)
	env := newLoginEnv(t, p, nil)
	var mu sync.Mutex
	var ok int
	var wg sync.WaitGroup
	env.svc.cfg.Browser = approving(p, func(redirect, state string) {
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if callback(t, redirect, url.Values{"state": {state}, "code": {"code-t017"}}) == http.StatusOK {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}()
		}
	})
	if _, err := env.svc.Login(context.Background(), env.ec); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if n := p.exchanges.Load(); n != 1 {
		t.Fatalf("exchanges = %d, want exactly 1", n)
	}
	if env.store.saves != 1 {
		t.Fatalf("saves = %d, want exactly 1", env.store.saves)
	}
	if ok != 1 {
		t.Fatalf("%d callbacks were accepted, want exactly 1", ok)
	}
	// A replay after completion reaches a closed listener, never a second exchange.
	if st := callback(t, "http://127.0.0.1:"+portOf(env.ec.Auth.RedirectURI)+"/callback", url.Values{"state": {"x"}, "code": {"y"}}); st == http.StatusOK {
		t.Fatalf("replay status %d", st)
	}
	if n := p.exchanges.Load(); n != 1 {
		t.Fatalf("replay caused an exchange: %d", n)
	}
}

func portOf(raw string) string {
	u, _ := url.Parse(raw)
	return u.Port()
}

func Test_login_failure_cleanup(t *testing.T) {
	approve := func(p *provider, query func(state string) url.Values) BrowserOpener {
		return approving(p, func(redirect, state string) { callback(t, redirect, query(state)) })
	}
	good := func(state string) url.Values { return url.Values{"state": {state}, "code": {"code-t017"}} }

	cases := []struct {
		name     string
		setup    func(p *provider, env *loginEnv, cancel context.CancelFunc)
		wantExit root.ExitCode
		wantCode string
		wantXchg int32
	}{
		{"timeout", func(p *provider, env *loginEnv, _ context.CancelFunc) {
			env.svc.cfg.LoginTimeout = 80 * time.Millisecond
			env.svc.cfg.Browser = BrowserFunc(func(context.Context, string) error { return nil })
		}, root.ExitTimeout, "login_timeout", 0},
		{"cancellation", func(p *provider, env *loginEnv, cancel context.CancelFunc) {
			env.svc.cfg.Browser = BrowserFunc(func(context.Context, string) error { cancel(); return nil })
		}, root.ExitTimeout, "interrupted", 0},
		{"browser failure", func(p *provider, env *loginEnv, _ context.CancelFunc) {
			env.svc.cfg.Browser = BrowserFunc(func(context.Context, string) error { return errors.New("no display") })
		}, root.ExitFailure, "browser_unavailable", 0},
		{"provider denial", func(p *provider, env *loginEnv, _ context.CancelFunc) {
			env.svc.cfg.Browser = approve(p, func(s string) url.Values {
				return url.Values{"state": {s}, "error": {"access_denied"}, "error_description": {"x"}}
			})
		}, root.ExitAuth, "login_denied", 0},
		{"rejected exchange", func(p *provider, env *loginEnv, _ context.CancelFunc) {
			p.status = http.StatusBadRequest
			env.svc.cfg.Browser = approve(p, good)
		}, root.ExitAuth, "token_exchange_rejected", 1},
		{"uncertain exchange: connection dropped", func(p *provider, env *loginEnv, _ context.CancelFunc) {
			p.drop = true
			env.svc.cfg.Browser = approve(p, good)
		}, root.ExitFailure, "token_exchange_uncertain", 1},
		{"uncertain exchange: server error", func(p *provider, env *loginEnv, _ context.CancelFunc) {
			p.status = http.StatusServiceUnavailable
			env.svc.cfg.Browser = approve(p, good)
		}, root.ExitFailure, "token_exchange_uncertain", 1},
		// oauth2 reports a 200 without access_token as a plain error; the
		// code may be consumed, so it is handled like any uncertain exchange.
		{"no access token", func(p *provider, env *loginEnv, _ context.CancelFunc) {
			p.omitToken = true
			env.svc.cfg.Browser = approve(p, good)
		}, root.ExitFailure, "token_exchange_uncertain", 1},
		{"unsupported token type", func(p *provider, env *loginEnv, _ context.CancelFunc) {
			p.tokenType = "mac"
			env.svc.cfg.Browser = approve(p, good)
		}, root.ExitFailure, "token_unusable", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t)
			env := newLoginEnv(t, p, nil)
			// A credential from an earlier login must survive a failed attempt.
			prior := Credential{AccessToken: "prior-token"}
			if err := env.store.Save(context.Background(), env.ec.CredentialKey(), prior); err != nil {
				t.Fatal(err)
			}
			env.store.saves = 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.setup(p, env, cancel)
			_, err := env.svc.Login(ctx, env.ec)
			exit, code := exitOf(t, err)
			if exit != tc.wantExit || code != tc.wantCode {
				t.Fatalf("exit/code = %d/%s, want %d/%s (%v)", exit, code, tc.wantExit, tc.wantCode, err)
			}
			if got := p.exchanges.Load(); got != tc.wantXchg {
				t.Fatalf("exchanges = %d, want %d", got, tc.wantXchg)
			}
			if env.store.saves != 0 {
				t.Fatal("a failed login saved a credential")
			}
			if c, _ := env.store.Load(context.Background(), env.ec.CredentialKey()); c.AccessToken != "prior-token" {
				t.Fatal("a failed login must not disturb the previous credential")
			}
			if !portIsFree(env.port) {
				t.Fatal("the loopback listener was not closed")
			}
			msg := root.Classify(err)
			rendered := msg.Message + msg.Code
			for _, secret := range []string{"code-t017", "access-token-t017", "id-token-t017"} {
				if strings.Contains(rendered, secret) {
					t.Fatalf("error text leaks %q: %s", secret, rendered)
				}
			}
			if strings.HasPrefix(tc.name, "uncertain") && msg.Details["outcomeUncertain"] != true {
				t.Fatalf("details = %v", msg.Details)
			}
		})
	}

	t.Run("expired callback after the deadline is refused", func(t *testing.T) {
		now := time.Now()
		l, redirect, err := newLoopback(mustRedirect(t, freePort(t)), "state", now.Add(time.Second), func() time.Time { return now.Add(2 * time.Second) })
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if st := callback(t, redirect, url.Values{"state": {"state"}, "code": {"c"}}); st != http.StatusGone {
			t.Fatalf("status = %d, want 410", st)
		}
		select {
		case <-l.result:
			t.Fatal("an expired callback must not be accepted")
		default:
		}
	})

	t.Run("noninteractive login never opens a browser", func(t *testing.T) {
		p := newProvider(t)
		opened := false
		env := newLoginEnv(t, p, BrowserFunc(func(context.Context, string) error { opened = true; return nil }))
		env.ec.NonInteractive = true
		_, err := env.svc.Login(context.Background(), env.ec)
		if exit, code := exitOf(t, err); exit != root.ExitUsage || code != "interactive_required" {
			t.Fatalf("exit/code = %d/%s", exit, code)
		}
		if opened {
			t.Fatal("browser opened")
		}
	})
}

func mustRedirect(t *testing.T, port int) *url.URL {
	t.Helper()
	u, err := parseRedirect("http://127.0.0.1:" + strconv.Itoa(port) + "/callback")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func Test_token_source_binds_factory(t *testing.T) {
	p := newProvider(t)
	env := newLoginEnv(t, p, nil)
	svc, store := env.svc, env.store
	other := env.ec
	other.Factory = "https://other.t017.example.invalid"

	if err := store.Save(context.Background(), env.ec.CredentialKey(), Credential{AccessToken: "tok-a"}); err != nil {
		t.Fatal(err)
	}
	src, _ := svc.TokenSource(env.ec)
	if tok, err := src.AccessToken(context.Background()); err != nil || tok != "tok-a" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	src, _ = svc.TokenSource(other)
	if _, err := src.AccessToken(context.Background()); err == nil || root.Classify(err).Exit != root.ExitAuth {
		t.Fatalf("another factory must not see this credential: %v", err)
	}
	// Expired credential requires explicit login; no implicit refresh or login.
	expired := Credential{AccessToken: "tok-old", Expiry: time.Now().Add(-time.Hour)}
	_ = store.Save(context.Background(), env.ec.CredentialKey(), expired)
	src, _ = svc.TokenSource(env.ec)
	if _, err := src.AccessToken(context.Background()); err == nil || root.Classify(err).Code != "not_authenticated" {
		t.Fatalf("expired credential: %v", err)
	}
}

func Test_exchange_http_timeout_is_exit_5(t *testing.T) {
	p := newProvider(t)
	p.hang = true
	env := newLoginEnv(t, p, nil)
	env.svc.cfg.LoginTimeout = time.Minute
	env.svc.cfg.HTTP = &http.Client{Timeout: 50 * time.Millisecond}
	env.svc.cfg.Browser = approving(p, func(redirect, state string) {
		callback(t, redirect, url.Values{"state": {state}, "code": {"code-t017"}})
	})
	_, err := env.svc.Login(context.Background(), env.ec)
	exit, code := exitOf(t, err)
	if exit != root.ExitTimeout || code != "timeout" {
		t.Fatalf("exit %d code %s: %v", exit, code, err)
	}
	var le *root.Error
	if !errors.As(err, &le) || le.Details["outcomeUncertain"] != true {
		t.Fatalf("exchange timeout lost uncertainty: %#v", err)
	}
	if _, loadErr := env.store.Load(context.Background(), env.ec.CredentialKey()); loadErr == nil {
		t.Fatal("timed-out exchange persisted a credential")
	}
	if n := p.exchanges.Load(); n != 1 {
		t.Fatalf("exchanges = %d, want 1", n)
	}
}
