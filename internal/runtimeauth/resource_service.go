package runtimeauth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

func (c *ResourceConfig) validate() error {
	switch {
	case c.Store == nil:
		return errors.New("runtimeauth: a resource grant store is required")
	case c.State == nil:
		return errors.New("runtimeauth: a resource state observer is required (no allow-all fallback)")
	case c.Workloads == nil:
		return errors.New("runtimeauth: a resource workload observer is required (no allow-all fallback)")
	case c.Tokens == nil || c.Verifier == nil:
		return errors.New("runtimeauth: resource token issuance and verification are required")
	}
	return nil
}

func (s *Service) resources() (*ResourceConfig, error) {
	if s.cfg.Resources == nil {
		return nil, fail(ErrUnavailable, "run-resource authority is not configured")
	}
	return s.cfg.Resources, nil
}

func (s *Service) resourceWrapper(rc *ResourceConfig) WrapperPolicy {
	if rc.Wrapper != nil {
		return rc.Wrapper
	}
	return s.cfg.Wrapper
}

// logResourceOp records one operation. Only the operation, validated run and key
// identifiers and the classified outcome are written.
func (s *Service) logResourceOp(ctx context.Context, op string, ref ResourceRef, err error) {
	level := slog.LevelInfo
	if err != nil && outcome(err) == "unavailable" {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{slog.String("op", op), slog.String("outcome", outcome(err))}
	if ref.RunID.Validate() == nil {
		attrs = append(attrs, slog.String("runId", string(ref.RunID)))
	}
	if _, kerr := ResourceKindOf(ref.Key); kerr == nil {
		attrs = append(attrs, slog.String("resourceKey", ref.Key))
	}
	s.cfg.Logger.LogAttrs(ctx, level, "runtime resource authority", attrs...)
}

// ResourceRegisterCommand is the trusted adapter's registration of a resource
// workload's admission credential (S5).
type ResourceRegisterCommand struct {
	Scope           domain.Scope
	RunID           domain.RunID
	Key             string
	Credential      Credential
	SpecFingerprint string
	// ExpiresAt is the resource's deadline: the credential never admits after it
	// and no token or session outlives it. It must not be later than the deadline
	// execution committed.
	ExpiresAt time.Time
}

// RegisterResource idempotently stores the digest of a generated credential
// together with the resource's immutable authority. The committed state decides
// what is registrable: the resource must be active and its fingerprint must be
// the one the adapter registers for; the caller supplies only the credential, the
// fingerprint it built the workload from and the deadline.
//
// A retry with the same credential and content (after a lost response) returns
// without extending anything. Any contradicting registration is ErrConflict and
// replaces nothing.
func (s *Service) RegisterResource(ctx context.Context, cmd ResourceRegisterCommand) (err error) {
	ref := ResourceRef{Scope: cmd.Scope, RunID: cmd.RunID, Key: cmd.Key}
	defer func() { err = Classify(err); s.logResourceOp(ctx, "register_resource", ref, err) }()
	rc, err := s.resources()
	if err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if cmd.Credential.IsZero() {
		return fail(ErrInvalidRequest, "a credential is required")
	}
	if !fingerprintPattern.MatchString(cmd.SpecFingerprint) {
		return fail(ErrInvalidRequest, "a resource spec fingerprint is required")
	}
	generation := rc.Tokens.Generation()
	return rc.Store.DoResource(ctx, cmd.Scope.TenantID, func(ctx context.Context, tx ResourceTx) error {
		state, err := tx.LockResource(ctx, ref)
		if err != nil {
			return err
		}
		if err := state.Validate(); err != nil {
			return err
		}
		if state.Scope != cmd.Scope || state.RunID != cmd.RunID || state.Key != cmd.Key {
			return fail(ErrConflict, "the resource does not match the registration")
		}
		if state.Phase != ResourceActive {
			return fail(ErrResourceRevoked, "the resource is no longer authorized")
		}
		if state.Fingerprint != cmd.SpecFingerprint {
			return fail(ErrConflict, "the fingerprint is not the committed resource's")
		}
		if micros(cmd.ExpiresAt).After(micros(state.DeadlineAt)) {
			return fail(ErrConflict, "the expiry is later than the committed deadline")
		}
		g, err := NewResourceGrant(ResourceProvision{
			Ref: ref, CredentialDigest: cmd.Credential.Digest(), Generation: generation,
			SpecFingerprint: cmd.SpecFingerprint, DeadlineAt: cmd.ExpiresAt,
		}, s.now(), s.cfg.Policy)
		if err != nil {
			return err
		}
		stored, inserted, err := tx.InsertResourceGrant(ctx, g)
		if err != nil {
			return err
		}
		if !inserted {
			if !stored.SameRegistration(g) {
				return fail(ErrConflict, "a different registration exists for the resource")
			}
			if stored.Revoked() {
				return fail(ErrResourceRevoked, "the resource is no longer authorized")
			}
		}
		return nil
	})
}

// verifyResource verifies a bearer of the resource audience. A genuine attempt
// token is forbidden (the audiences differ), never accepted.
func (s *Service) verifyResource(ctx context.Context, rc *ResourceConfig, bearer string) (TokenClaims, error) {
	claims, err := rc.Verifier.VerifyResource(ctx, bearer)
	if err == nil {
		return claims, nil
	}
	if errors.Is(err, ErrInvalidCredential) {
		if _, aerr := s.cfg.Verifier.Verify(ctx, bearer); aerr == nil {
			return TokenClaims{}, fail(ErrForbidden, "the token is not valid for this route family")
		}
	}
	return TokenClaims{}, err
}

// verifyAttempt verifies a bearer of the attempt audience on a v2 attempt route.
// A genuine resource token is forbidden.
func (s *Service) verifyAttempt(ctx context.Context, bearer string) (TokenClaims, error) {
	claims, err := s.cfg.Verifier.Verify(ctx, bearer)
	if err == nil {
		return claims, nil
	}
	if errors.Is(err, ErrInvalidCredential) && s.cfg.Resources != nil {
		if _, rerr := s.cfg.Resources.Verifier.VerifyResource(ctx, bearer); rerr == nil {
			return TokenClaims{}, fail(ErrForbidden, "the token is not valid for this route family")
		}
	}
	return TokenClaims{}, err
}

func resourceTokenInfo(token string, exp, max time.Time) runtimev2.ResourceTokenInfo {
	return runtimev2.ResourceTokenInfo{
		Token: token, TokenType: "Bearer", Audience: runtimev2.ResourceAudience,
		ExpiresAt:    runtimev2.Timestamp(exp.UTC().Format(time.RFC3339Nano)),
		MaxExpiresAt: runtimev2.Timestamp(max.UTC().Format(time.RFC3339Nano)),
	}
}

// sameResource reports whether the locked state still describes the resource the
// grant was registered for and the workload it admitted.
func sameResource(st ResourceState, g ResourceGrant) bool {
	return st.Scope == g.Scope && st.RunID == g.RunID && st.Key == g.Key && st.Kind == g.Kind && st.Fingerprint == g.SpecFingerprint
}

// AdmitResource exchanges the one-time admission credential of a preparation or
// service workload for a resource token and its context.
//
// Sequence: restricted credential lookup; advisory read of the committed
// resource; the claimed workload is compared with the committed (name, bound UID)
// and, when a controller-owned Pod is claimed, the backend confirms it belongs to
// that UID, outside any transaction; then one tenant transaction that re-reads
// and locks the resource, compares it with what was observed, locks the grant,
// and consumes it. The token is returned only after the commit; a lost response
// is recovered by repeating the same credential and nonce.
func (s *Service) AdmitResource(ctx context.Context, route ResourceRef, req runtimev2.ResourceAdmitRequest) (resp runtimev2.ResourceAdmitResponse, err error) {
	defer func() { err = Classify(err); s.logResourceOp(ctx, "admit_resource", route, err) }()
	rc, err := s.resources()
	if err != nil {
		return resp, err
	}
	if err := req.Validate(); err != nil {
		return resp, failWith(ErrInvalidRequest, err, "the admission request is invalid")
	}
	if _, kerr := ResourceKindOf(route.Key); kerr != nil || route.RunID.Validate() != nil {
		return resp, fail(ErrInvalidCredential, "the credential is not valid for this resource")
	}
	cred, err := ParseCredential(req.AdmissionCredential)
	if err != nil {
		return resp, err
	}
	digest := cred.Digest()
	loc, err := rc.Store.LocateResource(ctx, digest)
	if err != nil {
		return resp, err
	}
	if loc.Ref.RunID != route.RunID || loc.Ref.Key != route.Key {
		return resp, fail(ErrInvalidCredential, "the credential is not valid for this resource")
	}
	ref := loc.Ref

	pre, err := rc.State.ObserveResource(ctx, ref)
	if err != nil {
		return resp, missing(err)
	}
	if err := pre.Validate(); err != nil {
		return resp, err
	}
	now := s.now()
	if pre.Phase != ResourceActive || !now.Before(micros(pre.DeadlineAt)) {
		return resp, fail(ErrResourceRevoked, "the resource is no longer authorized")
	}
	if !pre.Bound() {
		return resp, fail(ErrResourceUnbound, "the resource instance is not bound yet")
	}
	ident := IdentityOf(req.Identity)
	if ident.Name != pre.WorkloadName || ident.UID != pre.WorkloadUID {
		return resp, fail(ErrForbidden, "the identity does not match the committed resource")
	}
	if ident.HasPod() {
		err := rc.Workloads.VerifyPod(ctx, ResourceWorkload{Ref: ref, Name: pre.WorkloadName, UID: pre.WorkloadUID}, ident.PodName, ident.PodUID)
		switch {
		case err == nil:
		case errors.Is(err, ErrNotObserved), errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound):
			return resp, fail(ErrForbidden, "the pod does not belong to the workload")
		default:
			return resp, failWith(ErrUnavailable, err, "the workload could not be observed")
		}
	}

	var denied error
	err = rc.Store.DoResource(ctx, ref.Scope.TenantID, func(ctx context.Context, tx ResourceTx) error {
		denied = nil
		state, err := tx.LockResource(ctx, ref)
		if err != nil {
			return missing(err)
		}
		if err := state.Validate(); err != nil {
			return err
		}
		g, err := tx.ResourceGrantByCredential(ctx, digest, LockUpdate)
		if err != nil {
			return missing(err)
		}
		if g.Ref() != ref {
			return fail(ErrInvalidCredential, "the credential is not valid for this resource")
		}
		now := s.now()
		revoke := func(reason error) error {
			if g.Revoke(now) {
				if err := tx.SaveResourceRevocation(ctx, g); err != nil {
					return err
				}
			}
			denied = reason
			return nil
		}
		if state.Phase != ResourceActive || !now.Before(micros(state.DeadlineAt)) {
			return revoke(fail(ErrResourceRevoked, "the resource is no longer authorized"))
		}
		if !sameResource(state, g) {
			return revoke(fail(ErrResourceRevoked, "the resource changed since the credential was registered"))
		}
		if state.WorkloadName != pre.WorkloadName || state.WorkloadUID != pre.WorkloadUID || state.Fingerprint != pre.Fingerprint {
			return fail(ErrUnavailable, "the resource changed during the observation; retry")
		}
		if g.Generation != rc.Tokens.Generation() {
			denied = fail(ErrInvalidCredential, "the credential is not valid")
			return nil
		}
		if err := s.resourceWrapper(rc).Check(req.ClientVersion, ""); err != nil {
			denied = err
			return nil
		}
		session, err := NewSessionID(s.cfg.Random)
		if err != nil {
			return err
		}
		adm, err := g.Consume(NonceDigest(req.Nonce), ident, session, now, s.cfg.Policy)
		if err != nil {
			denied = err
			return nil
		}
		if !adm.Replayed {
			if err := tx.SaveResourceConsumption(ctx, g); err != nil {
				return err
			}
		}
		exp, err := g.TokenExpiry(now, s.cfg.Policy.TokenTTL)
		if err != nil {
			denied = err
			return nil
		}
		token, err := rc.Tokens.IssueResource(IssueRequest{
			Tenant: g.Scope.TenantID, Session: adm.SessionID, Generation: g.Generation, IssuedAt: now, ExpiresAt: exp,
		})
		if err != nil {
			return err // rolls back the consumption: nothing was handed out
		}
		resp = runtimev2.ResourceAdmitResponse{Token: resourceTokenInfo(token, exp, adm.MaxExpiresAt), Context: state.Context}
		return nil
	})
	if err != nil {
		return runtimev2.ResourceAdmitResponse{}, err
	}
	if denied != nil {
		return runtimev2.ResourceAdmitResponse{}, denied
	}
	if err := resp.Validate(); err != nil {
		return runtimev2.ResourceAdmitResponse{}, failWith(ErrUnavailable, err, "the response is not a valid admission")
	}
	return resp, nil
}

