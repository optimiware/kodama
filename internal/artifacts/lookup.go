package artifacts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// View is the safe public representation of an artifact: metadata plus
// optional, short-lived download instructions. It carries no storage key or
// bucket.
type View struct {
	Artifact Artifact
	Download *Transfer
}

// Metadata returns an artifact of the scope, optionally with a download
// instruction. The caller is reauthorized against current membership
// (including diagnostic visibility) before anything is issued, on every call.
// A missing artifact, a foreign one and a hidden one are indistinguishable.
func (s *Service) Metadata(ctx context.Context, caller Caller, scope domain.Scope, id domain.ArtifactID, download bool) (View, error) {
	if err := scope.Validate(); err != nil {
		return View{}, invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return View{}, invalidf("%v", err)
	}
	a, err := s.cfg.Repository.Get(ctx, scope, id)
	if err != nil {
		return View{}, s.dependency("load artifact", err)
	}
	subject := ReadSubject{ID: a.ID(), Attempt: a.Attempt(), Kind: a.Kind(), Role: a.Role(), Download: download}
	if err := s.cfg.Reader.AuthorizeRead(ctx, caller, scope, subject); err != nil {
		switch {
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
			return View{}, err
		}
		return View{}, fmt.Errorf("%w: read authority: %v", ErrUnavailable, err)
	}
	v := View{Artifact: a}
	if !download {
		return v, nil
	}
	if a.Kind() != KindFile || a.State() != StateUsable || a.Disposition() != DispositionActive || a.SealedKey() == "" {
		return View{}, fmt.Errorf("%w: no verified content to download", ErrIncomplete)
	}
	expires := s.clock().Add(s.cfg.DownloadTTL).Truncate(time.Microsecond)
	t, err := s.cfg.Objects.PresignDownload(ctx, DownloadSpec{Key: a.SealedKey(), MediaType: a.MediaType(), ExpiresAt: expires})
	if err != nil {
		return View{}, unavailable("issue download instruction", err)
	}
	v.Download = &t
	return v, nil
}

// References returns the consumer view of the attempt's artifacts among ids.
// An id that is absent, foreign to the scope or produced by another attempt is
// omitted, so a consumer cannot tell them apart. An incomplete artifact is
// present with Usable false.
func (s *Service) References(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]Reference, error) {
	if err := scope.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if err := attempt.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	for _, id := range ids {
		if err := id.Validate(); err != nil {
			return nil, invalidf("%v", err)
		}
	}
	out, err := s.cfg.Repository.ReferencesForAttempt(ctx, scope, attempt, ids)
	if err != nil {
		return nil, unavailable("load references", err)
	}
	return out, nil
}

// Reference returns one artifact of the scope for evidence and source
// consumers, or ErrNotFound for an absent or foreign one.
func (s *Service) Reference(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (Reference, error) {
	if err := scope.Validate(); err != nil {
		return Reference{}, invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return Reference{}, invalidf("%v", err)
	}
	a, err := s.cfg.Repository.Get(ctx, scope, id)
	if err != nil {
		return Reference{}, s.dependency("load artifact", err)
	}
	return a.Reference(), nil
}

// Pin protects a usable artifact from retention deletion for owner. Pinning is
// serialized against deletion in the repository; a removed artifact reports
// ErrNotFound.
func (s *Service) Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner PinOwner) error {
	if err := validPin(scope, id, owner); err != nil {
		return err
	}
	if err := s.cfg.Repository.Pin(ctx, scope, id, owner, s.clock()); err != nil {
		return s.dependency("pin artifact", err)
	}
	return nil
}

// Unpin releases a pin; releasing an absent pin succeeds.
func (s *Service) Unpin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner PinOwner) error {
	if err := validPin(scope, id, owner); err != nil {
		return err
	}
	if err := s.cfg.Repository.Unpin(ctx, scope, id, owner); err != nil {
		return s.dependency("unpin artifact", err)
	}
	return nil
}

func validPin(scope domain.Scope, id domain.ArtifactID, owner PinOwner) error {
	if err := scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if owner.Kind == "" || len(owner.Kind) > 32 || domain.ArtifactID(owner.ID).Validate() != nil {
		return invalidf("a pin needs an owner kind and identifier")
	}
	return nil
}
