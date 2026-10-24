package artifacts

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// CompleteInput is the producer's completion declaration.
type CompleteInput struct {
	Digest Digest
	Size   int64
}

// Completion is the immutable result of a completed artifact. Repeating the
// completion returns the same value.
type Completion struct {
	ID     domain.ArtifactID
	State  State
	Digest Digest
	Size   int64
}

// Complete verifies the bytes uploaded for a file reservation and makes them
// usable, or confirms a verified OCI registration.
//
// The declaration is compared first, then the stored bytes are read and hashed
// (never the ETag or caller metadata), copied to a server-only sealed
// destination, and only then is the artifact committed usable if its fenced
// claim is still current. Staging uploads can therefore never change usable
// content. The attempt is reauthorized before finalization. External calls
// happen outside every SQL transaction; every candidate destination is durably
// recorded before it is written.
func (s *Service) Complete(ctx context.Context, caller Caller, attempt domain.AttemptID, id domain.ArtifactID, in CompleteInput, requestID string) (Completion, error) {
	g, err := s.grant(ctx, caller, attempt)
	if err != nil {
		return Completion{}, err
	}
	if err := id.Validate(); err != nil {
		return Completion{}, invalidf("%v", err)
	}
	if err := in.Digest.Validate(); err != nil {
		return Completion{}, err
	}
	if in.Size < 0 || in.Size > MaxFileBytes {
		return Completion{}, invalidf("sizeBytes out of range")
	}
	a, err := s.cfg.Repository.GetForAttempt(ctx, g.Scope, attempt, id)
	if err != nil {
		return Completion{}, s.dependency("load artifact", err)
	}
	if a.Disposition() != DispositionActive {
		return Completion{}, ErrNotFound
	}
	if err := a.CheckCompletion(in.Digest, in.Size); err != nil {
		return Completion{}, err
	}
	if a.Usable() {
		return completionOf(a), nil // idempotent repeat: the same immutable result
	}

	now := s.clock()
	token := s.cfg.IDs.NewToken()
	dest := ObjectKey(g.Scope, id, SegmentSealed, s.cfg.IDs.NewToken())
	cr, err := s.cfg.Repository.BeginCompletion(ctx, BeginCompletion{Scope: g.Scope, Attempt: attempt, ID: id, Token: token, Destination: dest, Lease: s.cfg.ClaimLease, Now: now})
	if err != nil {
		return Completion{}, s.dependency("begin completion", err)
	}
	if cr.Claim == nil {
		return completionOf(cr.Artifact), nil
	}
	claim := *cr.Claim
	art := cr.Artifact

	ev, err := s.verifyAndSeal(ctx, art, dest, claim)
	if err != nil {
		if definitive(err) {
			// The claim produced a definitive failure: free the lease so the
			// producer can upload correct bytes and retry immediately.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			_ = s.cfg.Repository.ReleaseClaim(rctx, g.Scope, id, claim.Token)
			cancel()
		}
		return Completion{}, err
	}

	// Reauthorize before finalization: a revoked or cancelled attempt cannot make
	// content usable.
	if _, err := s.grant(ctx, caller, attempt); err != nil {
		return Completion{}, err
	}
	done, err := s.cfg.Repository.CommitUsable(ctx, CommitCommand{Scope: g.Scope, Attempt: attempt, ID: id, Claim: claim, Destination: dest,
		Verification: ev, Actor: workloadActor(g), RequestID: requestID, Now: s.clock()})
	if err == nil {
		return completionOf(done), nil
	}
	return s.settle(ctx, g, id, dest, err)
}

// settle resolves an error from the final commit. An unknown outcome is never
// success: the durable state is re-read, and only a committed usable artifact
// is reported.
func (s *Service) settle(ctx context.Context, g AttemptGrant, id domain.ArtifactID, dest string, commitErr error) (Completion, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	cur, err := s.cfg.Repository.GetForAttempt(rctx, g.Scope, g.Attempt, id)
	if err == nil && cur.Usable() {
		// This commit, or a concurrent completion of identical verified content,
		// won. Either way the stored result is the one every repeat returns.
		return completionOf(cur), nil
	}
	if errors.Is(commitErr, ErrClaimLost) {
		return Completion{}, fmt.Errorf("%w: another completion holds the artifact", ErrInProgress)
	}
	return Completion{}, unavailable("commit completion", commitErr)
}

func completionOf(a Artifact) Completion {
	size, _ := a.Size()
	return Completion{ID: a.ID(), State: StateUsable, Digest: a.Digest(), Size: size}
}

// dependency maps repository errors, preserving the nondisclosing not-found.
func (s *Service) dependency(what string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrInProgress), errors.Is(err, ErrInvalid), errors.Is(err, ErrInconsistent):
		return err
	}
	return unavailable(what, err)
}

// definitive reports failures that will repeat for the same bytes.
func definitive(err error) bool {
	return errors.Is(err, ErrIncomplete) || errors.Is(err, ErrDigestMismatch)
}

// verifyAndSeal reads the staging object into bounded local scratch while
// hashing it, requires the declared size and digest, and seals the verified
// bytes at the already-recorded destination. The scratch file is removed on
// every exit path.
func (s *Service) verifyAndSeal(ctx context.Context, a Artifact, dest string, claim Claim) (Verification, error) {
	size, _ := a.Size()
	// The lease bounds every object-store call, so a slow worker cannot write a
	// destination after its claim could have been taken over and swept.
	lctx, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()

	obj, err := s.cfg.Objects.Open(lctx, a.StagingKey())
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			return Verification{}, fmt.Errorf("%w: no uploaded object", ErrIncomplete)
		}
		return Verification{}, unavailable("read upload", err)
	}
	defer obj.Body.Close()
	if obj.Size != size {
		return Verification{}, fmt.Errorf("%w: the uploaded object is not the declared size", ErrIncomplete)
	}

	f, err := os.CreateTemp(s.cfg.SpoolDir, "kodama-artifact-*")
	if err != nil {
		return Verification{}, unavailable("scratch space", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(obj.Body, size+1))
	if err != nil {
		return Verification{}, unavailable("read upload", err)
	}
	if n != size {
		return Verification{}, fmt.Errorf("%w: the uploaded object is not the declared size", ErrIncomplete)
	}
	got := DigestOfSum(h.Sum(nil))
	if got != a.Digest() {
		return Verification{}, fmt.Errorf("%w: uploaded content does not match the declared digest", ErrDigestMismatch)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Verification{}, unavailable("scratch space", err)
	}
	if err := s.cfg.Objects.Put(lctx, PutSpec{Key: dest, Body: f, Size: size, Digest: got, MediaType: a.MediaType()}); err != nil {
		return Verification{}, unavailable("seal content", err)
	}
	return Verification{Digest: got, Size: size, At: s.clock()}, nil
}