// ResourceAuthorizeRequest is a bearer's request to perform an operation as the
// preparation or service named by the route.
type ResourceAuthorizeRequest struct {
	RunID  domain.RunID
	Key    string
	Bearer string
	Op     ResourceOp
}

// ResourceAuthorization is the verified authority of a resource workload for one
// operation. Consumers use the identifiers and the committed context it carries,
// never ones from the request.
type ResourceAuthorization struct {
	Ref     ResourceRef
	Kind    runtimev2.ResourceKind
	Session SessionID
	Op      ResourceOp
	// WorkloadUID is the admitted, committed instance.
	WorkloadUID string
	// TokenGeneration is the signing generation of the verified token (see
	// CheckResourceSession).
	TokenGeneration int64
	// Context is the committed assignment (pinned secret bindings of a service,
	// the pinned commit of a preparation).
	Context runtimev2.ResourceContext
}

// Bindings returns the secret bindings a service may resolve: exactly its own
// pinned ones. A preparation has none.
func (a ResourceAuthorization) Bindings() []runtimev2.SecretBinding {
	if a.Context.Service == nil {
		return nil
	}
	return append([]runtimev2.SecretBinding(nil), a.Context.Service.Secrets...)
}

// CheckResourceSession applies every resource authority check inside the
// caller's transaction and in the module-wide lock order: the resource's
// execution state is re-read and locked first, then the grant FOR SHARE. It
// checks the session, the route's resource, the signing generation, the
// committed phase, fingerprint and bound workload, and the session lifetime. The
// resource's deadline is part of the session maximum.
func CheckResourceSession(ctx context.Context, tx ResourceTx, route ResourceRef, session SessionID, tokenGen, curGen int64, now time.Time) (ResourceGrant, ResourceState, error) {
	first, err := tx.ResourceGrantBySession(ctx, session, LockNone)
	if err != nil {
		return ResourceGrant{}, ResourceState{}, missing(err)
	}
	if first.RunID != route.RunID || first.Key != route.Key {
		// A genuine token of another resource: foreign authority, forbidden.
		return ResourceGrant{}, ResourceState{}, fail(ErrForbidden, "the token is not valid for this resource")
	}
	state, err := tx.LockResource(ctx, first.Ref())
	if err != nil {
		return ResourceGrant{}, ResourceState{}, missing(err)
	}
	if err := state.Validate(); err != nil {
		return ResourceGrant{}, ResourceState{}, err
	}
	g, err := tx.ResourceGrantBySession(ctx, session, LockShare)
	if err != nil {
		return ResourceGrant{}, ResourceState{}, missing(err)
	}
	if g.Ref() != first.Ref() {
		return ResourceGrant{}, ResourceState{}, fail(ErrUnavailable, "the grant changed during the check")
	}
	if tokenGen != g.Generation || g.Generation != curGen {
		return ResourceGrant{}, ResourceState{}, fail(ErrInvalidCredential, "the token's signing generation is retired")
	}
	if g.Revoked() || state.Phase != ResourceActive || !micros(now).Before(micros(state.DeadlineAt)) ||
		!sameResource(state, g) || g.Consumption == nil {
		return ResourceGrant{}, ResourceState{}, fail(ErrResourceRevoked, "the resource is no longer authorized")
	}
	w := g.Consumption.Workload
	if !state.Bound() || state.WorkloadUID != w.UID || state.WorkloadName != w.Name {
		// The committed instance is not the admitted one (replaced or rebound).
		return ResourceGrant{}, ResourceState{}, fail(ErrResourceRevoked, "the admitted instance is not the committed one")
	}
	if err := g.Authorize(now); err != nil {
		return ResourceGrant{}, ResourceState{}, err
	}
	return g, state, nil
}

