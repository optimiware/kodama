package runtimeauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// PinContext returns the canonical document and digest of a pinned context.
// The execution owner uses the same function to compute
// AttemptState.ContextDigest, so the grant and the execution state reference
// byte-identical content.
func PinContext(c runtimev1.Context) (Digest, []byte, error) {
	if err := c.Validate(); err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context is invalid: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context cannot be encoded")
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context cannot be canonicalized")
	}
	if len(canonical) > MaxContextBytes {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context is too large")
	}
	return sha256.Sum256(canonical), canonical, nil
}

func decodeContext(doc []byte) (runtimev1.Context, error) {
	c, err := runtimev1.DecodeRequest[runtimev1.Context](doc)
	if err != nil {
		return runtimev1.Context{}, failWith(ErrUnavailable, err, "the stored context is unreadable")
	}
	return c, nil
}

// PinContextV2 is PinContext for the run_shared wire: the canonical document and
// digest of a ContextV2. The execution owner computes AttemptState.ContextDigest
// the same way (SHA-256 of the canonical JSON), so the grant and the execution
// state reference byte-identical content.
func PinContextV2(c runtimev2.ContextV2) (Digest, []byte, error) {
	if err := c.Validate(); err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context is invalid: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context cannot be encoded")
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context cannot be canonicalized")
	}
	if len(canonical) > MaxContextBytes {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context is too large")
	}
	return sha256.Sum256(canonical), canonical, nil
}

// PinContextV3 is PinContext for the source-set wire: the canonical document and
// digest of a ContextV3. The execution owner computes AttemptState.ContextDigest
// the same way, so the grant and the execution state reference byte-identical
// content.
func PinContextV3(c runtimev3.ContextV3) (Digest, []byte, error) {
	if err := c.Validate(); err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context is invalid: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context cannot be encoded")
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context cannot be canonicalized")
	}
	if len(canonical) > MaxContextBytes {
		return Digest{}, nil, fail(ErrInvalidRequest, "the context is too large")
	}
	return sha256.Sum256(canonical), canonical, nil
}

func decodeContextV3(doc []byte) (runtimev3.ContextV3, error) {
	c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](doc)
	if err != nil {
		return runtimev3.ContextV3{}, failWith(ErrUnavailable, err, "the stored context is unreadable")
	}
	return c, nil
}

func decodeContextV2(doc []byte) (runtimev2.ContextV2, error) {
	c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](doc)
	if err != nil {
		return runtimev2.ContextV2{}, failWith(ErrUnavailable, err, "the stored context is unreadable")
	}
	return c, nil
}

// The runtime wires of an attempt: v1 serves attempt_private contexts, v2
// serves run_shared ContextV2 documents. A grant belongs to exactly one wire,
// fixed by its pinned context.
const (
	WireV1 int64 = 1
	WireV2 int64 = 2
	// WireV3 serves run_shared contexts of a source set (ContextV3, T-150): the
	// same attempt token and grant rules, plus the Git credential delivery and the
	// runtime-issued Git grants.
	WireV3 int64 = 3
)

// WireOf returns the runtime wire of a pinned context document: 3 for a
// ContextV3 (contextVersion 3), 2 for a ContextV2 (contextVersion 2), 1 for every
// other document. It only reads the discriminator; the strict decoding of the
// matching version follows.
func WireOf(doc []byte) int64 {
	var v struct {
		ContextVersion int64 `json:"contextVersion"`
	}
	if json.Unmarshal(doc, &v) == nil {
		switch v.ContextVersion {
		case runtimev3.ContextVersion:
			return WireV3
		case runtimev2.ContextVersion:
			return WireV2
		}
	}
	return WireV1
}

// Wire returns the runtime wire the grant's attempt belongs to.
func (g AdmissionGrant) Wire() int64 { return WireOf(g.Context) }

// RegisterCommand is the trusted executor's registration of an attempt's
// admission credential.
type RegisterCommand struct {
	Scope      domain.Scope
	AttemptID  domain.AttemptID
	Credential Credential
	// Actions is the immutable ceiling of the attempt's workload.
	Actions ActionSet
	// Context is the immutable pinned context the entrypoint will receive.
	Context runtimev1.Context
}

