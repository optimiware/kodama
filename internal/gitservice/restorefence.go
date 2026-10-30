package gitservice

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The restore fence (SetRestoreFence, T-160) is the repository-level "restoring"
// state of the worker: while it is raised, receives, server ref operations and
// maintenance are refused with 503 unavailable, initialization and reads are
// admitted, and ReplayBundle is admitted (it is refused at any other time).
//
// It is durable. A marker file <DataRoot>/restore-fences/<repositoryId> exists
// whenever the fence is raised: it is written (and the directory synced) before
// the in-memory flag is set, and removed before the flag is cleared, both under
// the repository's exclusive gate, so a failure to persist leaves the fence as
// it was. A replacement worker reads the markers after it holds the generation
// lock and before it serves anything, so it comes up closed for every
// repository whose fence was raised: a crash between the steps of a restore can
// never reopen a half-restored repository to pushes. A crash in the middle of a
// failed entry may leave a marker without the flag; that errs on the closed
// side, and the control plane's repeat of the request converges.
//
// The fence may be raised for a repository that does not exist yet: a restore
// raises it, initializes the repository and replays its bundles. A marker lives
// on the worker's volume: a volume restored from a snapshot carries the markers
// of the snapshot, so the control plane, whose access closure is durable,
// raises the fence again after it restores a volume and leaves it only when the
// repository is reconciled.

func (s *Service) fenceMarker(repositoryID string) string {
	return filepath.Join(s.layout.fences, repositoryID)
}

// loadRestoreFences raises the fence of every repository that has a marker. It
// runs once, synchronously, from New, and fails closed: a directory that cannot
// be read (a volume restored with other ownership, say) means the worker cannot
// know which repositories are fenced, so New fails and nothing is served. An
// entry whose name is a repository ID is a fence whatever its file type.
func (s *Service) loadRestoreFences() error {
	entries, err := os.ReadDir(s.layout.fences)
	if err != nil {
		return fmt.Errorf("gitservice: reading the restore fences: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") { // an interrupted marker write
			_ = os.Remove(filepath.Join(s.layout.fences, name))
			continue
		}
		if !repositoryIDPattern.MatchString(name) {
			continue
		}
		g := s.gates.of(name)
		g.mu.Lock()
		g.restoring = true
		g.mu.Unlock()
		s.cfg.Logger.Warn("the repository comes up fenced for restore", "repositoryId", name)
	}
	return nil
}

// restoreMarked reports whether a marker exists for the repository; a marker
// that cannot be examined is reported as present (closed).
func (s *Service) restoreMarked(repositoryID string) bool {
	_, err := os.Lstat(s.fenceMarker(repositoryID))
	return err == nil || !os.IsNotExist(err)
}

// persistRestoreFence returns the durable step of entering or leaving the fence.
func (s *Service) persistRestoreFence(repositoryID string) func(on bool) error {
	return func(on bool) error {
		path := s.fenceMarker(repositoryID)
		if on {
			if err := writeFileAtomic(path, []byte("restoring\n"), 0o600); err != nil {
				return err
			}
			return syncDir(s.layout.fences)
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		return syncDir(s.layout.fences)
	}
}

// setRestoreFence is the RPC. Both directions are idempotent; entering waits for
// the running receives and operations, leaving for a running replay. A failure
// before the state changed is the 503 unavailable of any retryable refusal.
func (s *Service) setRestoreFence(ctx context.Context, pathID string, req gitv1.RestoreFenceRequest) (gitv1.RestoreFenceResponse, error) {
	if pathID != req.RepositoryID {
		return gitv1.RestoreFenceResponse{}, contractError(domain.CodeInvalidRequest, "repositoryId does not match the path")
	}
	if _, err := s.repoDir(req.RepositoryID); err != nil {
		return gitv1.RestoreFenceResponse{}, err
	}
	changed, err := s.gates.of(req.RepositoryID).setRestoring(ctx, req.Fenced, s.persistRestoreFence(req.RepositoryID), func() bool { return s.restoreMarked(req.RepositoryID) })
	if err != nil {
		return gitv1.RestoreFenceResponse{}, err
	}
	return gitv1.RestoreFenceResponse{RepositoryID: req.RepositoryID, Fenced: req.Fenced, Changed: changed, WorkerInstance: s.instance}, nil
}