func (s *Service) withResourceSession(ctx context.Context, rc *ResourceConfig, route ResourceRef, bearer string, op ResourceOp, fn func(g ResourceGrant, st ResourceState, now time.Time) error) (ResourceAuthorization, error) {
	var out ResourceAuthorization
	claims, err := s.verifyResource(ctx, rc, bearer)
	if err != nil {
		return out, err
	}
	kind, err := ResourceKindOf(route.Key)
	if err != nil || route.RunID.Validate() != nil {
		return out, fail(ErrInvalidCredential, "the token is not valid for this resource")
	}
	if !OpAllowed(kind, op) {
		return out, fail(ErrForbidden, "the operation is not granted to this kind of resource")
	}
	err = rc.Store.DoResource(ctx, claims.Tenant, func(ctx context.Context, tx ResourceTx) error {
		now := s.now()
		g, st, err := CheckResourceSession(ctx, tx, route, claims.Session, claims.Generation, rc.Tokens.Generation(), now)
		if err != nil {
			return err
		}
		if g.Scope.TenantID != claims.Tenant {
			return fail(ErrInvalidCredential, "the token is not valid for this resource")
		}
		out = ResourceAuthorization{Ref: g.Ref(), Kind: g.Kind, Session: claims.Session, Op: op, WorkloadUID: g.Consumption.Workload.UID, TokenGeneration: claims.Generation, Context: st.Context}
		if fn != nil {
			return fn(g, st, now)
		}
		return nil
	})
	if err != nil {
		return ResourceAuthorization{}, err
	}
	return out, nil
}