// Registration is the result of Register.
type Registration struct {
	// ExpiresAt is the end of the first-admission window, fixed by the first
	// successful registration.
	ExpiresAt time.Time
	// Created is false when an identical registration already existed.
	Created bool
}

// Register idempotently stores the digest of a generated credential together
// with the attempt's immutable authority. Ownership, the Job binding, the
// pinned context and the authority revision come from the attempt's locked
// execution state, not from the caller; the caller supplies only the credential
// and the action ceiling, which must not exceed what the attempt allows.
//
// A retry with the same credential and content (after a lost response)
// returns the original registration without extending its expiry. Any
// contradicting registration is ErrConflict and replaces nothing.
func (s *Service) Register(ctx context.Context, cmd RegisterCommand) (reg Registration, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "register", cmd.AttemptID, err) }()
	if err := firstErr(cmd.Scope.Validate(), cmd.AttemptID.Validate()); err != nil {
		return Registration{}, fail(ErrInvalidRequest, "%v", err)
	}
	if cmd.Context.Scope() != cmd.Scope || cmd.Context.AttemptID != cmd.AttemptID {
		return Registration{}, fail(ErrInvalidRequest, "the context belongs to another scope or attempt")
	}
	contextDigest, doc, err := PinContext(cmd.Context)
	if err != nil {
		return Registration{}, err
	}
	return s.register(ctx, cmd.Scope, cmd.AttemptID, cmd.Context.RunID, cmd.Credential, cmd.Actions, contextDigest, doc)
}

// RegisterCommandV2 is Register for a run_shared attempt: the pinned context is
// the ContextV2 the entrypoint receives from /runtime/v2.
type RegisterCommandV2 struct {
	Scope      domain.Scope
	AttemptID  domain.AttemptID
	Credential Credential
	Actions    ActionSet
	Context    runtimev2.ContextV2
}

// RegisterV2 registers the admission credential of a run_shared attempt with
// exactly the semantics of Register. The grant is bound to the v2 wire: the v1
// routes refuse it with context_version_mismatch and the reverse.
func (s *Service) RegisterV2(ctx context.Context, cmd RegisterCommandV2) (reg Registration, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "register", cmd.AttemptID, err) }()
	if err := firstErr(cmd.Scope.Validate(), cmd.AttemptID.Validate()); err != nil {
		return Registration{}, fail(ErrInvalidRequest, "%v", err)
	}
	if (domain.Scope{TenantID: cmd.Context.TenantID, ProjectID: cmd.Context.ProjectID}) != cmd.Scope || cmd.Context.AttemptID != cmd.AttemptID {
		return Registration{}, fail(ErrInvalidRequest, "the context belongs to another scope or attempt")
	}
	contextDigest, doc, err := PinContextV2(cmd.Context)
	if err != nil {
		return Registration{}, err
	}
	return s.register(ctx, cmd.Scope, cmd.AttemptID, cmd.Context.RunID, cmd.Credential, cmd.Actions, contextDigest, doc)
}

// RegisterCommandV3 is Register for a source-set attempt: the pinned context is
// the ContextV3 the entrypoint receives from /runtime/v3.
type RegisterCommandV3 struct {
	Scope      domain.Scope
	AttemptID  domain.AttemptID
	Credential Credential
	Actions    ActionSet
	Context    runtimev3.ContextV3
}

// RegisterV3 registers the admission credential of a source-set attempt with
// exactly the semantics of Register. The grant is bound to the v3 wire: the v1
// and v2 routes refuse it with context_version_mismatch and the reverse.
func (s *Service) RegisterV3(ctx context.Context, cmd RegisterCommandV3) (reg Registration, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "register", cmd.AttemptID, err) }()
	if err := firstErr(cmd.Scope.Validate(), cmd.AttemptID.Validate()); err != nil {
		return Registration{}, fail(ErrInvalidRequest, "%v", err)
	}
	if cmd.Context.Scope() != cmd.Scope || cmd.Context.AttemptID != cmd.AttemptID {
		return Registration{}, fail(ErrInvalidRequest, "the context belongs to another scope or attempt")
	}
	contextDigest, doc, err := PinContextV3(cmd.Context)
	if err != nil {
		return Registration{}, err
	}
	return s.register(ctx, cmd.Scope, cmd.AttemptID, cmd.Context.RunID, cmd.Credential, cmd.Actions, contextDigest, doc)
}

