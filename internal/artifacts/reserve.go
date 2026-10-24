package artifacts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ReserveInput is the caller's declaration, already structurally validated by
// the transport. Pointers mirror the runtime contract's explicit nulls.
type ReserveInput struct {
	Name      string
	Kind      Kind
	Role      Role
	Digest    Digest
	MediaType *string
	Size      *int64
	Registry  *string
}

// ReserveOutput is the reservation response. Upload is set for a file only and
// is freshly issued by every call; it is never stored.
type ReserveOutput struct {
	ID      domain.ArtifactID
	State   State
	Upload  *Transfer
	Created bool
}

// Reserve reserves a named file upload or registers a verified OCI artifact for
// the attempt. It reauthorizes on every call, replays an identical repeat
// (200) without rewriting anything, rejects changed metadata for the same name
// (conflict), and never reports success before durable persistence is known.
func (s *Service) Reserve(ctx context.Context, caller Caller, attempt domain.AttemptID, in ReserveInput, requestID string) (ReserveOutput, error) {
	g, err := s.grant(ctx, caller, attempt)
	if err != nil {
		return ReserveOutput{}, err
	}
	res, err := s.reservation(g, in)
	if err != nil {
		return ReserveOutput{}, err
	}
	if err := s.permitted(g, res); err != nil {
		return ReserveOutput{}, err
	}

	existing, err := s.cfg.Repository.FindByName(ctx, g.Scope, attempt, res.Name)
	switch {
	case err == nil:
		return s.replay(ctx, g, existing, res)
	case !errors.Is(err, ErrNotFound):
		return ReserveOutput{}, unavailable("reservation lookup", err)
	}

	now := s.clock()
	id := s.cfg.IDs.NewArtifactID()
	var art Artifact
	switch res.Kind {
	case KindFile:
		res.Provenance = g.Source
		expires := s.uploadExpiry(g, now)
		if expires.Sub(now) < minTransfer {
			return ReserveOutput{}, ErrAttemptInactive
		}
		key := ObjectKey(g.Scope, id, SegmentStaging, s.cfg.IDs.NewToken())
		art, err = NewFile(id, res, key, expires, now)
	case KindOCI:
		// Registry access is an external call: it happens before, and outside,
		// any transaction. Only a verified registration is ever stored.
		v, verr := s.cfg.Registry.Verify(ctx, OCIRequest{Scope: g.Scope, Attempt: g.Attempt, Run: g.Run, Registry: res.Registry, Digest: res.Digest})
		if verr != nil {
			return ReserveOutput{}, classifyRegistry(verr)
		}
		res.Registry, res.Provenance = v.Repository, v.Provenance
		art, err = NewRegistered(id, res, v, now)
	}
	if err != nil {
		return ReserveOutput{}, err
	}
	out, err := s.cfg.Repository.Reserve(ctx, ReserveCommand{Artifact: art, Limits: s.limits(g), Actor: workloadActor(g), RequestID: requestID, Now: now})
	if err != nil {
		if errors.Is(err, ErrLimitExceeded) || errors.Is(err, ErrIdempotencyConflict) || errors.Is(err, ErrInvalid) {
			return ReserveOutput{}, err
		}
		// An unknown commit outcome is never success: the caller retries and the
		// replay lookup observes the committed reservation, if any.
		return ReserveOutput{}, unavailable("persist reservation", err)
	}
	if !out.Created {
		return s.replay(ctx, g, out.Artifact, res)
	}
	return s.respond(ctx, g, out.Artifact, true)
}

func (s *Service) limits(g AttemptGrant) Limits {
	l := g.Limits
	l.DiagnosticFiles, l.BundleBytes, l.BundleFiles = s.cfg.DiagnosticFiles, s.cfg.BundleBytes, s.cfg.BundleFiles
	return l
}