// AuthorizeResource decides whether the bearer may perform req.Op as the
// resource named by the route now. It never widens: the operation lists of
// OpAllowed are closed per kind, and a token of another resource is forbidden.
func (s *Service) AuthorizeResource(ctx context.Context, req ResourceAuthorizeRequest) (a ResourceAuthorization, err error) {
	route := ResourceRef{RunID: req.RunID, Key: req.Key}
	defer func() { err = Classify(err); s.logResourceOp(ctx, "authorize_resource", route, err) }()
	rc, err := s.resources()
	if err != nil {
		return a, err
	}
	return s.withResourceSession(ctx, rc, route, req.Bearer, req.Op, nil)
}

// RenewResource issues a fresh token for a resource that is still authorized.
// The expiry is clamped to the session maximum, itself never beyond the
// resource's deadline; a renewal never moves it.
func (s *Service) RenewResource(ctx context.Context, run domain.RunID, key, bearer string) (resp runtimev2.ResourceTokenResponse, err error) {
	route := ResourceRef{RunID: run, Key: key}
	defer func() { err = Classify(err); s.logResourceOp(ctx, "renew_resource", route, err) }()
	rc, err := s.resources()
	if err != nil {
		return resp, err
	}
	_, err = s.withResourceSession(ctx, rc, route, bearer, OpResourceRenew, func(g ResourceGrant, _ ResourceState, now time.Time) error {
		exp, err := g.TokenExpiry(now, s.cfg.Policy.TokenTTL)
		if err != nil {
			return err
		}
		token, err := rc.Tokens.IssueResource(IssueRequest{
			Tenant: g.Scope.TenantID, Session: g.Consumption.SessionID, Generation: g.Generation, IssuedAt: now, ExpiresAt: exp,
		})
		if err != nil {
			return err
		}
		resp = runtimev2.ResourceTokenResponse{Token: resourceTokenInfo(token, exp, g.Consumption.MaxExpiresAt)}
		return nil
	})
	if err != nil {
		return runtimev2.ResourceTokenResponse{}, err
	}
	if err := resp.Validate(); err != nil {
		return runtimev2.ResourceTokenResponse{}, failWith(ErrUnavailable, err, "the response is not a valid token")
	}
	return resp, nil
}