func (s *Service) register(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, run domain.RunID, cred Credential, actions ActionSet, contextDigest Digest, doc []byte) (reg Registration, err error) {
	if cred.IsZero() {
		return Registration{}, fail(ErrInvalidRequest, "a credential is required")
	}
	if actions.IsZero() {
		return Registration{}, fail(ErrInvalidRequest, "a grant needs at least one action")
	}
	generation := s.cfg.Tokens.Generation()

	err = s.cfg.Store.Do(ctx, scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		state, err := tx.LockAttempt(ctx, scope, attempt)
		if err != nil {
			return err
		}
		if err := state.Validate(); err != nil {
			return err
		}
		if state.Scope != scope || state.AttemptID != attempt || state.RunID != run {
			return fail(ErrConflict, "the attempt does not match the registration")
		}
		if state.Phase != AttemptActive {
			return fail(ErrAttemptRevoked, "the attempt is no longer authorized")
		}
		if !state.ContextDigest.Equal(contextDigest) {
			return fail(ErrConflict, "the context is not the attempt's pinned context")
		}
		if !actions.SubsetOf(state.Actions) {
			return fail(ErrForbidden, "the actions exceed what the attempt allows")
		}
		g, err := NewAdmissionGrant(Provision{
			Scope: scope, RunID: state.RunID, AttemptID: attempt, Job: state.Job,
			CredentialDigest: cred.Digest(), Generation: generation, Actions: actions,
			AuthorityRevision: state.Revision, ContextDigest: contextDigest, Context: doc,
		}, s.now(), s.cfg.Policy)
		if err != nil {
			return err
		}
		stored, inserted, err := tx.InsertGrant(ctx, g)
		if err != nil {
			return err
		}
		if !inserted {
			if !stored.SameRegistration(g) {
				return fail(ErrConflict, "a different registration exists for the attempt")
			}
			if stored.Revoked() {
				return fail(ErrAttemptRevoked, "the attempt is no longer authorized")
			}
		}
		reg = Registration{ExpiresAt: stored.ExpiresAt, Created: inserted}
		return nil
	})
	if err != nil {
		return Registration{}, err
	}
	return reg, nil
}

// verifyOwnership compares Kubernetes evidence with the authoritative Job and
// the claimed identity. The claim is evidence only: the namespace and Job name
// come from execution state, and the Pod must be controlled by that Job.
func verifyOwnership(expected ExpectedJob, claimed runtimev1.JobIdentity, ev JobEvidence) error {
	ok := claimed.Name == expected.Name &&
		ev.Namespace == expected.Namespace && ev.PodNamespace == expected.Namespace &&
		ev.JobName == expected.Name && ev.JobUID != "" && ev.JobUID == claimed.UID &&
		ev.PodName == claimed.PodName && ev.PodUID != "" && ev.PodUID == claimed.PodUID &&
		ev.PodOwnerUID == ev.JobUID
	if !ok {
		return fail(ErrForbidden, "the Job identity does not match the attempt")
	}
	return nil
}

// Admit exchanges the one-time admission credential for a runtime token and
// the immutable context.
//
// Sequence: restricted credential lookup; advisory read of the attempt; a
// Kubernetes ownership observation outside any transaction; then one tenant
// transaction that re-reads and locks the attempt, compares it with what was
// observed, locks the grant, and consumes it. The token is signed inside the
// transaction but returned only after the commit, so a response is never
// handed out for an unrecorded consumption. A lost response, a failed
// signature or an unknown commit outcome is recovered by repeating the same
// credential and nonce.
func (s *Service) Admit(ctx context.Context, route domain.AttemptID, req runtimev1.AdmitRequest) (resp runtimev1.AdmitResponse, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "admit", route, err) }()
	info, doc, err := s.admit(ctx, route, req, WireV1)
	if err != nil {
		return resp, err
	}
	c, err := decodeContext(doc)
	if err != nil {
		return resp, err
	}
	resp = runtimev1.AdmitResponse{Token: info, Context: c}
	if err := resp.Validate(); err != nil {
		return runtimev1.AdmitResponse{}, failWith(ErrUnavailable, err, "the response is not a valid admission")
	}
	return resp, nil
}

