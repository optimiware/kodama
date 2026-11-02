package jwt

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
)

var (
	bg  = context.Background()
	t0  = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	iss = "https://git.t136.invalid"
)

type kit struct {
	signer   *Signer
	fwd, idn *KeyRing
	// worker holds the forwarding PUBLIC keys only; control the identity public
	// keys only. Both are built from PEM, the way public keys are distributed.
	worker  *Verifier
	control *Verifier
	fwdPub  *PublicKeyRing
	idnPub  *PublicKeyRing
	now     time.Time
}

func distribute(t *testing.T, keys PublicKeys) *PublicKeyRing {
	t.Helper()
	var out PublicKeys
	for i, src := range []*ecdsa.PublicKey{keys.Current, keys.Previous} {
		if src == nil {
			continue
		}
		pemBytes, err := MarshalPublicKeyPEM(src)
		if err != nil {
			t.Fatal(err)
		}
		pub, err := ParsePublicKeyPEM(pemBytes)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			out.Current = pub
		} else {
			out.Previous = pub
		}
	}
	r, err := NewPublicKeyRing(out)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newKit(t *testing.T) *kit {
	t.Helper()
	ring := func() *KeyRing {
		key, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewKeyRing(1, key)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	k := &kit{fwd: ring(), idn: ring(), now: t0}
	var err error
	k.signer, err = NewSigner(SignerConfig{Issuer: iss, Forwarding: k.fwd, Identity: k.idn, Clock: func() time.Time { return k.now }})
	if err != nil {
		t.Fatal(err)
	}
	k.fwdPub = distribute(t, k.signer.ForwardingPublicKeys())
	k.idnPub = distribute(t, k.signer.IdentityPublicKeys())
	if k.worker, err = NewVerifier(VerifierConfig{Issuer: iss, Forwarding: k.fwdPub}); err != nil {
		t.Fatal(err)
	}
	if k.control, err = NewVerifier(VerifierConfig{Issuer: iss, Identity: k.idnPub}); err != nil {
		t.Fatal(err)
	}
	return k
}

// syncKeys distributes the signer's current public keys, as a rotation does.
func (k *kit) syncKeys(t *testing.T) {
	t.Helper()
	for ring, keys := range map[*PublicKeyRing]PublicKeys{k.fwdPub: k.signer.ForwardingPublicKeys(), k.idnPub: k.signer.IdentityPublicKeys()} {
		if err := ring.Replace(keys); err != nil {
			t.Fatal(err)
		}
	}
}

func ts(t time.Time) gitv1.Timestamp { return gitv1.Timestamp(t.UTC().Format(time.RFC3339Nano)) }

func assertion(tenant string, issued time.Time, life time.Duration) gitv1.ForwardingAssertion {
	return gitv1.ForwardingAssertion{
		Audience: gitv1.WorkerAudience(tenant), TenantID: tenant, RepositoryID: "repo_01", GrantID: "gg_01", ReceivePack: true,
		IssuedAt: ts(issued), ExpiresAt: ts(issued.Add(life)), RequestID: "req_01",
	}
}

func identityOf(tenant string, issued time.Time, life time.Duration) gitv1.WorkerIdentity {
	return gitv1.WorkerIdentity{Audience: gitv1.ControlAudience, TenantID: tenant, WorkerInstance: "worker_7f", IssuedAt: ts(issued), ExpiresAt: ts(issued.Add(life))}
}

func wantCode(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	if !gitauth.IsCode(err, code) {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

// craft signs arbitrary claims with a key, kid and typ: a correctly signed token
// that is wrong in one respect.
func craft(t *testing.T, key *ecdsa.PrivateKey, kid, typ string, claims any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader(jose.HeaderKey("kid"), kid))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := josejwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func std(sub string, aud string, issued time.Time, life time.Duration) josejwt.Claims {
	return josejwt.Claims{Issuer: iss, Subject: sub, Audience: josejwt.Audience{aud}, ID: "jti",
		IssuedAt: josejwt.NewNumericDate(issued), NotBefore: josejwt.NewNumericDate(issued), Expiry: josejwt.NewNumericDate(issued.Add(life))}
}

// Test_unit_gitauth_forwarding_assertion_sign_verify: a forwarding assertion is
// signed by the control plane and verified by the tenant's worker with key
// rotation; every way it can be wrong is refused (S3, GR-03, IT-17).
func Test_unit_gitauth_forwarding_assertion_sign_verify(t *testing.T) {
	k := newKit(t)
	a := assertion("t_acme", t0, 4*time.Minute)
	tok, err := k.signer.SignForwarding(bg, a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(tok, ".") != 2 || strings.Contains(tok, a.GrantID+"."+a.RepositoryID) {
		t.Fatalf("token shape: %q", tok)
	}
	got, err := k.worker.VerifyForwarding(bg, tok, "t_acme", t0.Add(time.Minute))
	if err != nil || got != a {
		t.Fatalf("verify = %+v, %v", got, err)
	}
	if err := got.CheckService("git-receive-pack"); err != nil {
		t.Fatalf("receivePack: %v", err)
	}
	// The claim set is the contract's: fractional seconds survive exactly.
	frac := assertion("t_acme", t0.Add(123456*time.Microsecond), 4*time.Minute)
	ftok, _ := k.signer.SignForwarding(bg, frac)
	if got, err := k.worker.VerifyForwarding(bg, ftok, "t_acme", t0.Add(time.Minute)); err != nil || got != frac {
		t.Fatalf("fractional verify = %+v, %v", got, err)
	}

	// Tenant binding: another tenant's worker refuses it (forbidden).
	_, err = k.worker.VerifyForwarding(bg, tok, "t_globex", t0.Add(time.Minute))
	wantCode(t, err, domain.CodeForbidden)

	// Window: not before issuance (skew allowed), not at or after expiry.
	if _, err := k.worker.VerifyForwarding(bg, tok, "t_acme", t0.Add(-gitv1.ClockSkew)); err != nil {
		t.Fatalf("within skew: %v", err)
	}
	for _, at := range []time.Time{t0.Add(-gitv1.ClockSkew - time.Second), t0.Add(4 * time.Minute), t0.Add(time.Hour)} {
		_, err := k.worker.VerifyForwarding(bg, tok, "t_acme", at)
		wantCode(t, err, domain.CodeInvalidCredential)
	}

	// Signing refuses what the contract refuses: lifetime above five minutes,
	// another audience, an already expired assertion.
	long := assertion("t_acme", t0, 6*time.Minute)
	_, err = k.signer.SignForwarding(bg, long)
	wantCode(t, err, domain.CodeInvalidRequest)
	wrong := a
	wrong.Audience = gitv1.GrantAudience
	_, err = k.signer.SignForwarding(bg, wrong)
	wantCode(t, err, domain.CodeInvalidRequest)
	wrong.Audience = gitv1.WorkerAudience("t_globex")
	_, err = k.signer.SignForwarding(bg, wrong)
	wantCode(t, err, domain.CodeInvalidRequest)
	k.now = t0.Add(10 * time.Minute)
	_, err = k.signer.SignForwarding(bg, a)
	wantCode(t, err, domain.CodeInvalidRequest)
	k.now = t0

	// Tampering: payload, signature, truncation and junk are one answer.
	parts := strings.Split(tok, ".")
	flip := func(s string) string {
		b := []byte(s)
		if b[2] == 'A' {
			b[2] = 'B'
		} else {
			b[2] = 'A'
		}
		return string(b)
	}
	for name, bad := range map[string]string{
		"payload":   parts[0] + "." + flip(parts[1]) + "." + parts[2],
		"signature": parts[0] + "." + parts[1] + "." + flip(parts[2]),
		"header":    flip(parts[0]) + "." + parts[1] + "." + parts[2],
		"truncated": tok[:len(tok)-10],
		"empty":     "",
		"junk":      "not-a-token",
		"huge":      strings.Repeat("a", MaxTokenBytes+1),
	} {
		_, err := k.worker.VerifyForwarding(bg, bad, "t_acme", t0.Add(time.Minute))
		if !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err = k.worker.VerifyForwarding(bg, tok, "bad tenant", t0)
	wantCode(t, err, domain.CodeInvalidCredential)

	// Algorithm confusion and unknown keys.
	hs, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(strings.Repeat("k", 32))}, (&jose.SignerOptions{}).WithType(jose.ContentType(forwardingType)))
	if err != nil {
		t.Fatal(err)
	}
	hsTok, _ := josejwt.Signed(hs).Claims(forwardingClaims{Claims: std("gg_01", a.Audience, t0, 4*time.Minute), Assertion: a}).Serialize()
	_, err = k.worker.VerifyForwarding(bg, hsTok, "t_acme", t0)
	wantCode(t, err, domain.CodeInvalidCredential)
	foreign, _ := GenerateKey()
	foreignTok := craft(t, foreign, k.fwd.Current().KeyID, forwardingType, forwardingClaims{Claims: std("gg_01", a.Audience, t0, 4*time.Minute), Assertion: a})
	_, err = k.worker.VerifyForwarding(bg, foreignTok, "t_acme", t0)
	wantCode(t, err, domain.CodeInvalidCredential) // right kid, wrong key
	other := craft(t, foreign, "unknown-kid", forwardingType, forwardingClaims{Claims: std("gg_01", a.Audience, t0, 4*time.Minute), Assertion: a})
	_, err = k.worker.VerifyForwarding(bg, other, "t_acme", t0)
	wantCode(t, err, domain.CodeInvalidCredential)

	// A correctly signed token whose standard claims disagree with the contract
	// document is refused: issuer, audience, subject, window.
	cur := k.fwd.Current()
	sign := func(c josejwt.Claims, doc gitv1.ForwardingAssertion) string {
		return craft(t, cur.Key, cur.KeyID, forwardingType, forwardingClaims{Claims: c, Assertion: doc})
	}
	good := std("gg_01", a.Audience, t0, 4*time.Minute)
	if _, err := k.worker.VerifyForwarding(bg, sign(good, a), "t_acme", t0); err != nil {
		t.Fatalf("control: %v", err)
	}
	for name, tok := range map[string]string{
		"issuer":   sign(func() josejwt.Claims { c := good; c.Issuer = "https://elsewhere"; return c }(), a),
		"subject":  sign(std("gg_other", a.Audience, t0, 4*time.Minute), a),
		"audience": sign(std("gg_01", gitv1.WorkerAudience("t_globex"), t0, 4*time.Minute), a),
		"expiry":   sign(std("gg_01", a.Audience, t0, time.Hour), a),
		"issued":   sign(std("gg_01", a.Audience, t0.Add(time.Hour), 4*time.Minute), a),
		"lifetime": sign(std("gg_01", a.Audience, t0, 4*time.Minute), assertion("t_acme", t0, time.Hour)),
	} {
		if _, err := k.worker.VerifyForwarding(bg, tok, "t_acme", t0); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Errorf("%s: %v", name, err)
		}
	}
	noDoc := craft(t, cur.Key, cur.KeyID, forwardingType, good)
	_, err = k.worker.VerifyForwarding(bg, noDoc, "t_acme", t0)
	wantCode(t, err, domain.CodeInvalidCredential)
	wrongType := craft(t, cur.Key, cur.KeyID, "JWT", forwardingClaims{Claims: good, Assertion: a})
	_, err = k.worker.VerifyForwarding(bg, wrongType, "t_acme", t0)
	wantCode(t, err, domain.CodeInvalidCredential)

	// Key rotation: the previous key still verifies tokens signed just before
	// the rotation; new tokens use the new key; retiring ends the overlap.
	next, _ := GenerateKey()
	gen, err := k.fwd.Rotate(next)
	if err != nil || gen != 2 {
		t.Fatalf("rotate: %d %v", gen, err)
	}
	k.syncKeys(t) // the worker receives the new public keys (current and previous)
	if _, err := k.worker.VerifyForwarding(bg, tok, "t_acme", t0.Add(time.Minute)); err != nil {
		t.Fatalf("a token signed before the rotation verifies during the overlap: %v", err)
	}
	tok2, err := k.signer.SignForwarding(bg, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.worker.VerifyForwarding(bg, tok2, "t_acme", t0.Add(time.Minute)); err != nil {
		t.Fatalf("new key: %v", err)
	}
	k.fwd.RetirePrevious()
	k.syncKeys(t)
	_, err = k.worker.VerifyForwarding(bg, tok, "t_acme", t0.Add(time.Minute))
	wantCode(t, err, domain.CodeInvalidCredential)
	if _, err := k.worker.VerifyForwarding(bg, tok2, "t_acme", t0.Add(time.Minute)); err != nil {
		t.Fatalf("the current key keeps verifying: %v", err)
	}
	// Two rotations without retirement drop the oldest key; reuse is refused.
	third, _ := GenerateKey()
	if _, err := k.fwd.Rotate(third); err != nil {
		t.Fatal(err)
	}
	k.syncKeys(t)
	if _, err := k.fwd.Rotate(next); err == nil {
		t.Fatal("a key that was used before cannot be rotated in again")
	}
	fourth, _ := GenerateKey()
	if _, err := k.fwd.Rotate(fourth); err != nil {
		t.Fatal(err)
	}
	k.syncKeys(t)
	_, err = k.worker.VerifyForwarding(bg, tok2, "t_acme", t0.Add(time.Minute))
	wantCode(t, err, domain.CodeInvalidCredential) // two generations back

	if k.fwd.Generation() != 4 {
		t.Fatalf("generation = %d", k.fwd.Generation())
	}
}

// Test_unit_gitauth_worker_identity_tenant_bound: a worker identity is bound to
// one tenant, has its own audience and rotates with its own keys (S3, IT-17).
func Test_unit_gitauth_worker_identity_tenant_bound(t *testing.T) {
	k := newKit(t)
	w := identityOf("t_acme", t0, 50*time.Minute)
	tok, err := k.signer.SignWorkerIdentity(bg, w)
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.control.VerifyWorkerIdentity(bg, tok, "t_acme", t0.Add(10*time.Minute))
	if err != nil || got != w {
		t.Fatalf("verify = %+v, %v", got, err)
	}
	// A worker of one tenant is refused on every other tenant's route.
	_, err = k.control.VerifyWorkerIdentity(bg, tok, "t_globex", t0.Add(10*time.Minute))
	wantCode(t, err, domain.CodeForbidden)
	_, err = k.control.VerifyWorkerIdentity(bg, tok, "bad tenant", t0)
	wantCode(t, err, domain.CodeInvalidCredential)

	// Window and lifetime.
	for _, at := range []time.Time{t0.Add(-time.Minute), t0.Add(50 * time.Minute), t0.Add(2 * time.Hour)} {
		_, err := k.control.VerifyWorkerIdentity(bg, tok, "t_acme", at)
		wantCode(t, err, domain.CodeInvalidCredential)
	}
	_, err = k.signer.SignWorkerIdentity(bg, identityOf("t_acme", t0, 2*time.Hour))
	wantCode(t, err, domain.CodeInvalidRequest)
	bad := w
	bad.Audience = gitv1.WorkerAudience("t_acme")
	_, err = k.signer.SignWorkerIdentity(bg, bad)
	wantCode(t, err, domain.CodeInvalidRequest)
	bad = w
	bad.WorkerInstance = "bad instance"
	_, err = k.signer.SignWorkerIdentity(bg, bad)
	wantCode(t, err, domain.CodeInvalidRequest)

	// Tampering, a foreign key and a wrong audience in a correctly signed token.
	parts := strings.Split(tok, ".")
	_, err = k.control.VerifyWorkerIdentity(bg, parts[0]+"."+parts[1]+"."+parts[2][:len(parts[2])-4]+"AAAA", "t_acme", t0)
	wantCode(t, err, domain.CodeInvalidCredential)
	cur := k.idn.Current()
	good := std(w.WorkerInstance, gitv1.ControlAudience, t0, 50*time.Minute)
	if _, err := k.control.VerifyWorkerIdentity(bg, craft(t, cur.Key, cur.KeyID, workerIdentityType, identityClaims{Claims: good, Identity: w}), "t_acme", t0); err != nil {
		t.Fatalf("control: %v", err)
	}
	foreign, _ := GenerateKey()
	for name, crafted := range map[string]string{
		"foreign key": craft(t, foreign, cur.KeyID, workerIdentityType, identityClaims{Claims: good, Identity: w}),
		"audience":    craft(t, cur.Key, cur.KeyID, workerIdentityType, identityClaims{Claims: std(w.WorkerInstance, gitv1.WorkerAudience("t_acme"), t0, 50*time.Minute), Identity: w}),
		"subject":     craft(t, cur.Key, cur.KeyID, workerIdentityType, identityClaims{Claims: std("worker_other", gitv1.ControlAudience, t0, 50*time.Minute), Identity: w}),
		"tenant swap": craft(t, cur.Key, cur.KeyID, workerIdentityType, identityClaims{Claims: good, Identity: func() gitv1.WorkerIdentity { x := w; x.TenantID = "bad tenant"; return x }()}),
		"wrong type":  craft(t, cur.Key, cur.KeyID, forwardingType, identityClaims{Claims: good, Identity: w}),
		"no document": craft(t, cur.Key, cur.KeyID, workerIdentityType, good),
	} {
		if _, err := k.control.VerifyWorkerIdentity(bg, crafted, "t_acme", t0); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Rotation with overlap, then retirement.
	next, _ := GenerateKey()
	if _, err := k.idn.Rotate(next); err != nil {
		t.Fatal(err)
	}
	k.syncKeys(t)
	if _, err := k.control.VerifyWorkerIdentity(bg, tok, "t_acme", t0.Add(time.Minute)); err != nil {
		t.Fatalf("overlap: %v", err)
	}
	tok2, err := k.signer.SignWorkerIdentity(bg, w)
	if err != nil {
		t.Fatal(err)
	}
	k.idn.RetirePrevious()
	k.syncKeys(t)
	_, err = k.control.VerifyWorkerIdentity(bg, tok, "t_acme", t0.Add(time.Minute))
	wantCode(t, err, domain.CodeInvalidCredential)
	if _, err := k.control.VerifyWorkerIdentity(bg, tok2, "t_acme", t0.Add(time.Minute)); err != nil {
		t.Fatalf("current: %v", err)
	}
}

func Test_unit_gitauth_tokens_configuration(t *testing.T) {
	k := newKit(t)
	if _, err := NewSigner(SignerConfig{Forwarding: k.fwd, Identity: k.idn}); err == nil {
		t.Fatal("an issuer is required")
	}
	if _, err := NewSigner(SignerConfig{Issuer: iss, Forwarding: k.fwd, Identity: k.fwd}); err == nil {
		t.Fatal("the rings must differ")
	}
	if _, err := NewSigner(SignerConfig{Issuer: iss, Forwarding: k.fwd}); err == nil {
		t.Fatal("both rings are required")
	}
	sameKey, err := NewKeyRing(1, k.fwd.Current().Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSigner(SignerConfig{Issuer: iss, Forwarding: k.fwd, Identity: sameKey}); err == nil {
		t.Fatal("a key signing forwarding assertions must not sign worker identities")
	}
	if _, err := NewVerifier(VerifierConfig{Forwarding: k.fwdPub}); err == nil {
		t.Fatal("a verifier needs an issuer")
	}
	if _, err := NewVerifier(VerifierConfig{Issuer: iss}); err == nil {
		t.Fatal("a verifier needs a public key ring")
	}
	if _, err := NewKeyRing(0, k.fwd.Current().Key); err == nil {
		t.Fatal("generation starts at 1")
	}
	if _, err := NewKeyRing(1, nil); err == nil {
		t.Fatal("a key is required")
	}
	if _, err := k.fwd.Rotate(k.fwd.Current().Key); err == nil {
		t.Fatal("rotation requires a different key")
	}
	if _, err := ParsePrivateKeyPEM([]byte("not pem")); err == nil {
		t.Fatal("PEM is required")
	}
}

// Test_unit_gitauth_verifier_cannot_sign: a worker is given public keys only. It
// verifies what an independent signer issued, can mint nothing, cannot verify
// the other token kind, and rejects tokens of any other signer (IT-17, IT-19).
func Test_unit_gitauth_verifier_cannot_sign(t *testing.T) {
	k := newKit(t)
	a := assertion("t_acme", t0, 4*time.Minute)
	tok, err := k.signer.SignForwarding(bg, a)
	if err != nil {
		t.Fatal(err)
	}
	// The signer and the verifier are independent instances sharing only public keys.
	if got, err := k.worker.VerifyForwarding(bg, tok, "t_acme", t0); err != nil || got != a {
		t.Fatalf("independent verifier: %+v %v", got, err)
	}
	// The verifier type has no signing or issuing method.
	var v any = k.worker
	if _, ok := v.(interface {
		SignForwarding(context.Context, gitv1.ForwardingAssertion) (string, error)
	}); ok {
		t.Fatal("a verifier must not sign forwarding assertions")
	}
	if _, ok := v.(interface {
		SignWorkerIdentity(context.Context, gitv1.WorkerIdentity) (string, error)
	}); ok {
		t.Fatal("a verifier must not issue worker identities")
	}
	if _, ok := v.(gitauth.ForwardingSigner); ok {
		t.Fatal("a verifier must not be a ForwardingSigner")
	}
	if _, ok := v.(gitauth.WorkerIdentityIssuer); ok {
		t.Fatal("a verifier must not be a WorkerIdentityIssuer")
	}
	// Distribution carries public material: a private key is not accepted as one.
	priv, _ := GenerateKey()
	privPEM := pemEncodePrivate(t, priv)
	if _, err := ParsePublicKeyPEM(privPEM); err == nil {
		t.Fatal("a private key must not be accepted where a public key is expected")
	}
	pubPEM, err := MarshalPublicKeyPEM(&priv.PublicKey)
	if err != nil || strings.Contains(string(pubPEM), "PRIVATE") {
		t.Fatalf("public PEM: %v", err)
	}
	if _, err := NewPublicKeyRing(PublicKeys{}); err == nil {
		t.Fatal("a ring needs a current key")
	}
	if _, err := NewPublicKeyRing(PublicKeys{Current: &priv.PublicKey, Previous: &priv.PublicKey}); err == nil {
		t.Fatal("previous must differ from current")
	}
	if _, err := NewPublicKeyRing(PublicKeys{Current: &ecdsa.PublicKey{}}); err == nil {
		t.Fatal("an invalid key is refused")
	}

	// A verifier holding another signer's public keys rejects this signer's tokens.
	other := newKit(t)
	if _, err := other.worker.VerifyForwarding(bg, tok, "t_acme", t0); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Fatalf("foreign public keys: %v", err)
	}
	// A forwarding-only verifier (a worker) cannot verify identities; it is not configured to.
	idTok, _ := k.signer.SignWorkerIdentity(bg, identityOf("t_acme", t0, 10*time.Minute))
	if _, err := k.worker.VerifyWorkerIdentity(bg, idTok, "t_acme", t0); !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("a worker verifier verifying identities: %v", err)
	}
	if _, err := k.control.VerifyForwarding(bg, tok, "t_acme", t0); !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("an identity verifier verifying assertions: %v", err)
	}

	// Rotation reaches the verifier as new public keys only.
	next, _ := GenerateKey()
	if _, err := k.fwd.Rotate(next); err != nil {
		t.Fatal(err)
	}
	if _, err := k.worker.VerifyForwarding(bg, mustSign(t, k, a), "t_acme", t0); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Fatalf("before the new public keys arrive a rotated key is unknown: %v", err)
	}
	k.syncKeys(t)
	if _, err := k.worker.VerifyForwarding(bg, tok, "t_acme", t0); err != nil {
		t.Fatalf("the previous public key overlaps: %v", err)
	}
	if _, err := k.worker.VerifyForwarding(bg, mustSign(t, k, a), "t_acme", t0); err != nil {
		t.Fatalf("the new public key: %v", err)
	}
	// Replacing with the same keys is a no-op (a poller repeats it).
	k.syncKeys(t)
	if _, err := k.worker.VerifyForwarding(bg, tok, "t_acme", t0); err != nil {
		t.Fatal(err)
	}
	// A signer's own verifier follows its rings.
	if _, err := k.signer.Verifier().VerifyForwarding(bg, tok, "t_acme", t0); err != nil {
		t.Fatalf("signer verifier: %v", err)
	}
	if _, err := k.signer.Tokens().VerifyForwarding(bg, tok, "t_acme", t0); err != nil {
		t.Fatalf("tokens: %v", err)
	}
}

func mustSign(t *testing.T, k *kit, a gitv1.ForwardingAssertion) string {
	t.Helper()
	tok, err := k.signer.SignForwarding(bg, a)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func pemEncodePrivate(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// Test_unit_gitauth_worker_identity_renewal: a worker holds an issued,
// tenant-bound identity token and renews it through the control plane; it never
// holds an identity signing key.
func Test_unit_gitauth_worker_identity_renewal(t *testing.T) {
	k := newKit(t)
	w := identityOf("t_acme", t0, 20*time.Minute)
	tok, err := k.signer.SignWorkerIdentity(bg, w)
	if err != nil {
		t.Fatal(err)
	}
	k.now = t0.Add(15 * time.Minute)
	renewed, err := k.signer.RenewWorkerIdentity(bg, tok, "t_acme", 30*time.Minute)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	got, err := k.control.VerifyWorkerIdentity(bg, renewed, "t_acme", k.now)
	if err != nil || got.TenantID != "t_acme" || got.WorkerInstance != w.WorkerInstance || got.Audience != gitv1.ControlAudience {
		t.Fatalf("renewed identity = %+v %v", got, err)
	}
	if exp, _ := got.ExpiresAt.Time(); !exp.Equal(k.now.Add(30 * time.Minute)) {
		t.Fatalf("expires %s", got.ExpiresAt)
	}
	// Tenant binding, lifetime bound and expiry of the old token.
	_, err = k.signer.RenewWorkerIdentity(bg, tok, "t_globex", 30*time.Minute)
	wantCode(t, err, domain.CodeForbidden)
	_, err = k.signer.RenewWorkerIdentity(bg, tok, "t_acme", 2*time.Hour)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = k.signer.RenewWorkerIdentity(bg, tok, "t_acme", 0)
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = k.signer.RenewWorkerIdentity(bg, "junk", "t_acme", time.Minute)
	wantCode(t, err, domain.CodeInvalidCredential)
	k.now = t0.Add(20 * time.Minute)
	_, err = k.signer.RenewWorkerIdentity(bg, tok, "t_acme", 30*time.Minute)
	wantCode(t, err, domain.CodeInvalidCredential) // expired: re-provision
	// A forwarding assertion is not a renewable identity.
	fwd := mustSign(t, k, assertion("t_acme", k.now, time.Minute))
	_, err = k.signer.RenewWorkerIdentity(bg, fwd, "t_acme", time.Minute)
	wantCode(t, err, domain.CodeInvalidCredential)
}

// Test_unit_gitauth_token_times_are_enforced: both token kinds require nbf, iat
// and exp, nbf equal to iat, and the contract's 30 s skew bound, whatever the
// token library tolerates.
func Test_unit_gitauth_token_times_are_enforced(t *testing.T) {
	k := newKit(t)
	fa, ia := assertion("t_acme", t0, 4*time.Minute), identityOf("t_acme", t0, 30*time.Minute)
	fwdKey, idKey := k.fwd.Current(), k.idn.Current()
	fwd := func(c josejwt.Claims, doc gitv1.ForwardingAssertion) string {
		return craft(t, fwdKey.Key, fwdKey.KeyID, forwardingType, forwardingClaims{Claims: c, Assertion: doc})
	}
	idn := func(c josejwt.Claims, doc gitv1.WorkerIdentity) string {
		return craft(t, idKey.Key, idKey.KeyID, workerIdentityType, identityClaims{Claims: c, Identity: doc})
	}
	fstd := func(mod func(*josejwt.Claims)) josejwt.Claims {
		c := std(fa.GrantID, fa.Audience, t0, 4*time.Minute)
		if mod != nil {
			mod(&c)
		}
		return c
	}
	istd := func(mod func(*josejwt.Claims)) josejwt.Claims {
		c := std(ia.WorkerInstance, ia.Audience, t0, 30*time.Minute)
		if mod != nil {
			mod(&c)
		}
		return c
	}
	nd := func(t time.Time) *josejwt.NumericDate { return josejwt.NewNumericDate(t) }
	mods := map[string]func(*josejwt.Claims){
		"missing nbf":        func(c *josejwt.Claims) { c.NotBefore = nil },
		"missing iat":        func(c *josejwt.Claims) { c.IssuedAt = nil },
		"missing exp":        func(c *josejwt.Claims) { c.Expiry = nil },
		"nbf before iat":     func(c *josejwt.Claims) { c.NotBefore = nd(t0.Add(-time.Minute)) },
		"nbf after iat":      func(c *josejwt.Claims) { c.NotBefore = nd(t0.Add(time.Minute)) },
		"nbf far in future":  func(c *josejwt.Claims) { c.NotBefore = nd(t0.Add(time.Hour)) },
		"nbf in the distant": func(c *josejwt.Claims) { c.NotBefore = nd(t0.Add(-time.Hour)) },
	}
	if _, err := k.worker.VerifyForwarding(bg, fwd(fstd(nil), fa), "t_acme", t0); err != nil {
		t.Fatalf("control (forwarding): %v", err)
	}
	if _, err := k.control.VerifyWorkerIdentity(bg, idn(istd(nil), ia), "t_acme", t0); err != nil {
		t.Fatalf("control (identity): %v", err)
	}
	for name, mod := range mods {
		if _, err := k.worker.VerifyForwarding(bg, fwd(fstd(mod), fa), "t_acme", t0); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Errorf("forwarding, %s: %v", name, err)
		}
		if _, err := k.control.VerifyWorkerIdentity(bg, idn(istd(mod), ia), "t_acme", t0); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Errorf("identity, %s: %v", name, err)
		}
	}
	// The skew bound: a token issued up to 30 s ahead of the verifier's clock is
	// accepted, one issued later is not, with nbf and iat consistent with the
	// document in both cases.
	for _, ahead := range []time.Duration{gitv1.ClockSkew, gitv1.ClockSkew + time.Second, time.Minute} {
		issued := t0.Add(ahead)
		fdoc, idoc := assertion("t_acme", issued, 4*time.Minute), identityOf("t_acme", issued, 30*time.Minute)
		_, ferr := k.worker.VerifyForwarding(bg, fwd(std(fdoc.GrantID, fdoc.Audience, issued, 4*time.Minute), fdoc), "t_acme", t0)
		_, ierr := k.control.VerifyWorkerIdentity(bg, idn(std(idoc.WorkerInstance, idoc.Audience, issued, 30*time.Minute), idoc), "t_acme", t0)
		if ahead <= gitv1.ClockSkew {
			if ferr != nil || ierr != nil {
				t.Errorf("%s ahead is within the skew bound: %v %v", ahead, ferr, ierr)
			}
			continue
		}
		if !gitauth.IsCode(ferr, domain.CodeInvalidCredential) || !gitauth.IsCode(ierr, domain.CodeInvalidCredential) {
			t.Errorf("%s ahead exceeds the skew bound: %v %v", ahead, ferr, ierr)
		}
	}
	// A token whose nbf and iat are in the future but whose document says otherwise is refused.
	future := t0.Add(time.Hour)
	if _, err := k.worker.VerifyForwarding(bg, fwd(std(fa.GrantID, fa.Audience, future, 4*time.Minute), fa), "t_acme", t0); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Errorf("standard times that disagree with the document: %v", err)
	}
	// Issued tokens always carry nbf equal to iat.
	tok := mustSign(t, k, fa)
	var c forwardingClaims
	parsed, err := josejwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.UnsafeClaimsWithoutVerification(&c); err != nil {
		t.Fatal(err)
	}
	if c.NotBefore == nil || c.IssuedAt == nil || *c.NotBefore != *c.IssuedAt {
		t.Fatalf("issued token nbf=%v iat=%v", c.NotBefore, c.IssuedAt)
	}
}