// ResourceContext returns the committed context of the resource, unchanged.
func (s *Service) ResourceContext(ctx context.Context, run domain.RunID, key, bearer string) (c runtimev2.ResourceContext, err error) {
	route := ResourceRef{RunID: run, Key: key}
	defer func() { err = Classify(err); s.logResourceOp(ctx, "resource_context", route, err) }()
	rc, err := s.resources()
	if err != nil {
		return c, err
	}
	a, err := s.withResourceSession(ctx, rc, route, bearer, OpResourceContext, nil)
	if err != nil {
		return runtimev2.ResourceContext{}, err
	}
	return a.Context, nil
}

// RevokeResource irreversibly denies the resource's grant. It is idempotent and
// makes no claim about what the workload already did: it ends authority to use
// Kodama's interfaces. Execution normally revokes inside its own stop
// transaction through the adapter's RevokeResource; the committed state denies
// every call anyway, so this entry point serves callers without one.
func (s *Service) RevokeResource(ctx context.Context, ref ResourceRef) (err error) {
	defer func() { err = Classify(err); s.logResourceOp(ctx, "revoke_resource", ref, err) }()
	rc, err := s.resources()
	if err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	return rc.Store.DoResource(ctx, ref.Scope.TenantID, func(ctx context.Context, tx ResourceTx) error {
		_, err := tx.RevokeResource(ctx, ref, s.now())
		return err
	})
}

