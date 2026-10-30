package kernelrt

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// recState is the lifecycle of one identity in the cluster. It only advances:
// active, then stopped, then removed.
type recState string

const (
	recActive  recState = "active"
	recStopped recState = "stopped"
	recRemoved recState = "removed"
)

func (s recState) rank() int {
	switch s {
	case recActive:
		return 0
	case recStopped:
		return 1
	}
	return 2
}

// record is the fence record of one identity. It is created before the first
// effect on the identity, so a missing record proves this adapter never issued
// an effect for it. Its Secret carries no data.
type record struct {
	secret   kubernetes.Secret
	state    recState
	fence    runtime.Fence
	inflight time.Time // a create was issued and its outcome may be unknown
	volume   string    // the volume a workspace claim was bound to
	created  time.Time
}

func decodeRecord(s kubernetes.Secret) (record, error) {
	a := s.Metadata.Annotations
	r := record{secret: s, state: recState(a[annState])}
	switch r.state {
	case recActive, recStopped, recRemoved:
	default:
		return record{}, fmt.Errorf("kernelrt: record state %q", a[annState])
	}
	f, err := strconv.ParseUint(a[annFence], 10, 64)
	if err != nil || f == 0 {
		return record{}, fmt.Errorf("kernelrt: record fence %q", a[annFence])
	}
	r.fence = runtime.Fence(f)
	if v := a[annInflight]; v != "" {
		t, ok := parseTime(v)
		if !ok {
			return record{}, fmt.Errorf("kernelrt: record in-flight time %q", v)
		}
		r.inflight = t
	}
	r.volume = a[annVolume]
	if t, ok := parseTime(s.Metadata.CreationTimestamp); ok {
		r.created = t
	}
	return r, nil
}

// apply writes the mutable fields into the Secret's annotations.
func (r *record) apply(now time.Time) {
	if r.secret.Metadata.Annotations == nil {
		r.secret.Metadata.Annotations = map[string]string{}
	}
	a := r.secret.Metadata.Annotations
	a[annState] = string(r.state)
	a[annFence] = strconv.FormatUint(uint64(r.fence), 10)
	a[annSettled] = formatTime(now)
	setOrDelete(a, annInflight, r.inflight)
	if r.volume == "" {
		delete(a, annVolume)
	} else {
		a[annVolume] = r.volume
	}
}

func setOrDelete(a map[string]string, k string, t time.Time) {
	if t.IsZero() {
		delete(a, k)
		return
	}
	a[k] = formatTime(t)
}

// quiet reports whether no create of this identity can still be in flight.
func (r record) quiet(now time.Time, quiescence time.Duration) bool {
	return r.inflight.IsZero() || !now.Before(r.inflight.Add(quiescence))
}

// readRecord returns the record for id. found is false only for a definite
// absence; any other read failure is returned. A record that is not this
// identity's is ErrForeign, one for another specification ErrSpecMismatch.
func (c *core) readRecord(ctx context.Context, ns string, id ident) (record, bool, error) {
	s, err := c.cluster.GetSecret(ctx, ns, id.recordName())
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return record{}, false, nil
	case err != nil:
		return record{}, false, fmt.Errorf("kernelrt: reading the record: %w", err)
	}
	if !id.owns(s.Metadata.Labels, s.Metadata.Annotations) || s.Metadata.Labels[labelRole] != roleRecord {
		return record{}, false, fmt.Errorf("%w: record %s", ErrForeign, s.Metadata.Name)
	}
	if !id.digestMatches(s.Metadata.Annotations) {
		return record{}, false, fmt.Errorf("%w: the identity exists with another specification digest", runtime.ErrSpecMismatch)
	}
	r, err := decodeRecord(s)
	if err != nil {
		return record{}, false, err
	}
	return r, true, nil
}

func (c *core) newRecord(id ident, fence runtime.Fence, state recState) record {
	now := c.opts.Clock()
	r := record{state: state, fence: fence}
	r.secret = kubernetes.Secret{
		Metadata: kubernetes.ObjectMeta{Name: id.recordName(), Labels: id.labels(roleRecord), Annotations: id.annotations()},
		Type:     "Opaque",
	}
	r.apply(now)
	return r
}

// write conditionally replaces the record under its resourceVersion.
func (c *core) write(ctx context.Context, ns string, r record) (record, error) {
	r.apply(c.opts.Clock())
	out, err := c.cluster.UpdateSecret(ctx, ns, r.secret)
	if err != nil {
		return record{}, err
	}
	r.secret = out
	return r, nil
}

const recordRetries = 4