// AdmitV2 is Admit on the run_shared wire: the same exchange, the same one-time
// credential, replay and revocation rules and the unchanged attempt token
// (audience kodama-runtime), returning the ContextV2. An attempt of the other
// wire is ErrContextVersionMismatch with nothing consumed or revoked.
func (s *Service) AdmitV2(ctx context.Context, route domain.AttemptID, req runtimev1.AdmitRequest) (resp runtimev2.AdmitResponseV2, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "admit", route, err) }()
	info, doc, err := s.admit(ctx, route, req, WireV2)
	if err != nil {
		return resp, err
	}
	c, err := decodeContextV2(doc)
	if err != nil {
		return resp, err
	}
	resp = runtimev2.AdmitResponseV2{Token: info, Context: c}
	if err := resp.Validate(); err != nil {
		return runtimev2.AdmitResponseV2{}, failWith(ErrUnavailable, err, "the response is not a valid admission")
	}
	return resp, nil
}

// AdmitV3 is Admit on the source-set wire: the same exchange, one-time credential,
// replay and revocation rules and the unchanged attempt token (audience
// kodama-runtime), returning the ContextV3. An attempt of another wire is
// ErrContextVersionMismatch with nothing consumed or revoked. The Git credential
// is not part of the exchange: it is delivered to the workload's credential file
// by the runtime adapter (execution's GitGrants), never through the admission.
func (s *Service) AdmitV3(ctx context.Context, route domain.AttemptID, req runtimev1.AdmitRequest) (resp runtimev3.AdmitResponseV3, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "admit", route, err) }()
	info, doc, err := s.admit(ctx, route, req, WireV3)
	if err != nil {
		return resp, err
	}
	c, err := decodeContextV3(doc)
	if err != nil {
		return resp, err
	}
	resp = runtimev3.AdmitResponseV3{Token: info, Context: c}
	if err := resp.Validate(); err != nil {
		return runtimev3.AdmitResponseV3{}, failWith(ErrUnavailable, err, "the response is not a valid admission")
	}
	return resp, nil
}