// RevokeRunResources revokes every resource grant of a run (cancellation or a
// terminal run).
func (s *Service) RevokeRunResources(ctx context.Context, scope domain.Scope, run domain.RunID) (err error) {
	ref := ResourceRef{Scope: scope, RunID: run}
	defer func() { err = Classify(err); s.logResourceOp(ctx, "revoke_run_resources", ref, err) }()
	rc, err := s.resources()
	if err != nil {
		return err
	}
	if err := firstErr(scope.Validate(), run.Validate()); err != nil {
		return fail(ErrInvalidRequest, "%v", err)
	}
	return rc.Store.DoResource(ctx, scope.TenantID, func(ctx context.Context, tx ResourceTx) error {
		_, err := tx.RevokeRun(ctx, scope, run, s.now())
		return err
	})
}

// ReauthorizeResource re-verifies, now and in one tenant transaction, that the
// session of a previous resource authorization is still current (see Reauthorize):
// the grant is not revoked, the committed phase, fingerprint, bound instance and
// deadline still hold, and the operation is still the kind's. A handler calls it
// after reading a request body and immediately before a durable mutation.
func (s *Service) ReauthorizeResource(ctx context.Context, a ResourceAuthorization) (err error) {
	defer func() { err = Classify(err); s.logResourceOp(ctx, "reauthorize_resource", a.Ref, err) }()
	rc, err := s.resources()
	if err != nil {
		return err
	}
	if a.Session.Validate() != nil || a.Ref.Validate() != nil || !OpAllowed(a.Kind, a.Op) {
		return fail(ErrInvalidCredential, "the authorization is malformed")
	}
	return rc.Store.DoResource(ctx, a.Ref.Scope.TenantID, func(ctx context.Context, tx ResourceTx) error {
		g, st, err := CheckResourceSession(ctx, tx, a.Ref, a.Session, a.TokenGeneration, rc.Tokens.Generation(), s.now())
		if err != nil {
			return err
		}
		if g.Consumption.Workload.UID != a.WorkloadUID || st.Fingerprint != g.SpecFingerprint {
			return fail(ErrResourceRevoked, "the admitted instance is not the committed one")
		}
		return nil
	})
}
