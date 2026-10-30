package secrets

import (
	"context"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// RunBindings names the secret bindings a run disclosed to its OTHER attempts and
// services: the pinned bindings of every other attempt that began a command and of
// every service. The execution module owns that knowledge (T-116 bridges it); this
// package declares the port it consumes.
type RunBindings interface {
	// PriorBindings returns the run of the attempt and the bindings disclosed to
	// everything of the run except that attempt. An attempt that is not part of a
	// run_shared run is reported as run == "" with no bindings.
	PriorBindings(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (run domain.RunID, bindings []runtimev1.SecretBinding, err error)
}

// Disclosures yields the plaintext values the run disclosed to its other steps and
// services, for scanning and redaction only (workspace.RunDisclosures and
// logs.RunDisclosures are satisfied by it). It reads exactly the pinned versions
// the run was already given; it never releases a value to any caller and never
// writes an audit entry (nothing is disclosed to a new subject).
type Disclosures struct {
	bindings RunBindings
	source   VersionSource
}

func (d *Disclosures) open(ctx context.Context, req VersionRequest) (PendingValue, error) {
	if sc, ok := d.source.(ScanSource); ok {
		return sc.OpenForScan(ctx, req)
	}
	return d.source.Open(ctx, req)
}

// NewDisclosures requires both ports.
func NewDisclosures(b RunBindings, s VersionSource) (*Disclosures, error) {
	if b == nil || s == nil {
		return nil, invalidf("run bindings and a version source are required")
	}
	return &Disclosures{bindings: b, source: s}, nil
}

// Prior returns the values, deduplicated, with the secrets' scan/redaction bounds
// left to the consumers. A value an earlier step received stays a value to keep
// out of source and logs after its version is revoked: revocation ends use, not
// the duty to redact. The values are therefore read through the source's scan
// capability (ScanSource), which reads revoked versions and audits each read as a
// scan. A source without it, or any failure, makes the whole answer unavailable
// (the caller refuses the publication or the log read): a partial set would
// silently narrow the scan.
func (d *Disclosures) Prior(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([][]byte, error) {
	run, bindings, err := d.bindings.PriorBindings(ctx, scope, attempt)
	if err != nil {
		return nil, fmt.Errorf("%w: run bindings: %v", ErrUnavailable, err)
	}
	if run == "" || len(bindings) == 0 {
		return [][]byte{}, nil
	}
	seen := map[string]bool{}
	out := make([][]byte, 0, len(bindings))
	for _, b := range bindings {
		key := fmt.Sprintf("%s@%d", b.Ref, b.Version)
		if seen[key] {
			continue
		}
		seen[key] = true
		name, err := ParseSecretRef(b.Ref)
		if err != nil {
			return nil, err
		}
		pending, err := d.open(ctx, VersionRequest{Scope: scope, RunID: run, SecretName: name, Version: b.Version})
		if err != nil {
			return nil, fmt.Errorf("%w: a disclosed value could not be read: %v", ErrUnavailable, err)
		}
		if v := pending.Value(); len(v) > 0 {
			out = append(out, append([]byte(nil), v...))
		}
		pending.Discard()
	}
	return out, nil
}