// reservation normalizes the declaration into the entity's input.
func (s *Service) reservation(g AttemptGrant, in ReserveInput) (Reservation, error) {
	r := Reservation{Scope: g.Scope, Attempt: g.Attempt, Run: g.Run, Step: g.Step, Name: in.Name, Kind: in.Kind, Role: in.Role, Digest: in.Digest, Size: in.Size}
	if in.MediaType != nil {
		r.MediaType = *in.MediaType
	}
	if in.Registry != nil {
		ref, err := ParseRegistryRef(*in.Registry)
		if err != nil {
			return Reservation{}, err
		}
		r.Registry = ref.String()
	}
	if in.Kind == KindOCI {
		// Provenance is established by verification, not by the caller. Validate
		// the declaration shape with a placeholder.
		probe := r
		probe.Provenance = Provenance{RepositoryURL: "https://example.invalid/x", Commit: "0000000000000000000000000000000000000000"}
		if err := probe.Validate(); err != nil {
			return Reservation{}, err
		}
		return r, nil
	}
	return r, r.Validate()
}

// permitted checks the declaration against the attempt's trusted context.
func (s *Service) permitted(g AttemptGrant, r Reservation) error {
	switch r.Role {
	case RoleOutput:
		d, ok := g.declared(r.Name)
		if !ok || d.Kind != r.Kind {
			return fmt.Errorf("%w: the name is not a declared %s output", ErrOutputInvalid, r.Kind)
		}
	case RoleBundle:
		if !g.Writer {
			return fmt.Errorf("%w: only a writer attempt may produce a source bundle", ErrOutputInvalid)
		}
	}
	return nil
}

// minTransfer is the shortest instruction the object store can sign; an
// attempt whose authority lapses sooner is treated as inactive.
const minTransfer = time.Second

func (s *Service) uploadExpiry(g AttemptGrant, now time.Time) time.Time {
	e := now.Add(s.cfg.UploadTTL)
	if g.ExpiresAt.Before(e) {
		e = g.ExpiresAt
	}
	return e.Truncate(time.Microsecond)
}

// replay answers an identical repeat of a reservation. It returns a fresh
// instruction for a file (recording its expiry durably first) and never reopens
// or modifies usable content: the shape stays "reserved with upload" for a
// file whatever its current state.
func (s *Service) replay(ctx context.Context, g AttemptGrant, existing Artifact, res Reservation) (ReserveOutput, error) {
	if !existing.Matches(res) {
		return ReserveOutput{}, fmt.Errorf("%w: the name was reserved with different metadata", ErrIdempotencyConflict)
	}
	if existing.Disposition() != DispositionActive {
		return ReserveOutput{}, fmt.Errorf("%w: the earlier reservation was closed", ErrIdempotencyConflict)
	}
	return s.respond(ctx, g, existing, false)
}

func (s *Service) respond(ctx context.Context, g AttemptGrant, a Artifact, created bool) (ReserveOutput, error) {
	if a.Kind() == KindOCI {
		return ReserveOutput{ID: a.ID(), State: StateRegistered, Created: created}, nil
	}
	now := s.clock()
	expires := s.uploadExpiry(g, now)
	if expires.Sub(now) < minTransfer {
		return ReserveOutput{}, ErrAttemptInactive
	}
	// The latest issued expiry is durable before the instruction exists, so a
	// cleanup sweep can never miss a still-valid transfer.
	if err := s.cfg.Repository.RecordTransfer(ctx, g.Scope, a.ID(), expires, now); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ReserveOutput{}, fmt.Errorf("%w: the earlier reservation was closed", ErrIdempotencyConflict)
		}
		return ReserveOutput{}, unavailable("record transfer", err)
	}
	size, _ := a.Size()
	up, err := s.cfg.Objects.PresignUpload(ctx, UploadSpec{Key: a.StagingKey(), MediaType: a.MediaType(), Digest: a.Digest(), Size: size, ExpiresAt: expires})
	if err != nil {
		return ReserveOutput{}, unavailable("issue upload instruction", err)
	}
	return ReserveOutput{ID: a.ID(), State: StateReserved, Upload: &up, Created: created}, nil
}

func classifyRegistry(err error) error {
	switch {
	case errors.Is(err, ErrRegistryDenied), errors.Is(err, ErrRegistryContent), errors.Is(err, ErrProvenanceMissing), errors.Is(err, ErrInvalid):
		return err
	case errors.Is(err, ErrUnavailable):
		return err
	}
	return unavailable("registry verification", err)
}
