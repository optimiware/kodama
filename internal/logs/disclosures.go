package logs

import (
	"bytes"
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// RunDisclosures supplies the secret values a run disclosed to its OTHER steps
// and services (T-113, VS-14): in a run_shared run a value injected into an
// earlier step or a service can be echoed by a later attempt that was never given
// it (through the shared scratch or a service's reply). The set is read from
// values the run was already given; it grants no new secret-read authority. An
// error fails the read closed.
type RunDisclosures interface {
	Prior(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([][]byte, error)
}

// WithPriorDisclosures returns Redactions that remove the attempt's own injected
// values and every value the run disclosed to its other steps and services.
// Neither dependency is optional: a missing one is a configuration error, never a
// silent narrowing of the redaction.
func WithPriorDisclosures(own Redactions, prior RunDisclosures) (Redactions, error) {
	if own == nil || prior == nil {
		return nil, fmt.Errorf("%w: redactions and run disclosures", ErrNotConfigured)
	}
	return disclosed{own: own, prior: prior}, nil
}

type disclosed struct {
	own   Redactions
	prior RunDisclosures
}

func (d disclosed) Values(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([][]byte, error) {
	own, err := d.own.Values(ctx, scope, attempt)
	if err != nil {
		return nil, err
	}
	prior, err := d.prior.Prior(ctx, scope, attempt)
	if err != nil {
		return nil, fmt.Errorf("%w: run disclosures: %w", ErrUnavailable, err)
	}
	out := make([][]byte, 0, len(own)+len(prior))
	seen := map[string]bool{}
	for _, group := range [][][]byte{own, prior} {
		for _, v := range group {
			if len(v) == 0 || seen[string(v)] {
				continue
			}
			seen[string(v)] = true
			out = append(out, bytes.Clone(v))
		}
	}
	return out, nil
}
