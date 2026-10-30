package adapters

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/secrets"
	secretsruntime "github.com/optimiware/kodama/internal/secrets/runtime"
)

// SecretRoute serves the runtime contract's resolveSecret operation
// (POST /runtime/v1/attempts/{attemptId}/secrets/resolve). It joins three
// owners and decides nothing itself:
//
//   - runtimeauth proves the bearer: the verified token, the durable grant, the
//     signing generation, the attempt's phase and authority revision and the
//     secret.resolve action are checked in a tenant transaction
//     (runtimeauth.CheckSession), exactly as every other runtime route does;
//   - what may be resolved is what the attempt's pinned environment names: the
//     bindings of the attempt's immutable pinned context (stored with the grant)
//     that equal, exactly (reference, version, name and delivery form), a
//     binding of the environment's retained configuration snapshot. The
//     snapshot is read by the pin (environment and digest) the context carries
//     and in the grant's scope, and must be intact and carry that pin. A
//     step-only binding, a reference or version outside the environment's
//     credentials, or any reference of an attempt that pins no environment, is
//     undeclared, whatever the request says. A missing, foreign, altered or
//     mismatching snapshot resolves nothing (not found); an unreadable one is
//     unavailable;
//   - the secrets module's Resolver decrypts exactly the pinned version in the
//     grant's scope, rechecks authority after the decryption and finalizes the
//     read-for-use audit atomically with the release of the value.
//
// The tenant and project come from the durable grant, never from the request
// or the route; a token of another tenant is invalid. The attempt authority is
// consulted again for every resolution (before and after the decryption), so a
// cancellation, revocation or retirement between them releases nothing.
//
// Repetition follows the secrets module: a second resolution of the same
// declared version in the same attempt is a new, separately authorized and
// audited resolution of the same pinned value; it is neither cached nor
// refused. It is refused as soon as the attempt is no longer active. No value
// is cached, logged or returned in an error.
type SecretRoute struct {
	verifier  runtimeauth.TokenVerifier
	gens      Generations
	store     runtimeauth.Store
	pins      EnvironmentSnapshots
	source    secrets.VersionSource
	clock     func() time.Time
	stepUnion bool
}

// EnvironmentSnapshots reads the immutable configuration snapshot retained
// under an environment pin, in the given scope. delivery.ErrNotFound is a
// missing or foreign snapshot; any other error is an outage.
// *delivery/postgres.Store (GetSnapshot) implements it.
type EnvironmentSnapshots interface {
	GetSnapshot(ctx context.Context, scope domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (delivery.ConfigurationSnapshot, error)
}

// Generations reports the signing generation in force. runtimeauth's token
// issuer (jwt.KeyRing, Signer) satisfies it.
type Generations interface{ Generation() int64 }

// NewSecretRoute requires every dependency; there is no allow-all fallback and
// a route without the environment snapshots or a version source cannot be
// built.
func NewSecretRoute(verifier runtimeauth.TokenVerifier, gens Generations, store runtimeauth.Store, pins EnvironmentSnapshots, source secrets.VersionSource, clock func() time.Time) (*SecretRoute, error) {
	if verifier == nil || gens == nil || store == nil || pins == nil || source == nil {
		return nil, errors.New("execution/adapters: a token verifier, a signing generation, a grant store, environment snapshots and a secret version source are required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &SecretRoute{verifier: verifier, gens: gens, store: store, pins: pins, source: source, clock: clock}, nil
}

// NewSecretUnionRoute constructs a SecretRoute that resolves the authorized
// union of a step's pinned environment credentials and its explicit step-declared
// secret bindings (ADR-0012, VS-15). Every environment credential in the retained
// snapshot must appear unaltered in the grant's digest-checked context, and any
// conflicting name, reference, version or delivery form fails closed.
func NewSecretUnionRoute(verifier runtimeauth.TokenVerifier, gens Generations, store runtimeauth.Store, pins EnvironmentSnapshots, source secrets.VersionSource, clock func() time.Time) (*SecretRoute, error) {
	r, err := NewSecretRoute(verifier, gens, store, pins, source, clock)
	if err != nil {
		return nil, err
	}
	r.stepUnion = true
	return r, nil
}

// Resolve resolves one declared secret for the route's attempt. attempt is the
// path's attempt and bearer the request's token; requestID is the
// server-generated identifier recorded in the audit entry. The error is always
// a *runtimev1.ContractError whose text never names the reference, the value or
// the cause.
func (r *SecretRoute) Resolve(ctx context.Context, attempt domain.AttemptID, bearer string, req runtimev1.SecretResolveRequest, requestID string) (runtimev1.SecretResolveResponse, error) {
	if err := attempt.Validate(); err != nil {
		return runtimev1.SecretResolveResponse{}, routeError(runtimeauth.ErrInvalidCredential)
	}
	claims, err := r.verifier.Verify(ctx, bearer)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, routeError(err)
	}
	authority := routeAuthority{r: r, route: attempt, claims: claims}
	// The first check is made here so an authority failure answers with its own
	// contract code; the resolver then consults the authority again around the
	// decryption.
	if _, err := authority.check(ctx); err != nil {
		return runtimev1.SecretResolveResponse{}, routeError(err)
	}
	resolver, err := secrets.NewResolver(authority, r.source)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, routeError(err)
	}
	adapter, err := secretsruntime.NewAdapter(resolver)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, routeError(err)
	}
	return adapter.Resolve(ctx, attempt, req, requestID)
}

