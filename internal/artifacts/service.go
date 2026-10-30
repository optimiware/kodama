package artifacts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ServiceConfig wires the artifact use cases. Every security-critical
// dependency is required; there are no permissive defaults.
type ServiceConfig struct {
	Repository Repository
	// Authority is the current attempt authority (runtime operations).
	Authority AttemptAuthority
	// Reader authorizes public metadata and download access.
	Reader  ReadAuthority
	Objects ObjectStore
	// Registry verifies OCI registrations.
	Registry RegistryVerifier

	IDs   IDs
	Clock func() time.Time

	// UploadTTL and DownloadTTL are the maximum lifetimes of issued transfer
	// instructions. Upload instructions are further bounded by the attempt's
	// authority expiry.
	UploadTTL   time.Duration
	DownloadTTL time.Duration
	// ClaimLease bounds one completion attempt; it also bounds the object-store
	// calls it makes.
	ClaimLease time.Duration
	// SpoolDir holds temporary verification copies ("" selects os.TempDir).
	SpoolDir string
	// DiagnosticFiles, BundleBytes and BundleFiles bound the artifact roles the
	// attempt context does not limit.
	DiagnosticFiles int64
	BundleBytes     int64
	BundleFiles     int64
}

// Service implements the artifact use cases.
type Service struct {
	cfg ServiceConfig
	now func() time.Time
}

// NewService validates cfg.
func NewService(cfg ServiceConfig) (*Service, error) {
	switch {
	case cfg.Repository == nil:
		return nil, errors.New("artifacts: a repository is required")
	case cfg.Authority == nil:
		return nil, errors.New("artifacts: an attempt authority is required")
	case cfg.Reader == nil:
		return nil, errors.New("artifacts: a read authority is required")
	case cfg.Objects == nil:
		return nil, errors.New("artifacts: an object store is required")
	case cfg.Registry == nil:
		return nil, errors.New("artifacts: a registry verifier is required")
	case cfg.UploadTTL <= 0 || cfg.DownloadTTL <= 0 || cfg.ClaimLease <= 0:
		return nil, errors.New("artifacts: UploadTTL, DownloadTTL and ClaimLease must be positive")
	case cfg.BundleBytes < 0 || cfg.BundleFiles < 0 || cfg.DiagnosticFiles < 0:
		return nil, errors.New("artifacts: bounds must not be negative")
	}
	if cfg.IDs == nil {
		cfg.IDs = RandomIDs{}
	}
	if cfg.DiagnosticFiles == 0 {
		cfg.DiagnosticFiles = 32
	}
	if cfg.BundleBytes == 0 {
		cfg.BundleBytes = 1 << 30
	}
	if cfg.BundleFiles == 0 {
		cfg.BundleFiles = 4
	}
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = os.TempDir()
	}
	s := &Service{cfg: cfg, now: cfg.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// RandomIDs generates identifiers from crypto/rand.
type RandomIDs struct{}

func (RandomIDs) NewArtifactID() domain.ArtifactID { return domain.ArtifactID("art_" + randomHex(16)) }
func (RandomIDs) NewToken() string                 { return randomHex(24) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("artifacts: random source failed: %v", err)) // never continue with predictable IDs
	}
	return hex.EncodeToString(b)
}

// grant reauthorizes the attempt. It is called on every runtime operation,
// including repeats and replays; the module never caches authority.
func (s *Service) grant(ctx context.Context, caller Caller, attempt domain.AttemptID) (AttemptGrant, error) {
	if err := attempt.Validate(); err != nil {
		return AttemptGrant{}, invalidf("%v", err)
	}
	return s.checkGrant(ctx, caller, attempt, s.cfg.Authority.Grant)
}

func (s *Service) grantRead(ctx context.Context, caller Caller, attempt domain.AttemptID) (AttemptGrant, error) {
	if err := attempt.Validate(); err != nil {
		return AttemptGrant{}, invalidf("%v", err)
	}
	reader, ok := s.cfg.Authority.(AttemptReadAuthority)
	if !ok {
		return AttemptGrant{}, ErrForbidden
	}
	return s.checkGrant(ctx, caller, attempt, reader.GrantRead)
}

func (s *Service) checkGrant(ctx context.Context, caller Caller, attempt domain.AttemptID, lookup func(context.Context, Caller, domain.AttemptID) (AttemptGrant, error)) (AttemptGrant, error) {
	g, err := lookup(ctx, caller, attempt)
	if err != nil {
		switch {
		case errors.Is(err, ErrAuthorityMissing), errors.Is(err, ErrAttemptInactive), errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
			return AttemptGrant{}, err
		}
		return AttemptGrant{}, fmt.Errorf("%w: attempt authority: %v", ErrUnavailable, err)
	}
	if g.Attempt != attempt || g.Scope.Validate() != nil || g.Run.Validate() != nil || g.Limits.Validate() != nil || g.Step == "" {
		// An authority that returns a different attempt or an unusable record is
		// never trusted.
		return AttemptGrant{}, fmt.Errorf("%w: attempt authority returned an unusable grant", ErrUnavailable)
	}
	if !g.ExpiresAt.After(s.clock()) {
		return AttemptGrant{}, ErrAttemptInactive
	}
	if err := g.Source.Validate(); err != nil {
		return AttemptGrant{}, fmt.Errorf("%w: attempt authority returned invalid provenance", ErrUnavailable)
	}
	return g, nil
}

func workloadActor(g AttemptGrant) Actor { return Actor{Type: "workload", ID: string(g.Attempt)} }

// unavailable wraps a dependency failure without leaking its text to callers
// of the transport layer (which maps by sentinel only).
func unavailable(what string, err error) error {
	if errors.Is(err, ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, what, err)
}
