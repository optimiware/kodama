package gitops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// Why a managed branch was not pruned.
const (
	KeptHeldRef    = "held_ref"    // a run, evidence or deployment record names the branch
	KeptHeldCommit = "held_commit" // such a record pins the branch's tip commit
	KeptUnmerged   = "unmerged"    // the tip is reachable from neither the default branch nor a retained ref
	KeptRetention  = "retention"   // the tip is younger than the retention period
	KeptMoved      = "moved"       // the branch changed since it was read
	KeptBusy       = "busy"        // another pruner holds a live reservation of the branch
)

// PruneResult is what one pruning pass of a repository did.
type PruneResult struct {
	Pruned []string
	// Kept maps each managed branch that was not pruned to why.
	Kept map[string]string
}

// PruneRepository deletes the repository's merged task, agent and run branches
// (GR-13, GR-A09). A branch is pruned only if all hold:
//
//   - nothing references it: its name is not in Held.Refs and its tip is not in
//     Held.Commits (a run in flight, evidence, a deployment);
//   - its tip is reachable from the default branch's tip or from a retained ref
//     (refs/kodama/runs/...), so deleting the name loses no commit: whatever a
//     run pinned stays reachable from its retained ref, which pruning never
//     touches;
//   - its tip commit is older than Options.BranchRetention.
//
// DeleteBranch carries the tip as ExpectedCommit, so a branch that moved
// since it was read is kept (ref_changed), and its operation ID is derived from
// (repository, ref, tip), so a repeat after an unknown outcome is the identical
// request. Archived repositories are never pruned (GR-14). If Held cannot be
// read, nothing is pruned: unknown is never "unreferenced". The reads are
// not atomic with the deletions, which is safe: a deletion only removes a name
// whose commits another ref keeps, and the expected value protects a moved
// branch. At most Options.MaxPrunePerRepository branches are deleted per pass.
func (s *Scheduler) PruneRepository(ctx context.Context, repo Repository) (PruneResult, error) {
	res := PruneResult{Kept: map[string]string{}}
	if repo.Archived {
		return res, nil
	}
	if s.c.Holds == nil || s.c.Objects == nil || s.c.Intents == nil {
		return res, fmt.Errorf("%w: pruning needs the holds, object and prune-intent ports", ErrNotConfigured)
	}
	tenant := repo.Scope.TenantID
	// Never while the tenant is being restored: a branch deleted between a
	// repository's replay and the reconciliation is one the recovery point expects.
	// (The worker refuses server ref operations inside its own restore fence too.)
	if _, err := requireQuiet(ctx, s.c.Fence, s.c.Recoveries, tenant); err != nil {
		return res, err
	}
	held, err := s.c.Holds.Held(ctx, repo.Scope, repo.ID)
	if err != nil {
		return res, fmt.Errorf("%w: what runs and evidence hold: %v", ErrUnavailable, err)
	}
	heads, err := listRefs(ctx, s.c.Worker, tenant, repo.ID, gitv1.HeadsPrefix)
	if err != nil {
		return res, fmt.Errorf("listing branches: %w", err)
	}
	var candidates []gitv1.RefTip
	for _, h := range heads {
		if isManagedBranch(repo.DefaultBranch, h.Ref) {
			candidates = append(candidates, h)
		}
	}
	if len(candidates) == 0 {
		return res, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Ref < candidates[j].Ref })
	defaultTip, _, err := refValue(ctx, s.c.Worker, tenant, repo.ID, gitv1.HeadsPrefix+repo.DefaultBranch)
	if err != nil {
		return res, fmt.Errorf("reading the default branch: %w", err)
	}
	server, err := listRefs(ctx, s.c.Worker, tenant, repo.ID, gitv1.ServerRefPrefix)
	if err != nil {
		return res, fmt.Errorf("listing retained refs: %w", err)
	}
	anchors := map[string]bool{}
	if defaultTip != "" {
		anchors[defaultTip] = true
	}
	for _, r := range server {
		if strings.HasPrefix(r.Ref, gitv1.RetainedRefPrefix) {
			anchors[r.Commit] = true
		}
	}
	anchorList := make([]string, 0, len(anchors))
	for c := range anchors {
		anchorList = append(anchorList, c)
	}
	sort.Strings(anchorList)

	now := s.now()
	var errs []error
	for _, c := range candidates {
		if len(res.Pruned) >= s.o.MaxPrunePerRepository {
			break
		}
		switch {
		case contains(held.Refs, c.Ref):
			res.Kept[c.Ref] = KeptHeldRef
			continue
		case contains(held.Commits, c.Commit):
			res.Kept[c.Ref] = KeptHeldCommit
			continue
		}
		reachable, err := s.reachable(ctx, repo, c.Commit, anchorList)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Ref, err))
			continue
		}
		if !reachable {
			res.Kept[c.Ref] = KeptUnmerged
			continue
		}
		at, err := s.c.Objects.CommitTime(ctx, tenant, repo.ID, c.Commit)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Ref, err))
			continue
		}
		if now.Sub(at) < s.o.BranchRetention {
			res.Kept[c.Ref] = KeptRetention
			continue
		}
		// Reserve the branch (durably) before the holds are read again: a module that
		// takes a hold afterwards sees the intent (PruneIntents), and one that took it
		// before is seen by the fresh read below.
		switch err := s.c.Intents.Declare(ctx, repo.Scope, repo.ID, c.Ref, c.Commit, s.o.Owner, s.o.IntentTTL); {
		case errors.Is(err, ErrPruneBusy):
			res.Kept[c.Ref] = KeptBusy
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: declaring the prune: %w", c.Ref, err))
			continue
		}
		fresh, err := s.c.Holds.Held(ctx, repo.Scope, repo.ID)
		if err != nil {
			_ = s.c.Intents.Release(context.WithoutCancel(ctx), repo.Scope, repo.ID, c.Ref, s.o.Owner)
			errs = append(errs, fmt.Errorf("%s: %w: what runs and evidence hold: %v", c.Ref, ErrUnavailable, err))
			continue
		}
		if contains(fresh.Refs, c.Ref) || contains(fresh.Commits, c.Commit) {
			_ = s.c.Intents.Release(context.WithoutCancel(ctx), repo.Scope, repo.ID, c.Ref, s.o.Owner)
			res.Kept[c.Ref] = KeptHeldRef
			if !contains(fresh.Refs, c.Ref) {
				res.Kept[c.Ref] = KeptHeldCommit
			}
			continue
		}
		// A restore can begin at any moment of a pass: the tenant is checked again
		// immediately before each deletion (not only at the start), so a branch the
		// recovery point expects is not deleted after the recovery began. What
		// remains is the instant between this check and the worker's own refusal
		// inside its restore fence.
		if _, qerr := requireQuiet(ctx, s.c.Fence, s.c.Recoveries, tenant); qerr != nil {
			_ = s.c.Intents.Release(context.WithoutCancel(ctx), repo.Scope, repo.ID, c.Ref, s.o.Owner)
			errs = append(errs, qerr)
			break
		}
		_, err = s.c.Worker.DeleteBranch(ctx, tenant, gitv1.DeleteBranchRequest{
			RepositoryID: repo.ID, OperationID: operationID("prn", string(tenant), repo.ID, c.Ref, c.Commit), Ref: c.Ref, ExpectedCommit: c.Commit,
		})
		var ce *gitv1.ContractError
		switch {
		case err == nil:
			res.Pruned = append(res.Pruned, c.Ref)
			_ = s.c.Intents.Release(context.WithoutCancel(ctx), repo.Scope, repo.ID, c.Ref, s.o.Owner)
		case errors.As(err, &ce) && ce.Code == gitv1.CodeRefChanged:
			res.Kept[c.Ref] = KeptMoved
			_ = s.c.Intents.Release(context.WithoutCancel(ctx), repo.Scope, repo.ID, c.Ref, s.o.Owner)
		default:
			// An unknown outcome may still have deleted the branch: the reservation
			// stays until it expires, and the identical request is repeated next pass.
			errs = append(errs, fmt.Errorf("%s: %w", c.Ref, err))
		}
	}
	return res, errors.Join(errs...)
}

// reachable reports whether the commit is reachable from any anchor. An ancestry
// error on one anchor does not decide: an anchor whose own commit the repository
// lacks is skipped, and only when no anchor answers is it an error.
func (s *Scheduler) reachable(ctx context.Context, repo Repository, commit string, anchors []string) (bool, error) {
	var last error
	answered := false
	for _, a := range anchors {
		ok, err := s.c.Objects.IsAncestor(ctx, repo.Scope.TenantID, repo.ID, commit, a)
		if err != nil {
			last = err
			continue
		}
		answered = true
		if ok {
			return true, nil
		}
	}
	if !answered && last != nil {
		return false, last
	}
	return false, nil
}
