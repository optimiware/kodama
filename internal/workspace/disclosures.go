package workspace

import (
	"bytes"
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// RunDisclosures supplies the secret values a run disclosed to its OTHER steps
// and services (T-113, VS-14). In a run_shared run the scratch survives from one
// step to the next, so a value injected into an earlier step or a service can
// reach a later writer's source without that writer ever having been given it;
// scanning only the attempt's own injections would miss it. The set is read from
// values the run was already given: it grants no new secret-read authority and is
// used only to refuse source, never delivered anywhere.
//
// An attempt that is not part of a run_shared run has none: the answer is empty,
// not an error. Any error fails the proposal closed.
type RunDisclosures interface {
	Prior(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([][]byte, error)
}

// WithPriorDisclosures returns a SecretNeedles that scans for the attempt's own
// injected values and for every value the run disclosed to its other steps and
// services. Neither dependency is optional: a missing one is a configuration
// error, never a silent narrowing of the scan.
func WithPriorDisclosures(own SecretNeedles, prior RunDisclosures) (SecretNeedles, error) {
	if own == nil || prior == nil {
		return nil, fmt.Errorf("%w: secret needles and run disclosures", ErrNotConfigured)
	}
	return disclosed{own: own, prior: prior}, nil
}

type disclosed struct {
	own   SecretNeedles
	prior RunDisclosures
}

func (d disclosed) Needles(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([][]byte, error) {
	own, err := d.own.Needles(ctx, scope, attempt)
	if err != nil {
		return nil, err
	}
	prior, err := d.prior.Prior(ctx, scope, attempt)
	if err != nil {
		return nil, unavailable("run disclosures", err)
	}
	out := make([][]byte, 0, len(own)+len(prior))
	for _, group := range [][][]byte{own, prior} {
		for _, n := range group {
			if len(n) == 0 || contains(out, n) {
				continue
			}
			out = append(out, bytes.Clone(n))
		}
	}
	return out, nil
}

func contains(set [][]byte, v []byte) bool {
	for _, s := range set {
		if bytes.Equal(s, v) {
			return true
		}
	}
	return false
}
