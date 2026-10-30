package adapters

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// PinOwnerKind is the artifact-pin owner kind of attempt finalization. The
// owner ID is the attempt, so one pin protects every artifact the attempt's
// result or source proposal consumes until its owner releases it.
const PinOwnerKind = "attempt_finalization"

// ArtifactLookup is the artifact module's scoped read and pin surface.
// *artifacts.Service implements it.
type ArtifactLookup interface {
	References(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]artifacts.Reference, error)
	Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner) error
}

// Artifacts implements results.Evidence over the artifact module.
type Artifacts struct{ svc ArtifactLookup }

var _ results.Evidence = (*Artifacts)(nil)

// NewArtifacts requires the artifact service.
func NewArtifacts(svc ArtifactLookup) (*Artifacts, error) {
	if svc == nil {
		return nil, errors.New("results/adapters: an artifact service is required")
	}
	return &Artifacts{svc: svc}, nil
}

// References implements results.Evidence. The artifact owner scopes the lookup
// to the attempt, so absent, foreign and removed ids are all omitted.
func (a *Artifacts) References(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]results.ArtifactEvidence, error) {
	refs, err := a.svc.References(ctx, scope, attempt, ids)
	if err != nil {
		return nil, mapArtifacts(err)
	}
	out := make(map[domain.ArtifactID]results.ArtifactEvidence, len(refs))
	for id, r := range refs {
		out[id] = results.ArtifactEvidence{
			ID: r.ID, Scope: r.Scope, Attempt: r.Attempt, Kind: runtimev1.ArtifactKind(r.Kind), Role: runtimev1.ArtifactRole(r.Role),
			Usable: r.Usable, Digest: runtimev1.Digest(r.Digest),
			Provenance: results.Provenance{RepositoryURL: r.Provenance.RepositoryURL, Commit: r.Provenance.Commit},
		}
	}
	return out, nil
}

// Protect implements results.Evidence. It pins only usable artifacts the
// attempt itself produced, so one attempt can never hold another attempt's
// content; anything else is skipped and rejected by the consumer's own checks.
func (a *Artifacts) Protect(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) error {
	if len(ids) == 0 {
		return nil
	}
	own, err := a.svc.References(ctx, scope, attempt, ids)
	if err != nil {
		return mapArtifacts(err)
	}
	for _, id := range ids {
		ref, ok := own[id]
		if !ok || !ref.Usable {
			continue
		}
		if err := a.svc.Pin(ctx, scope, id, artifacts.PinOwner{Kind: PinOwnerKind, ID: string(attempt)}); err != nil {
			if errors.Is(err, artifacts.ErrNotFound) || errors.Is(err, artifacts.ErrIncomplete) {
				// Removed or not usable any more: the consumer's evidence check
				// reports it; a pin is not what decides.
				continue
			}
			return mapArtifacts(err)
		}
	}
	return nil
}

func mapArtifacts(err error) error {
	switch {
	case errors.Is(err, artifacts.ErrNotFound):
		return results.Fail(results.ErrNotFound, err, "artifact not found")
	case errors.Is(err, artifacts.ErrIncomplete):
		return results.Fail(results.ErrArtifactIncomplete, err, "artifact upload is not complete")
	case errors.Is(err, artifacts.ErrInvalid):
		return results.Fail(results.ErrInvalidRequest, err, "the request is not valid for this operation")
	}
	return results.Fail(results.ErrUnavailable, err, "the artifact store could not answer")
}

// ---- artifact transfer authority ------------------------------------------

// RuntimePrincipal is the artifacts.Caller subject of a runtime request:
// "runtime:<tenant>:<session>". The runtime-token authenticator of the router
// (T-027) emits it as httpapi.Principal.ID after verifying the bearer; it is
// the only identity the artifact routes need, because every other fact (scope,
// attempt, run, limits, provenance) is read from the durable grant.
func RuntimePrincipal(tenant domain.TenantID, session runtimeauth.SessionID) string {
	return "runtime:" + string(tenant) + ":" + string(session)
}

func parsePrincipal(subject string) (domain.TenantID, runtimeauth.SessionID, bool) {
	rest, ok := strings.CutPrefix(subject, "runtime:")
	if !ok {
		return "", "", false
	}
	tenant, session, ok := strings.Cut(rest, ":")
	if !ok || domain.TenantID(tenant).Validate() != nil || runtimeauth.SessionID(session).Validate() != nil {
		return "", "", false
	}
	return domain.TenantID(tenant), runtimeauth.SessionID(session), true
}

// ArtifactAuthority implements artifacts.AttemptAuthority: the attempt's current
// authority for reserving and completing artifacts, derived from the verified
// grant and the attempt's pinned context. It is re-read on every call.
type ArtifactAuthority struct {
	store  runtimeauth.Store
	gens   Generations
	clock  func() time.Time
	begins Begins
}

var _ artifacts.AttemptAuthority = (*ArtifactAuthority)(nil)