// claim records that the writer with this fence intends an effect on id,
// creating the record first. A stopped or removed identity is never claimed
// again; an older fence than the record's is stale.
func (c *core) claim(ctx context.Context, ns string, id ident, fence runtime.Fence) (record, error) {
	for range recordRetries {
		r, found, err := c.readRecord(ctx, ns, id)
		if err != nil {
			return record{}, err
		}
		if !found {
			nr := c.newRecord(id, fence, recActive)
			out, err := c.cluster.CreateSecret(ctx, ns, nr.secret)
			switch {
			case err == nil:
				nr.secret = out
				return nr, nil
			case errors.Is(err, kubernetes.ErrAlreadyExists), errors.Is(err, kubernetes.ErrUnknownOutcome):
				continue // read it back
			}
			return record{}, fmt.Errorf("kernelrt: creating the record: %w", err)
		}
		if r.state != recActive {
			return record{}, ErrClosed
		}
		if fence < r.fence {
			return record{}, fmt.Errorf("%w: held %d, record %d", runtime.ErrStaleFence, fence, r.fence)
		}
		if fence == r.fence {
			return r, nil
		}
		r.fence = fence
		out, err := c.write(ctx, ns, r)
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, kubernetes.ErrConflict):
			continue
		}
		return record{}, fmt.Errorf("kernelrt: raising the record fence: %w", err)
	}
	return record{}, fmt.Errorf("kernelrt: the record kept changing: %w", kubernetes.ErrConflict)
}

// settle advances the record to the given state, raising its fence, and
// creates a tombstone when none exists so a delayed create is refused. A stale
// writer is refused with runtime.ErrStaleFence and nothing changes.
func (c *core) settle(ctx context.Context, ns string, id ident, fence runtime.Fence, to recState) (record, error) {
	for range recordRetries {
		r, found, err := c.readRecord(ctx, ns, id)
		if err != nil {
			return record{}, err
		}
		if !found {
			nr := c.newRecord(id, fence, to)
			out, err := c.cluster.CreateSecret(ctx, ns, nr.secret)
			switch {
			case err == nil:
				nr.secret = out
				return nr, nil
			case errors.Is(err, kubernetes.ErrAlreadyExists), errors.Is(err, kubernetes.ErrUnknownOutcome):
				continue
			}
			return record{}, fmt.Errorf("kernelrt: creating the record: %w", err)
		}
		if fence < r.fence {
			return record{}, fmt.Errorf("%w: held %d, record %d", runtime.ErrStaleFence, fence, r.fence)
		}
		if fence == r.fence && r.state.rank() >= to.rank() {
			return r, nil
		}
		r.fence = fence
		if to.rank() > r.state.rank() {
			r.state = to
		}
		out, err := c.write(ctx, ns, r)
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, kubernetes.ErrConflict):
			continue
		}
		return record{}, fmt.Errorf("kernelrt: advancing the record: %w", err)
	}
	return record{}, fmt.Errorf("kernelrt: the record kept changing: %w", kubernetes.ErrConflict)
}

// mutate re-reads the record and applies f, retrying a lost race. It never
// changes the state or fence except through f.
func (c *core) mutate(ctx context.Context, ns string, id ident, f func(*record)) (record, error) {
	for range recordRetries {
		r, found, err := c.readRecord(ctx, ns, id)
		if err != nil {
			return record{}, err
		}
		if !found {
			return record{}, fmt.Errorf("kernelrt: the record of %s is gone", id.base())
		}
		f(&r)
		out, err := c.write(ctx, ns, r)
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, kubernetes.ErrConflict):
			continue
		}
		return record{}, err
	}
	return record{}, fmt.Errorf("kernelrt: the record kept changing: %w", kubernetes.ErrConflict)
}

// markInflight records that a create is about to be issued. It must succeed
// before the create, so a lost create is never mistaken for absence.
func (c *core) markInflight(ctx context.Context, ns string, id ident) error {
	now := c.opts.Clock()
	_, err := c.mutate(ctx, ns, id, func(r *record) { r.inflight = now })
	return err
}

// clearInflight records that the create's outcome is now known. Failing to do
// so is harmless: the marker only delays confirmation of absence.
func (c *core) clearInflight(ctx context.Context, ns string, id ident) {
	_, _ = c.mutate(ctx, ns, id, func(r *record) { r.inflight = time.Time{} })
}

// deleteRecord removes a record by UID. Only the reconciler does this, for a
// removed identity past its retention.
func (c *core) deleteRecord(ctx context.Context, ns string, r record) error {
	err := c.cluster.DeleteSecret(ctx, ns, r.secret.Metadata.Name, kubernetes.DeleteOptions{UID: r.secret.Metadata.UID})
	if errors.Is(err, kubernetes.ErrNotFound) {
		return nil
	}
	return err
}
