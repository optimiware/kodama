package jwt_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

const issuer = rtest.Issuer

var now = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

type kit struct {
	ring     *jwt.KeyRing
	signer   *jwt.Signer
	verifier *jwt.Verifier
	clock    *rtest.Clock
	session  runtimeauth.SessionID
}

func newKit(t *testing.T) *kit {
	t.Helper()
	key, err := jwt.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ring, err := jwt.NewKeyRing(1, key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jwt.NewSigner(ring, issuer)
	if err != nil {
		t.Fatal(err)
	}
	clock := rtest.NewClock()
	verifier, err := jwt.NewVerifier(jwt.VerifierConfig{Ring: ring, Issuer: issuer, MaxLifetime: 2 * time.Hour, Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	ses, err := runtimeauth.NewSessionID(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &kit{ring: ring, signer: signer, verifier: verifier, clock: clock, session: ses}
}

func (k *kit) issue(t *testing.T) string {
	t.Helper()
	tok, err := k.signer.Issue(runtimeauth.IssueRequest{Tenant: "t019_acme", Session: k.session, Generation: k.ring.Generation(), IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (k *kit) mint(t *testing.T, c rtest.MintClaims) string {
	t.Helper()
	if c.Subject == "" {
		c.Subject = string(k.session)
	}
	if c.Tenant == "" {
		c.Tenant = "t019_acme"
	}
	return rtest.MintWith(t, k.ring.Current(), c)
}

func (k *kit) verify(tok string) (runtimeauth.TokenClaims, error) {
	return k.verifier.Verify(context.Background(), tok)
}

func rejected(t *testing.T, name string, k *kit, tok string) {
	t.Helper()
	_, err := k.verify(tok)
	if !errors.Is(err, runtimeauth.ErrInvalidCredential) {
		t.Errorf("%s: got %v, want invalid credential", name, err)
		return
	}
	if tok != "" && strings.Contains(err.Error(), tok) {
		t.Errorf("%s: the error quotes the token", name)
	}
}

func Test_token_validation_and_bounds(t *testing.T) {
	k := newKit(t)

	t.Run("round trip carries tenant and session only", func(t *testing.T) {
		tok := k.issue(t)
		c, err := k.verify(tok)
		if err != nil {
			t.Fatal(err)
		}
		if c.Tenant != "t019_acme" || c.Session != k.session || c.Generation != 1 || c.TokenID == "" {
			t.Fatalf("claims = %+v", c)
		}
		if !c.ExpiresAt.Equal(now.Add(5*time.Minute)) || !c.IssuedAt.Equal(now) {
			t.Fatalf("times = %v %v", c.IssuedAt, c.ExpiresAt)
		}
		// The compact token never carries project, attempt, Job or actions.
		for _, forbidden := range []string{"project", "attempt", "action", "job", "pod"} {
			payload := strings.Split(tok, ".")[1]
			raw, _ := jose.ParseSigned(tok, []jose.SignatureAlgorithm{jose.ES256})
			if raw == nil || strings.Contains(strings.ToLower(string(raw.UnsafePayloadWithoutVerification())), forbidden) {
				t.Errorf("payload %q mentions %s", payload, forbidden)
			}
		}
	})

	t.Run("two tokens differ", func(t *testing.T) {
		if k.issue(t) == k.issue(t) {
			t.Fatal("token identifiers repeat")
		}
	})

	t.Run("bad signature", func(t *testing.T) {
		tok := k.issue(t)
		parts := strings.Split(tok, ".")
		sig := []byte(parts[2])
		if sig[10] == 'A' {
			sig[10] = 'B'
		} else {
			sig[10] = 'A'
		}
		rejected(t, "tampered signature", k, parts[0]+"."+parts[1]+"."+string(sig))
		rejected(t, "tampered payload", k, parts[0]+"."+parts[1][:len(parts[1])-2]+"xx."+parts[2])
		rejected(t, "truncated", k, tok[:len(tok)-10])
		rejected(t, "empty", k, "")
		rejected(t, "garbage", k, "not-a-token")
		rejected(t, "two parts", k, parts[0]+"."+parts[1])
	})

	t.Run("signed by another key", func(t *testing.T) {
		other, _ := jwt.GenerateKey()
		snap := k.ring.Current()
		snap.Key = other // right key ID, wrong key material
		tok := rtest.MintWith(t, snap, rtest.MintClaims{Subject: string(k.session), Tenant: "t019_acme"})
		rejected(t, "wrong key, current kid", k, tok)
		snap.KeyID = "unknown-kid"
		rejected(t, "unknown kid", k, rtest.MintWith(t, snap, rtest.MintClaims{Subject: string(k.session), Tenant: "t019_acme"}))
	})

	t.Run("issuer and audience", func(t *testing.T) {
		rejected(t, "wrong issuer", k, k.mint(t, rtest.MintClaims{Issuer: "https://human.example.invalid"}))
		rejected(t, "wrong audience", k, k.mint(t, rtest.MintClaims{Audience: "kodama-api"}))
		rejected(t, "empty-ish audience", k, k.mint(t, rtest.MintClaims{Audience: "kodama-runtime2"}))
		if _, err := k.verify(k.mint(t, rtest.MintClaims{})); err != nil {
			t.Fatalf("the control token must verify: %v", err)
		}
	})

	t.Run("algorithms", func(t *testing.T) {
		rsa := func() string {
			// An RS256 token is rejected before any key lookup.
			return "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCIsImtpZCI6IngifQ.eyJpc3MiOiJ4In0.c2ln"
		}()
		rejected(t, "RS256", k, rsa)
		rejected(t, "none", k, "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJpc3MiOiJ4In0.")
		rejected(t, "HS256", k, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJ4In0.c2ln")
		p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		snap := k.ring.Current()
		snap.Key = p384
		tok := rtest.MintWith(t, snap, rtest.MintClaims{Subject: string(k.session), Tenant: "t019_acme", Algorithm: jose.ES384})
		rejected(t, "ES384", k, tok)
	})

	t.Run("expiry and validity window", func(t *testing.T) {
		tok := k.issue(t) // valid from `now` for five minutes; the clock starts at `now`
		if _, err := k.verify(tok); err != nil {
			t.Fatal(err)
		}
		k.clock.Advance(5*time.Minute - time.Second)
		if _, err := k.verify(tok); err != nil {
			t.Fatalf("one second before expiry: %v", err)
		}
		k.clock.Advance(time.Second)
		rejected(t, "at expiry", k, tok)
		k.clock.Advance(time.Hour)
		rejected(t, "long after expiry", k, tok)
		k.clock.Advance(-(time.Hour + 5*time.Minute + 10*time.Minute)) // back before issuance
		rejected(t, "before nbf", k, tok)
		k.clock.Advance(10 * time.Minute)
	})

	t.Run("malformed claims", func(t *testing.T) {
		rejected(t, "no tenant", k, rtest.MintWith(t, k.ring.Current(), rtest.MintClaims{Subject: string(k.session), Tenant: ""}))
		rejected(t, "bad tenant", k, k.mint(t, rtest.MintClaims{Tenant: "bad tenant"}))
		rejected(t, "bad subject", k, k.mint(t, rtest.MintClaims{Subject: "user-42"}))
		rejected(t, "other generation", k, k.mint(t, rtest.MintClaims{Generation: 9}))
		rejected(t, "expired by claim", k, k.mint(t, rtest.MintClaims{IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}))
		rejected(t, "exp before iat", k, k.mint(t, rtest.MintClaims{IssuedAt: now, ExpiresAt: now.Add(-time.Second)}))
	})

	t.Run("maximum lifetime", func(t *testing.T) {
		rejected(t, "longer than the maximum", k, k.mint(t, rtest.MintClaims{IssuedAt: now, ExpiresAt: now.Add(2*time.Hour + time.Second)}))
		if _, err := k.verify(k.mint(t, rtest.MintClaims{IssuedAt: now, ExpiresAt: now.Add(2 * time.Hour)})); err != nil {
			t.Fatalf("exactly the maximum: %v", err)
		}
	})

	t.Run("wire size bound", func(t *testing.T) {
		long := domain.TenantID(strings.Repeat("t", 128))
		tok, err := k.signer.Issue(runtimeauth.IssueRequest{Tenant: long, Session: k.session, Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) > jwt.MaxTokenBytes {
			t.Fatalf("a maximum-length tenant gives %d bytes", len(tok))
		}
		rejected(t, "oversized input", k, strings.Repeat("a", jwt.MaxTokenBytes+1))
	})

	t.Run("issue refuses invalid requests and stale generations", func(t *testing.T) {
		good := runtimeauth.IssueRequest{Tenant: "t019_acme", Session: k.session, Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
		for name, mutate := range map[string]func(*runtimeauth.IssueRequest){
			"tenant":   func(r *runtimeauth.IssueRequest) { r.Tenant = "" },
			"session":  func(r *runtimeauth.IssueRequest) { r.Session = "x" },
			"lifetime": func(r *runtimeauth.IssueRequest) { r.ExpiresAt = r.IssuedAt },
			"retired":  func(r *runtimeauth.IssueRequest) { r.Generation = 0 },
			"future":   func(r *runtimeauth.IssueRequest) { r.Generation = 2 },
		} {
			r := good
			mutate(&r)
			if _, err := k.signer.Issue(r); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		_, err := k.signer.Issue(func() runtimeauth.IssueRequest { r := good; r.Generation = 2; return r }())
		if !errors.Is(err, runtimeauth.ErrInvalidCredential) {
			t.Errorf("a stale generation must be an invalid credential: %v", err)
		}
	})
}

func Test_key_rotation(t *testing.T) {
	k := newKit(t)
	old := k.issue(t)
	if _, err := k.verify(old); err != nil {
		t.Fatal(err)
	}
	next, _ := jwt.GenerateKey()
	gen, err := k.ring.Rotate(next)
	if err != nil || gen != 2 || k.signer.Generation() != 2 {
		t.Fatalf("rotate: %d %v", gen, err)
	}
	rejected(t, "token of the retired generation", k, old)
	tok, err := k.signer.Issue(runtimeauth.IssueRequest{Tenant: "t019_acme", Session: k.session, Generation: 2, IssuedAt: now, ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	c, err := k.verify(tok)
	if err != nil || c.Generation != 2 {
		t.Fatalf("new token: %+v %v", c, err)
	}
	if _, err := k.signer.Issue(runtimeauth.IssueRequest{Tenant: "t019_acme", Session: k.session, Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
		t.Fatalf("issuing for the retired generation: %v", err)
	}
	if _, err := k.ring.Rotate(next); err == nil {
		t.Fatal("rotating to the current key was accepted")
	}
}

func Test_rotation_races(t *testing.T) {
	k := newKit(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				gen := k.ring.Generation()
				tok, err := k.signer.Issue(runtimeauth.IssueRequest{Tenant: "t019_acme", Session: k.session, Generation: gen, IssuedAt: now, ExpiresAt: now.Add(time.Minute)})
				if err != nil {
					if !errors.Is(err, runtimeauth.ErrInvalidCredential) {
						t.Errorf("issue: %v", err)
					}
					continue // a rotation raced the request: fail closed
				}
				// A token that verifies must belong to the generation in
				// force at verification time or have been retired meanwhile.
				if c, err := k.verify(tok); err == nil && c.Generation > k.ring.Generation() {
					t.Errorf("a token from the future verified: %d", c.Generation)
				}
			}
		}()
	}
	for range 25 {
		key, _ := jwt.GenerateKey()
		before := k.issue(t)
		if _, err := k.ring.Rotate(key); err != nil {
			t.Fatal(err)
		}
		// Once Rotate has returned, the token from before can never verify.
		if _, err := k.verify(before); err == nil {
			t.Fatal("a token verified after rotation returned")
		}
	}
	close(stop)
	wg.Wait()
}

func Test_key_material(t *testing.T) {
	if _, err := jwt.NewKeyRing(0, mustKey(t)); err == nil {
		t.Error("generation 0 accepted")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := jwt.NewKeyRing(1, p384); err == nil {
		t.Error("a P-384 key was accepted")
	}
	if _, err := jwt.NewKeyRing(1, nil); err == nil {
		t.Error("a nil key was accepted")
	}
	if _, err := jwt.NewSigner(nil, issuer); err == nil {
		t.Error("a nil ring was accepted")
	}
	ring, _ := jwt.NewKeyRing(1, mustKey(t))
	if _, err := jwt.NewSigner(ring, ""); err == nil {
		t.Error("an empty issuer was accepted")
	}
	if _, err := jwt.NewVerifier(jwt.VerifierConfig{Ring: ring, Issuer: issuer}); err == nil {
		t.Error("a verifier without a maximum lifetime was accepted")
	}
	key := mustKey(t)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	parsed, err := jwt.ParsePrivateKeyPEM(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil || !parsed.Equal(key) {
		t.Fatalf("PKCS#8: %v", err)
	}
	sec1, _ := x509.MarshalECPrivateKey(key)
	if parsed, err = jwt.ParsePrivateKeyPEM(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})); err != nil || !parsed.Equal(key) {
		t.Fatalf("SEC1: %v", err)
	}
	if _, err := jwt.ParsePrivateKeyPEM([]byte("nope")); err == nil {
		t.Error("garbage parsed")
	}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := jwt.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