// NewArtifactAuthority requires the grant store and the signing generation.
func NewArtifactAuthority(store runtimeauth.Store, gens Generations, clock func() time.Time) (*ArtifactAuthority, error) {
	if store == nil || gens == nil {
		return nil, errors.New("results/adapters: a grant store and a signing generation are required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &ArtifactAuthority{store: store, gens: gens, clock: clock}, nil
}

// WithBegins returns the authority that also attributes the output of a
// run_shared attempt to the workspace state the server recorded at its command
// begin. A v2 attempt is refused without it (fail closed): its artifacts' source
// provenance would otherwise claim a clean checkout nobody verified.
func (a *ArtifactAuthority) WithBegins(b Begins) *ArtifactAuthority {
	c := *a
	c.begins = b
	return &c
}

// Grant implements artifacts.AttemptAuthority. The router has verified the
// token under the current signing key; the grant check here still requires the
// grant's generation to be the current one, the session to belong to the
// route's attempt, the attempt to be active and the grant unrevoked.
func (a *ArtifactAuthority) Grant(ctx context.Context, caller artifacts.Caller, attempt domain.AttemptID) (artifacts.AttemptGrant, error) {
	return a.grant(ctx, caller, attempt, runtimeauth.ActionArtifactReserve)
}

// GrantRead is the consumer-read counterpart of Grant. It checks the same
// session and attempt, but requires the artifact.read action.
func (a *ArtifactAuthority) GrantRead(ctx context.Context, caller artifacts.Caller, attempt domain.AttemptID) (artifacts.AttemptGrant, error) {
	return a.grant(ctx, caller, attempt, runtimeauth.ActionArtifactRead)
}

func (a *ArtifactAuthority) grant(ctx context.Context, caller artifacts.Caller, attempt domain.AttemptID, action runtimeauth.Action) (artifacts.AttemptGrant, error) {
	tenant, session, ok := parsePrincipal(caller.Subject)
	if !ok {
		return artifacts.AttemptGrant{}, artifacts.ErrAuthorityMissing
	}
	var out artifacts.AttemptGrant
	var v2 *runtimev2.ContextV2
	err := a.store.Do(ctx, tenant, func(ctx context.Context, tx runtimeauth.GrantTx) error {
		gen := a.gens.Generation()
		g, _, err := runtimeauth.CheckSession(ctx, tx, runtimeauth.SessionCheck{
			Route: attempt, Session: session, TokenGeneration: gen, CurrentGeneration: gen, Action: action, Now: a.clock(),
		})
		if err != nil {
			return err
		}
		pinned, c2, err := DecodePinned(g.Context)
		v2 = c2
		if err != nil {
			return errors.Join(runtimeauth.ErrUnavailable, err)
		}
		if g.Consumption == nil || pinned.Scope() != g.Scope || pinned.AttemptID != g.AttemptID {
			return errors.Join(runtimeauth.ErrUnavailable, errors.New("the grant's pinned context does not match its owner"))
		}
		out = artifactGrant(g, pinned)
		return nil
	})
	if err == nil && v2 != nil {
		err = a.attribute(ctx, &out, *v2)
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrBeginNotRecorded):
			return artifacts.AttemptGrant{}, artifacts.ErrForbidden
		case errors.Is(err, runtimeauth.ErrAttemptRevoked):
			return artifacts.AttemptGrant{}, artifacts.ErrAttemptInactive
		case errors.Is(err, runtimeauth.ErrForbidden):
			return artifacts.AttemptGrant{}, artifacts.ErrForbidden
		case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound):
			return artifacts.AttemptGrant{}, artifacts.ErrAuthorityMissing
		}
		return artifacts.AttemptGrant{}, fmt.Errorf("%w: %v", artifacts.ErrUnavailable, err)
	}
	return out, nil
}

// attribute replaces the source provenance of a run_shared attempt's grant with
// the one carrying the server-recorded begin state. It runs outside the grant
// transaction (it reads execution's own store).
func (a *ArtifactAuthority) attribute(ctx context.Context, out *artifacts.AttemptGrant, c runtimev2.ContextV2) error {
	if c.Workspace.Source == nil {
		return nil
	}
	if a.begins == nil {
		return errors.Join(runtimeauth.ErrUnavailable, errors.New("no begin records back the provenance of a run_shared attempt"))
	}
	rec, ok, err := a.begins.Begin(ctx, out.Scope, out.Attempt)
	if err != nil {
		return errors.Join(runtimeauth.ErrUnavailable, err)
	}
	var begin *execution.BeginRecord
	if ok {
		begin = &rec
	}
	prov, err := V2Provenance(c, begin)
	if err != nil {
		if errors.Is(err, ErrBeginNotRecorded) {
			return err
		}
		return errors.Join(runtimeauth.ErrUnavailable, err)
	}
	out.Source = prov
	return nil
}

// artifactGrant derives what the artifact module needs from verified facts
// only: the grant's owner and session bound, and the pinned context's declared
// outputs, limits and input source (the provenance every output carries).
func artifactGrant(g runtimeauth.AdmissionGrant, pinned runtimev1.Context) artifacts.AttemptGrant {
	out := artifacts.AttemptGrant{
		Scope: g.Scope, Attempt: g.AttemptID, Run: g.RunID, Step: pinned.StepID,
		ExpiresAt: g.Consumption.MaxExpiresAt,
		Writer:    pinned.Workspace.Access == runtimev1.AccessWrite,
		Limits: artifacts.Limits{
			OutputBytes: pinned.Limits.MaxOutputBytes, OutputFiles: pinned.Limits.MaxOutputFiles, DiagnosticBytes: pinned.Limits.MaxDiagnosticBytes,
		},
	}
	if src := pinned.Workspace.Source; src != nil {
		out.Source = artifacts.Provenance{RepositoryURL: src.RepositoryURL, Commit: string(src.Commit)}
	}
	for _, o := range pinned.Outputs {
		switch o.Type {
		case runtimev1.OutputArtifact:
			out.Outputs = append(out.Outputs, artifacts.OutputDecl{Name: o.Name, Kind: artifacts.KindFile})
		case runtimev1.OutputOCIArtifact:
			out.Outputs = append(out.Outputs, artifacts.OutputDecl{Name: o.Name, Kind: artifacts.KindOCI})
		}
	}
	for _, in := range pinned.ArtifactFiles {
		out.Inputs = append(out.Inputs, artifacts.InputRef{ID: domain.ArtifactID(in.ArtifactID), Digest: artifacts.Digest(in.Digest)})
	}
	return out
}