// routeError maps authority failures to the contract. Credential, grant and
// scope problems never disclose which of them applied.
func routeError(err error) *runtimev1.ContractError {
	switch {
	case errors.Is(err, runtimeauth.ErrAttemptRevoked):
		return secretsruntime.Translate(secrets.ErrAttemptInactive)
	case errors.Is(err, runtimeauth.ErrForbidden):
		return &runtimev1.ContractError{Code: domain.CodeForbidden, Message: "the action is not granted"}
	case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound), errors.Is(err, runtimeauth.ErrInvalidRequest):
		return &runtimev1.ContractError{Code: domain.CodeInvalidCredential, Message: "the credential is not valid"}
	case errors.Is(err, errEnvironmentRefused):
		return secretsruntime.Translate(secrets.ErrNotFound)
	}
	return &runtimev1.ContractError{Code: domain.CodeUnavailable, Message: "the secret could not be delivered"}
}

// routeAuthority is the secrets module's AttemptAuthority for one request: it
// re-reads the durable grant and the attempt's current state every time it is
// asked, and answers from the grant's immutable pinned context.
type routeAuthority struct {
	r      *SecretRoute
	route  domain.AttemptID
	claims runtimeauth.TokenClaims
}

var _ secrets.AttemptAuthority = routeAuthority{}

