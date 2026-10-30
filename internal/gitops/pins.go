package gitops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// PinsConfig holds the collaborators of Pins. Gate decides when a run's pins
// may be released; without it every release is refused (ErrRetention).
type PinsConfig struct {
	Worker Worker
	Gate   ReleaseGate
}

// Pins keeps commits reachable until retention releases them (GR-13): each pin
// is the hidden ref refs/kodama/runs/<runId>/<n> in the repository itself, so the
// retained refs are the record (listed back with ReadRefs) and no table is
// needed. A commit a retained ref names survives pruning and git maintenance.
type Pins struct{ c PinsConfig }

// NewPins requires a worker.
func NewPins(c PinsConfig) (*Pins, error) {
	if c.Worker == nil {
		return nil, fmt.Errorf("%w: a worker is required", ErrNotConfigured)
	}
	return &Pins{c: c}, nil
}

// runPins lists the run's retained refs ordered by their number.
func (p *Pins) runPins(ctx context.Context, tenant domain.TenantID, repositoryID string, runID domain.RunID) ([]gitv1.RefTip, int64, error) {
	all, err := listRefs(ctx, p.c.Worker, tenant, repositoryID, gitv1.ServerRefPrefix)
	if err != nil {
		return nil, 0, err
	}
	prefix := gitv1.RetainedRefPrefix + string(runID) + "/"
	var out []gitv1.RefTip
	var highest int64
	for _, r := range all {
		if !strings.HasPrefix(r.Ref, prefix) {
			continue
		}
		id, n, err := gitv1.ParseRetainedRef(r.Ref)
		if err != nil || id != string(runID) {
			continue
		}
		out = append(out, r)
		highest = max(highest, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, highest, nil
}

// Pin creates the next retained ref of the run for the commit and returns it;
// it is idempotent per (run, commit): the commit already pinned returns its ref.
// The operation ID derives from (repository, run, number, commit), so a repeat
// after an unknown outcome is the identical request; two pins racing for one
// number resolve through the worker's retained_ref_exists, and the loser takes
// the next number.
func (p *Pins) Pin(ctx context.Context, scope domain.Scope, repositoryID string, runID domain.RunID, commit string) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if err := gitv1.ValidateObjectID(commit); err != nil || gitv1.IsZeroObjectID(commit) {
		return "", errors.New("gitops: the commit to pin is not an object ID")
	}
	for range 5 {
		pins, highest, err := p.runPins(ctx, scope.TenantID, repositoryID, runID)
		if err != nil {
			return "", err
		}
		for _, pin := range pins {
			if pin.Commit == commit {
				return pin.Ref, nil
			}
		}
		ref, err := gitv1.RetainedRef(string(runID), highest+1)
		if err != nil {
			return "", err
		}
		_, err = p.c.Worker.SetRetainedRef(ctx, scope.TenantID, gitv1.SetRetainedRefRequest{
			RepositoryID: repositoryID, OperationID: operationID("pin", string(scope.TenantID), repositoryID, ref, commit), Ref: ref, Commit: commit,
		})
		var ce *gitv1.ContractError
		switch {
		case err == nil:
			return ref, nil
		case errors.As(err, &ce) && ce.Code == gitv1.CodeRetainedRefExists:
			continue // another pin took the number: read again
		default:
			return "", err
		}
	}
	return "", fmt.Errorf("%w: the retained ref numbers of the run keep changing", ErrUnavailable)
}

// Release deletes the run's retained refs once retention allows, each with its
// value as the expected commit (idempotent: a ref already gone is success).
// Without a gate, or while the gate says no, nothing is deleted (ErrRetention).
// A merged branch is pruned separately, by PruneRepository, and only after its
// tip is reachable from a retained ref or the default branch.
func (p *Pins) Release(ctx context.Context, scope domain.Scope, repositoryID string, runID domain.RunID) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if p.c.Gate == nil {
		return fmt.Errorf("%w: no retention gate is configured", ErrRetention)
	}
	ok, err := p.c.Gate.MayRelease(ctx, scope, runID)
	if err != nil {
		return fmt.Errorf("%w: retention could not be established: %v", ErrRetention, err)
	}
	if !ok {
		return fmt.Errorf("%w: the run's retention has not elapsed or a hold applies", ErrRetention)
	}
	pins, _, err := p.runPins(ctx, scope.TenantID, repositoryID, runID)
	if err != nil {
		return err
	}
	var errs []error
	for _, pin := range pins {
		_, err := p.c.Worker.DeleteRetainedRef(ctx, scope.TenantID, gitv1.DeleteRetainedRefRequest{
			RepositoryID: repositoryID, OperationID: operationID("rel", string(scope.TenantID), repositoryID, pin.Ref, pin.Commit), Ref: pin.Ref, ExpectedCommit: pin.Commit,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pin.Ref, err))
		}
	}
	return errors.Join(errs...)
}