func (s *Service) admit(ctx context.Context, route domain.AttemptID, req runtimev1.AdmitRequest, wire int64) (info runtimev1.TokenInfo, doc []byte, err error) {
	if err := req.Validate(); err != nil {
		return info, nil, failWith(ErrInvalidRequest, err, "the admission request is invalid")
	}
	if err := route.Validate(); err != nil {
		return info, nil, fail(ErrInvalidCredential, "the credential is not valid for this attempt")
	}
	cred, err := ParseCredential(req.AdmissionCredential)
	if err != nil {
		return info, nil, err
	}
	digest := cred.Digest()
	loc, err := s.cfg.Store.Locate(ctx, digest)
	if err != nil {
		return info, nil, err
	}
	if loc.AttemptID != route {
		return info, nil, fail(ErrInvalidCredential, "the credential is not valid for this attempt")
	}

	pre, err := s.cfg.Attempts.ObserveAttempt(ctx, loc.Scope, loc.AttemptID)
	if err != nil {
		return info, nil, missing(err)
	}
	if err := pre.Validate(); err != nil {
		return info, nil, err
	}
	if pre.Phase != AttemptActive {
		return info, nil, fail(ErrAttemptRevoked, "the attempt is no longer authorized")
	}
	evidence, err := s.cfg.Jobs.ObserveJob(ctx, JobObservation{
		Scope: loc.Scope, AttemptID: loc.AttemptID, Expected: pre.Job,
		ClaimedJobName: req.Job.Name, ClaimedJobUID: req.Job.UID, ClaimedPodName: req.Job.PodName, ClaimedPodUID: req.Job.PodUID,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return info, nil, fail(ErrForbidden, "the Job identity does not match the attempt")
		}
		return info, nil, failWith(ErrUnavailable, err, "the workload could not be observed")
	}
	if err := verifyOwnership(pre.Job, req.Job, evidence); err != nil {
		return info, nil, err
	}

	var denied error
	err = s.cfg.Store.Do(ctx, loc.Scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		state, err := tx.LockAttempt(ctx, loc.Scope, loc.AttemptID)
		if err != nil {
			return missing(err)
		}
		if err := state.Validate(); err != nil {
			return err
		}
		g, err := tx.GrantByCredential(ctx, digest, LockUpdate)
		if err != nil {
			return missing(err)
		}
		if g.Scope != loc.Scope || g.AttemptID != loc.AttemptID {
			return fail(ErrInvalidCredential, "the credential is not valid for this attempt")
		}
		if g.Wire() != wire {
			// Another runtime wire's attempt: nothing is consumed or revoked.
			denied = fail(ErrContextVersionMismatch, "the attempt belongs to another runtime version")
			return nil
		}
		now := s.now()
		revoke := func(reason error) error {
			if g.Revoke(now) {
				if err := tx.SaveRevocation(ctx, g); err != nil {
					return err
				}
			}
			denied = reason
			return nil
		}
		if state.Phase != AttemptActive {
			return revoke(fail(ErrAttemptRevoked, "the attempt is no longer authorized"))
		}
		if !sameBinding(state, g) {
			return revoke(fail(ErrAttemptRevoked, "the attempt's authority changed"))
		}
		if state.Revision != pre.Revision || state.Job != pre.Job {
			return fail(ErrUnavailable, "the attempt changed during the observation; retry")
		}
		if g.Generation != s.cfg.Tokens.Generation() {
			denied = fail(ErrInvalidCredential, "the credential is not valid")
			return nil
		}
		pinned, err := pinnedWrapper(g.Context, wire)
		if err != nil {
			return err
		}
		if err := s.cfg.Wrapper.Check(req.ClientVersion, pinned); err != nil {
			denied = err
			return nil
		}
		session, err := NewSessionID(s.cfg.Random)
		if err != nil {
			return err
		}
		pod := PodBinding{JobUID: req.Job.UID, PodName: req.Job.PodName, PodUID: req.Job.PodUID}
		adm, err := g.Consume(NonceDigest(req.Nonce), pod, session, now, s.cfg.Policy)
		if err != nil {
			denied = err
			return nil
		}
		if !adm.Replayed {
			if err := tx.SaveConsumption(ctx, g); err != nil {
				return err
			}
		}
		exp, err := g.TokenExpiry(now, s.cfg.Policy.TokenTTL)
		if err != nil {
			return err
		}
		token, err := s.cfg.Tokens.Issue(IssueRequest{
			Tenant: g.Scope.TenantID, Session: adm.SessionID, Generation: g.Generation, IssuedAt: now, ExpiresAt: exp,
		})
		if err != nil {
			return err // rolls back the consumption: nothing was handed out
		}
		info, doc = tokenInfo(token, exp, adm.MaxExpiresAt), bytes.Clone(g.Context)
		return nil
	})
	if err != nil {
		return runtimev1.TokenInfo{}, nil, err
	}
	if denied != nil {
		return runtimev1.TokenInfo{}, nil, denied
	}
	return info, doc, nil
}

// pinnedWrapper is the wrapper version a pinned profile requires, "" for none.
func pinnedWrapper(doc []byte, wire int64) (string, error) {
	if wire == WireV3 {
		c, err := decodeContextV3(doc)
		if err != nil {
			return "", err
		}
		if c.Profile != nil {
			return c.Profile.WrapperVersion, nil
		}
		return "", nil
	}
	if wire == WireV2 {
		c, err := decodeContextV2(doc)
		if err != nil {
			return "", err
		}
		if c.Profile != nil {
			return c.Profile.WrapperVersion, nil
		}
		return "", nil
	}
	c, err := decodeContext(doc)
	if err != nil {
		return "", err
	}
	if c.Profile != nil {
		return c.Profile.WrapperVersion, nil
	}
	return "", nil
}

// sameBinding reports whether the locked execution state still describes the
// attempt the grant was registered for.
func sameBinding(s AttemptState, g AdmissionGrant) bool {
	return s.Scope == g.Scope && s.RunID == g.RunID && s.AttemptID == g.AttemptID && s.Job == g.Job &&
		s.Revision == g.AuthorityRevision && s.ContextDigest.Equal(g.ContextDigest)
}

func tokenInfo(token string, exp, max time.Time) runtimev1.TokenInfo {
	return runtimev1.TokenInfo{
		Token: token, TokenType: "Bearer", Audience: runtimev1.Audience,
		ExpiresAt:    runtimev1.Timestamp(exp.UTC().Format(time.RFC3339Nano)),
		MaxExpiresAt: runtimev1.Timestamp(max.UTC().Format(time.RFC3339Nano)),
	}
}