// Grant implements secrets.AttemptAuthority. The attempt must be the route's
// attempt; anything else has no authority.
func (a routeAuthority) Grant(ctx context.Context, attempt domain.AttemptID) (secrets.AttemptGrant, error) {
	if attempt != a.route {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	g, err := a.check(ctx)
	switch {
	case err == nil:
		return g, nil
	case errors.Is(err, runtimeauth.ErrAttemptRevoked), errors.Is(err, runtimeauth.ErrForbidden):
		return secrets.AttemptGrant{}, secrets.ErrAttemptInactive
	case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound), errors.Is(err, runtimeauth.ErrInvalidRequest):
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	return secrets.AttemptGrant{}, err
}

// check applies every runtime authority check for secret.resolve and builds
// the grant from the durable record.
func (a routeAuthority) check(ctx context.Context) (secrets.AttemptGrant, error) {
	var out secrets.AttemptGrant
	var pin *runtimev1.EnvironmentPin
	var declared []runtimev1.SecretBinding
	err := a.r.store.Do(ctx, a.claims.Tenant, func(ctx context.Context, tx runtimeauth.GrantTx) error {
		g, _, err := runtimeauth.CheckSession(ctx, tx, runtimeauth.SessionCheck{
			Route: a.route, Session: a.claims.Session, TokenGeneration: a.claims.Generation,
			CurrentGeneration: a.r.gens.Generation(), Action: runtimeauth.ActionSecretResolve, Now: a.r.clock(),
		})
		if err != nil {
			return err
		}
		if g.Scope.TenantID != a.claims.Tenant || g.AttemptID != a.route {
			return errors.Join(runtimeauth.ErrInvalidCredential, errors.New("token of another tenant or attempt"))
		}
		pin, declared, err = pinnedBindings(g)
		if err != nil {
			return err
		}
		out = secrets.AttemptGrant{Scope: g.Scope, RunID: g.RunID, AttemptID: g.AttemptID}
		return nil
	})
	if err != nil {
		return secrets.AttemptGrant{}, err
	}
	// An attempt that pins no environment resolves the bindings its own step
	// declared. Admission validated each reference (a project secret the actor may
	// read, with a current version) and pinned its version in the grant's
	// immutable, digest-checked context, so the declared list is the whole of what
	// the step may receive: an undeclared reference or another version is refused
	// by the resolver, and a revoked or finished attempt by the authority check.
	if pin == nil {
		out.Bindings = append([]runtimev1.SecretBinding{}, declared...)
		return out, nil
	}
	// The snapshot is immutable and is read after the authority check, outside
	// the grant transaction (it makes its own scoped read). An attempt that pins
	// an environment resolves that environment's own credentials.
	bindings, err := a.environmentBindings(ctx, out.Scope, *pin, declared)
	if err != nil {
		return secrets.AttemptGrant{}, err
	}
	out.Bindings = bindings
	return out, nil
}

// errEnvironmentRefused marks a pinned environment whose retained snapshot is
// missing, foreign, altered or not the pinned one: nothing resolves.
var errEnvironmentRefused = errors.New("the pinned environment snapshot is not available")

// environmentBindings keeps the pinned context's bindings that the environment's
// retained snapshot names exactly. The snapshot is read in the grant's scope by
// the pin; it must be intact, belong to that scope and carry that pin.
func (a routeAuthority) environmentBindings(ctx context.Context, scope domain.Scope, pin runtimev1.EnvironmentPin, declared []runtimev1.SecretBinding) ([]runtimev1.SecretBinding, error) {
	snap, err := a.r.pins.GetSnapshot(ctx, scope, pin.ID, pin.ConfigDigest)
	switch {
	case err == nil:
	case errors.Is(err, delivery.ErrNotFound), errors.Is(err, delivery.ErrDenied), errors.Is(err, delivery.ErrInvalid):
		return nil, errEnvironmentRefused
	default:
		return nil, errors.Join(runtimeauth.ErrUnavailable, err)
	}
	if snap.Scope != scope || snap.Pin() != pin || snap.Validate() != nil {
		return nil, errEnvironmentRefused
	}
	named := make(map[runtimev1.SecretBinding]bool, len(snap.Secrets))
	byName := make(map[string]runtimev1.SecretBinding, len(snap.Secrets))
	byRef := make(map[string]runtimev1.SecretBinding, len(snap.Secrets))
	for _, b := range snap.Secrets {
		named[b] = true
		byName[b.Name] = b
		byRef[b.Ref] = b
	}
	if !a.r.stepUnion {
		out := []runtimev1.SecretBinding{}
		for _, b := range declared {
			if named[b] {
				out = append(out, b)
			}
		}
		return out, nil
	}
	// Under VS-15 (stepUnion), every credential of the retained environment
	// snapshot must be present unaltered in the grant's digest-checked context,
	// and step-declared bindings compose without name/ref collisions.
	seenEnv := 0
	out := make([]runtimev1.SecretBinding, 0, len(declared))
	seen := make(map[string]runtimev1.SecretBinding, len(declared))
	seenRef := make(map[string]runtimev1.SecretBinding, len(declared))
	for _, b := range declared {
		if envB, ok := byName[b.Name]; ok && envB != b {
			return nil, errEnvironmentRefused
		}
		if envB, ok := byRef[b.Ref]; ok && envB != b {
			return nil, errEnvironmentRefused
		}
		if prev, ok := seen[b.Name]; ok {
			if prev != b {
				return nil, errEnvironmentRefused
			}
			continue
		}
		if prev, ok := seenRef[b.Ref]; ok {
			if prev != b {
				return nil, errEnvironmentRefused
			}
			continue
		}
		if named[b] {
			seenEnv++
		}
		seen[b.Name] = b
		seenRef[b.Ref] = b
		out = append(out, b)
	}
	if seenEnv != len(snap.Secrets) {
		return nil, errEnvironmentRefused
	}
	return out, nil
}

// pinnedBindings returns the environment pin and the secret bindings of the grant's immutable pinned
// context after proving that the document is the one the grant registered and
// that it names exactly this scope, run and attempt. An inconsistent record is
// an outage, never a different grant.
func pinnedBindings(g runtimeauth.AdmissionGrant) (*runtimev1.EnvironmentPin, []runtimev1.SecretBinding, error) {
	sum := sha256.Sum256(g.Context)
	if !g.ContextDigest.Equal(runtimeauth.Digest(sum)) {
		return nil, nil, errors.Join(runtimeauth.ErrUnavailable, errors.New("the pinned context does not match its digest"))
	}
	c, err := runtimev1.DecodeRequest[runtimev1.Context](g.Context)
	if err != nil {
		return nil, nil, errors.Join(runtimeauth.ErrUnavailable, errors.New("the pinned context is unreadable"))
	}
	if c.Scope() != g.Scope || c.RunID != g.RunID || c.AttemptID != g.AttemptID {
		return nil, nil, errors.Join(runtimeauth.ErrUnavailable, errors.New("the pinned context belongs to another attempt"))
	}
	var pin *runtimev1.EnvironmentPin
	if c.Environment != nil {
		p := *c.Environment
		pin = &p
	}
	return pin, append([]runtimev1.SecretBinding(nil), c.Secrets...), nil
}
