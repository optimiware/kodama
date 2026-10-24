package runtimeauth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
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
	if cmd.Credential.IsZero() {
		return Registration{}, fail(ErrInvalidRequest, "a credential is required")
	}
	if cmd.Actions.IsZero() {
		return Registration{}, fail(ErrInvalidRequest, "a grant needs at least one action")
	}
	if cmd.Context.Scope() != cmd.Scope || cmd.Context.AttemptID != cmd.AttemptID {
		return Registration{}, fail(ErrInvalidRequest, "the context belongs to another scope or attempt")
	}
	contextDigest, doc, err := PinContext(cmd.Context)
	if err != nil {
		return Registration{}, err
	}
	generation := s.cfg.Tokens.Generation()

	err = s.cfg.Store.Do(ctx, cmd.Scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		state, err := tx.LockAttempt(ctx, cmd.Scope, cmd.AttemptID)
		if err != nil {
			return err
		}
		if err := state.Validate(); err != nil {
			return err
		}
		if state.Scope != cmd.Scope || state.AttemptID != cmd.AttemptID || state.RunID != cmd.Context.RunID {
			return fail(ErrConflict, "the attempt does not match the registration")
		}
		if state.Phase != AttemptActive {
			return fail(ErrAttemptRevoked, "the attempt is no longer authorized")
		}
		if !state.ContextDigest.Equal(contextDigest) {
			return fail(ErrConflict, "the context is not the attempt's pinned context")
		}
		if !cmd.Actions.SubsetOf(state.Actions) {
			return fail(ErrForbidden, "the actions exceed what the attempt allows")
		}
		g, err := NewAdmissionGrant(Provision{
			Scope: cmd.Scope, RunID: state.RunID, AttemptID: cmd.AttemptID, Job: state.Job,
			CredentialDigest: cmd.Credential.Digest(), Generation: generation, Actions: cmd.Actions,
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
	if err := req.Validate(); err != nil {
		return resp, failWith(ErrInvalidRequest, err, "the admission request is invalid")
	}
	if err := route.Validate(); err != nil {
		return resp, fail(ErrInvalidCredential, "the credential is not valid for this attempt")
	}
	cred, err := ParseCredential(req.AdmissionCredential)
	if err != nil {
		return resp, err
	}
	digest := cred.Digest()
	loc, err := s.cfg.Store.Locate(ctx, digest)
	if err != nil {
		return resp, err
	}
	if loc.AttemptID != route {
		return resp, fail(ErrInvalidCredential, "the credential is not valid for this attempt")
	}

	pre, err := s.cfg.Attempts.ObserveAttempt(ctx, loc.Scope, loc.AttemptID)
	if err != nil {
		return resp, missing(err)
	}
	if err := pre.Validate(); err != nil {
		return resp, err
	}
	if pre.Phase != AttemptActive {
		return resp, fail(ErrAttemptRevoked, "the attempt is no longer authorized")
	}
	evidence, err := s.cfg.Jobs.ObserveJob(ctx, JobObservation{
		Scope: loc.Scope, AttemptID: loc.AttemptID, Expected: pre.Job,
		ClaimedJobName: req.Job.Name, ClaimedJobUID: req.Job.UID, ClaimedPodName: req.Job.PodName, ClaimedPodUID: req.Job.PodUID,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return resp, fail(ErrForbidden, "the Job identity does not match the attempt")
		}
		return resp, failWith(ErrUnavailable, err, "the workload could not be observed")
	}
	if err := verifyOwnership(pre.Job, req.Job, evidence); err != nil {
		return resp, err
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
		doc, err := decodeContext(g.Context)
		if err != nil {
			return err
		}
		pinned := ""
		if doc.Profile != nil {
			pinned = doc.Profile.WrapperVersion
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
		resp = runtimev1.AdmitResponse{Token: tokenInfo(token, exp, adm.MaxExpiresAt), Context: doc}
		return nil
	})
	if err != nil {
		return runtimev1.AdmitResponse{}, err
	}
	if denied != nil {
		return runtimev1.AdmitResponse{}, denied
	}
	if err := resp.Validate(); err != nil {
		return runtimev1.AdmitResponse{}, failWith(ErrUnavailable, err, "the response is not a valid admission")
	}
	return resp, nil
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
