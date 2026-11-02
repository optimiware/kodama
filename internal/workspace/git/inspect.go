package git

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/workspace"
)

var _ workspace.PushInspector = (*Reader)(nil)

// Inspect implements workspace.PushInspector over the internal host's smart HTTP
// endpoint with a read-only credential (the checkout reference of the
// repository). A push that applied is already on the host, so the check reads
// what is there rather than trusting the pusher: it fetches the task branch into
// bounded private scratch, finds the pushed commit in that history, relates it to
// the commit accepted before it, and, for a fast-forward, scans every commit,
// tree and blob reachable from the pushed commit and not from the accepted one
// for the attempt's injected secret values and those the run disclosed to its
// other steps (ErrSecretInSource). Nothing is written to the host or to any
// repository directory but the scratch, which is removed on return.
//
// A pushed commit that cannot be found (the branch moved away from it, or was
// deleted) or an accepted commit that is gone is not a finding: the first is
// unavailable evidence, the second a divergence.
func (r *Reader) Inspect(ctx context.Context, req workspace.InspectRequest) (workspace.Relation, error) {
	if err := validateRepo(req.Repo); err != nil {
		return "", err
	}
	if err := workspace.ValidateBranch(req.Ref); err != nil {
		return "", err
	}
	if err := req.Commit.Validate(); err != nil {
		return "", err
	}
	if err := req.Since.Validate(); err != nil {
		return "", err
	}
	if len(req.Since) != len(req.Commit) {
		return "", fmt.Errorf("%w: commit and since use different object formats", workspace.ErrInvalidValue)
	}
	cred, err := r.e.credential(ctx, req.Repo, req.Repo.CheckoutSecretRef)
	if err != nil {
		return "", err
	}
	sc, err := r.e.newScratch()
	if err != nil {
		return "", err
	}
	defer sc.cleanup()

	_, exists, err := r.e.lsRemote(ctx, sc, req.Repo, cred, req.Ref)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%w: the branch does not exist", workspace.ErrUnavailable)
	}
	dir, err := r.e.initBare(ctx, sc, req.Commit)
	if err != nil {
		return "", err
	}
	if err := r.e.fetchBranch(ctx, sc, dir, req.Repo, cred, req.Ref, 0); err != nil {
		return "", err
	}
	have := func(c workspace.Commit) (bool, error) {
		ok, err := r.e.hasCommit(ctx, sc, dir, c)
		if err != nil || ok {
			return ok, err
		}
		// Not in the branch's history: it may still be an object of the host.
		if ferr := r.e.fetchCommit(ctx, sc, dir, req.Repo, cred, c); ferr != nil {
			if errors.Is(ferr, workspace.ErrLimitExceeded) {
				return false, ferr
			}
			return false, nil
		}
		return r.e.hasCommit(ctx, sc, dir, c)
	}
	if ok, err := have(req.Commit); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("%w: the pushed commit is not readable on the host", workspace.ErrUnavailable)
	}
	if ok, err := have(req.Since); err != nil {
		return "", err
	} else if !ok {
		return workspace.RelationDiverged, nil
	}

	contained, err := r.e.isAncestor(ctx, sc, dir, req.Commit, req.Since)
	if err != nil {
		return "", err
	}
	if contained {
		return workspace.RelationContained, nil
	}
	descends, err := r.e.isAncestor(ctx, sc, dir, req.Since, req.Commit)
	if err != nil {
		return "", err
	}
	if !descends {
		return workspace.RelationDiverged, nil
	}
	if err := r.e.scanNewObjects(ctx, sc, dir, req.Commit, req.Since, req.Needles); err != nil {
		var be *workspace.BundleError
		if errors.As(err, &be) {
			return "", fmt.Errorf("%w: reading the pushed objects: %s", workspace.ErrUnavailable, be.Reason)
		}
		return "", err
	}
	return workspace.RelationDescends, nil
}
