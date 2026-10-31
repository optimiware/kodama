package kernelrt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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

// record is the durable evidence of one identity. It is created before the
// first effect on the identity, so a missing record proves this adapter never
// issued one. It is never deleted: a removed identity keeps its closure state
// and highest fence, which is what rejects a delayed create from an older
// writer at any later time. Its Secret carries no data.
type record struct {
	secret kubernetes.Secret
	state  recState
	fence  runtime.Fence
	// markers are the creates that were issued and may still land, one per
	// reservation, keyed "<operation>.<invocation>". A reservation is cleared
	// only by the invocation that made it, and only when its outcome is known,
	// so neither another operation nor an overlapping invocation of the same
	// operation can erase its uncertainty.
	markers map[string]time.Time
	// created holds the UID of the object each operation established. A missing
	// object with a recorded UID was lost; it is never silently recreated.
	created map[string]string
	// reaping holds, per operation, the UID of an object whose creator has
	// claimed the right to delete it because the identity closed before anyone
	// established it. While it is set no one may establish that instance, so
	// "established by a newer writer" and "being reaped by its creator" exclude
	// each other through the record's version check.
	reaping map[string]string
	// workspace is the workspace claim this workload was started against.
	workspace string
	// secretsDir is where the step's declared secrets are mounted; empty when
	// the step declared none. It is committed intent: the adopted document is
	// checked against it, never the other way round.
	secretsDir string
	// volumes are the volumes a workspace claim was seen bound to or found
	// through its claimRef. They are persisted because the claim, and later the
	// volume object, can disappear while the volume is still attached.
	volumes []string
	// compensated are the UIDs of workspace claims this adapter created and then
	// deleted as unwanted (a conflicting or revoked create). They are cleanup
	// evidence only, never the authoritative established UID: their volumes are
	// still found through claimRef after the claims are gone.
	compensated []string
	// claimGone is when the workspace claim was first observed gone.
	claimGone time.Time
	createdAt time.Time
}

func decodeRecord(s kubernetes.Secret) (record, error) {
	a := s.Metadata.Annotations
	r := record{secret: s, state: recState(a[annState]), markers: map[string]time.Time{}, created: map[string]string{}, reaping: map[string]string{}}
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
	for k, v := range a {
		switch {
		case strings.HasPrefix(k, annMarkerPrefix):
			t, ok := parseTime(v)
			if !ok {
				return record{}, fmt.Errorf("kernelrt: record marker %s %q", k, v)
			}
			r.markers[strings.TrimPrefix(k, annMarkerPrefix)] = t
		case strings.HasPrefix(k, annCreatedPrefix):
			r.created[strings.TrimPrefix(k, annCreatedPrefix)] = v
		case strings.HasPrefix(k, annReapingPrefix):
			r.reaping[strings.TrimPrefix(k, annReapingPrefix)] = v
		}
	}
	if v := a[annClaimGone]; v != "" {
		t, ok := parseTime(v)
		if !ok {
			return record{}, fmt.Errorf("kernelrt: record claim time %q", v)
		}
		r.claimGone = t
	}
	r.workspace, r.secretsDir = a[annWorkspace], a[annSecretsDir]
	if v := a[annVolumes]; v != "" {
		r.volumes = strings.Split(v, ",")
	}
	if v := a[annCompensated]; v != "" {
		r.compensated = strings.Split(v, ",")
	}
	if t, ok := parseTime(s.Metadata.CreationTimestamp); ok {
		r.createdAt = t
	}
	return r, nil
}

// apply writes the mutable fields into the Secret's annotations.
func (r *record) apply(now time.Time) {
	if r.secret.Metadata.Annotations == nil {
		r.secret.Metadata.Annotations = map[string]string{}
	}
	a := r.secret.Metadata.Annotations
	for k := range a {
		if strings.HasPrefix(k, annMarkerPrefix) || strings.HasPrefix(k, annCreatedPrefix) || strings.HasPrefix(k, annReapingPrefix) {
			delete(a, k)
		}
	}
	a[annState] = string(r.state)
	a[annFence] = strconv.FormatUint(uint64(r.fence), 10)
	a[annSettled] = formatTime(now)
	for op, t := range r.markers {
		a[annMarkerPrefix+op] = formatTime(t)
	}
	for op, uid := range r.created {
		a[annCreatedPrefix+op] = uid
	}
	for op, uid := range r.reaping {
		a[annReapingPrefix+op] = uid
	}
	setOrDelete(a, annClaimGone, r.claimGone)
	setOrEmpty(a, annVolumes, strings.Join(r.volumes, ","))
	setOrEmpty(a, annCompensated, strings.Join(r.compensated, ","))
	setOrEmpty(a, annWorkspace, r.workspace)
	setOrEmpty(a, annSecretsDir, r.secretsDir)
}

func setOrDelete(a map[string]string, k string, t time.Time) {
	if t.IsZero() {
		delete(a, k)
		return
	}
	a[k] = formatTime(t)
}

func setOrEmpty(a map[string]string, k, v string) {
	if v == "" {
		delete(a, k)
		return
	}
	a[k] = v
}

// quiet reports whether no issued create of any operation can still land.
func (r record) quiet(now time.Time, quiescence time.Duration) bool {
	for _, t := range r.markers {
		if now.Before(t.Add(quiescence)) {
			return false
		}
	}
	return true
}

// marked reports whether any reservation of op is recorded, current or not.
func (r record) marked(op string) bool {
	for k := range r.markers {
		if strings.HasPrefix(k, op+".") {
			return true
		}
	}
	return false
}

// addVolumes records volume names not yet recorded.
func (r *record) addVolumes(names ...string) {
	for _, n := range names {
		if n != "" && !slices.Contains(r.volumes, n) {
			r.volumes = append(r.volumes, n)
		}
	}
	slices.Sort(r.volumes)
}

// addCompensated records the UID of a claim that is about to be deleted as
// unwanted.
func (r *record) addCompensated(uid string) {
	if uid != "" && !slices.Contains(r.compensated, uid) {
		r.compensated = append(r.compensated, uid)
		slices.Sort(r.compensated)
	}
}

// revoked reports whether a writer holding fence no longer has authority: the
// identity was stopped or removed, or a newer writer took over.
func (r record) revoked(fence runtime.Fence) error {
	switch {
	case r.state != recActive:
		return ErrClosed
	case fence < r.fence:
		return fmt.Errorf("%w: held %d, record %d", runtime.ErrStaleFence, fence, r.fence)
	}
	return nil
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
	r := record{state: state, fence: fence, markers: map[string]time.Time{}, created: map[string]string{}, reaping: map[string]string{}}
	r.secret = kubernetes.Secret{
		Metadata: kubernetes.ObjectMeta{Name: id.recordName(), Labels: id.labels(roleRecord), Annotations: id.annotations()},
		Type:     "Opaque",
	}
	r.apply(now)
	return r
}

// write conditionally replaces the record under its resourceVersion.
func (c *core) write(ctx context.Context, ns string, r record) (record, error) {
	now := c.opts.Clock()
	// A reservation long past its quiescence window no longer says anything the
	// clock does not; dropping it keeps the record small.
	for k, t := range r.markers {
		if !now.Before(t.Add(10 * c.opts.CreateQuiescence)) {
			delete(r.markers, k)
		}
	}
	r.apply(now)
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
		if err := r.revoked(fence); err != nil {
			return record{}, err
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
// writer is refused with runtime.ErrStaleFence and nothing changes. Markers
// are preserved: closing an identity never erases the evidence that a create
// may still land.
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
		if to == recRemoved && !r.quiet(c.opts.Clock(), c.opts.CreateQuiescence) {
			return r, errNotQuiet
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

// errNotQuiet: closing was refused because a create may still land.
var errNotQuiet = errors.New("kernelrt: a create may still land")

// mutate re-reads the record and applies f, retrying a lost race. f may return
// an error to abandon the change.
func (c *core) mutate(ctx context.Context, ns string, id ident, f func(*record) error) (record, error) {
	for range recordRetries {
		r, found, err := c.readRecord(ctx, ns, id)
		if err != nil {
			return record{}, err
		}
		if !found {
			return record{}, fmt.Errorf("kernelrt: the record of %s is gone", id.base())
		}
		if err := f(&r); err != nil {
			return r, err
		}
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

// reserve conditionally records that a create of op is about to be issued. It
// succeeds only while the record is active and the writer's fence is current,
// and it is one version-checked write: a Stop or Remove from a newer writer
// either lands before it (and the create is refused) or after it (and the
// create is then revoked). The reservation is unique to this invocation; the
// returned token settles exactly it.
func (c *core) reserve(ctx context.Context, ns string, id ident, fence runtime.Fence, op string, mod func(*record) error) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := op + "." + hex.EncodeToString(b[:])
	_, err := c.mutate(ctx, ns, id, func(r *record) error {
		if err := r.revoked(fence); err != nil {
			return err
		}
		// Committed intent is validated and initialized here, in the same
		// version-checked write as the reservation: what an earlier writer
		// committed is never overwritten.
		if mod != nil {
			if err := mod(r); err != nil {
				return err
			}
		}
		r.markers[token] = c.opts.Clock()
		r.fence = max(r.fence, fence)
		return nil
	})
	return token, err
}

// checkEstablish is the one rule every version-checked write that records an
// established instance applies: the instance may not be under a reaping claim
// and may not differ from the one already recorded.
func (r record) checkEstablish(op, uid string) error {
	if got := r.created[op]; got != "" && got != uid {
		return unverified("the %s was replaced by another instance", op)
	}
	if r.reaping[op] == uid {
		return unverified("the %s is being deleted by its creator", op)
	}
	return nil
}

// settleMarker records that the reserved create has a known outcome and clears
// that reservation only. established, when set, records the instance it
// established; the exclusion and UID-consistency rules apply inside this same
// version-checked write, so an instance whose creator has claimed it for
// reaping, or that differs from the recorded one, is not recorded. The
// reservation is cleared either way, and the refusal is reported. reaped clears
// a reaping claim.
func (c *core) settleMarker(ctx context.Context, ns string, id ident, op, token, established, reaped string) error {
	var refused error
	_, err := c.mutate(ctx, ns, id, func(r *record) error {
		refused = nil
		delete(r.markers, token)
		if established != "" {
			if err := r.checkEstablish(op, established); err != nil {
				refused = err
			} else {
				r.created[op] = established
			}
		}
		if reaped != "" && r.reaping[op] == reaped {
			delete(r.reaping, op)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return refused
}

// errNoReap: the identity is not closed or someone established the instance.
var errNoReap = errors.New("kernelrt: the instance is not the creator's to delete")

// claimReap takes the right to delete the instance uid a closed identity's
// creator made. It succeeds only while the identity is closed and no one has
// established or adopted any instance of op, in one version-checked write; a
// writer that establishes the instance first makes it fail, and once it
// succeeds establishing that instance is refused. It reports whether the caller
// may delete.
func (c *core) claimReap(ctx context.Context, ns string, id ident, op, uid string) (bool, error) {
	_, err := c.mutate(ctx, ns, id, func(r *record) error {
		if r.state == recActive || r.created[op] != "" {
			return errNoReap
		}
		if r.reaping[op] != "" && r.reaping[op] != uid {
			return errNoReap
		}
		r.reaping[op] = uid
		return nil
	})
	if errors.Is(err, errNoReap) {
		return false, nil
	}
	return err == nil, err
}

// establish records the UID of an object this identity found or created. A
// different UID than the recorded one means the object was replaced.
func (c *core) establish(ctx context.Context, ns string, id ident, rec record, op, uid string) error {
	if got := rec.created[op]; got == uid {
		return nil
	} else if got != "" {
		return unverified("the %s was replaced by another instance", op)
	}
	_, err := c.mutate(ctx, ns, id, func(r *record) error {
		if err := r.checkEstablish(op, uid); err != nil {
			return err
		}
		r.created[op] = uid
		return nil
	})
	return err
}

// checkInstance verifies a live object is the instance this identity
// established, when one was recorded.
func checkInstance(rec record, found bool, op, uid string) error {
	if found {
		if got := rec.created[op]; got != "" && got != uid {
			return unverified("the %s was replaced by another instance", op)
		}
	}
	return nil
}

// current re-reads the record and checks that the writer still has authority.
// An operation that paused between its first read and its effect calls it
// again, so it never decides on a record that a newer writer has since changed.
func (c *core) current(ctx context.Context, ns string, id ident, fence runtime.Fence) (record, error) {
	r, found, err := c.readRecord(ctx, ns, id)
	switch {
	case err != nil:
		return record{}, err
	case !found:
		return record{}, fmt.Errorf("kernelrt: the record of %s is gone", id.base())
	}
	return r, r.revoked(fence)
}

// guard issues one create under the reservation protocol.
type guard struct {
	op    string
	fence runtime.Fence
	// mod validates and initializes committed intent inside the reservation.
	mod func(*record) error
	// create issues the create and returns the new object's UID.
	create func() (string, error)
	// readBack reads an object that already holds the name, verifies it is this
	// identity's and returns its UID.
	readBack func() (string, error)
	// reap deletes exactly the instance with the given UID, if it is still the
	// one occupying the name; it never removes by identity.
	reap func(uid string) error
}

// guarded runs g: reserve, create, then recheck authority. A refused
// reservation, or a definite refusal by the cluster, is runtime.ErrNotReached.
// An unknown outcome leaves the reservation and is uncertain.
//
// After the create, authority is rechecked. A newer writer that took over while
// the identity stays active owns whatever now holds the name: this writer
// reports itself stale and touches nothing. Only when the identity was closed
// (stopped or removed), and only for an object this very call created, is the
// object deleted; an object adopted through AlreadyExists was not created here
// and is left to the Stop or Remove that closed the identity.
func (c *core) guarded(ctx context.Context, ns string, id ident, g guard) error {
	token, err := c.reserve(ctx, ns, id, g.fence, g.op, g.mod)
	if err != nil {
		return notReached(err)
	}
	uid, err := g.create()
	createdHere := false
	switch {
	case err == nil:
		createdHere = true
	case errors.Is(err, kubernetes.ErrAlreadyExists):
		u, rerr := g.readBack()
		if rerr != nil {
			if errors.Is(rerr, runtime.ErrNotReached) {
				// The name is held by something else: this create is settled.
				if serr := c.settleMarker(ctx, ns, id, g.op, token, "", ""); serr != nil {
					return uncertain(errors.Join(rerr, serr))
				}
			}
			return rerr
		}
		uid = u
	case kubernetes.Definite(err):
		if serr := c.settleMarker(ctx, ns, id, g.op, token, "", ""); serr != nil {
			return uncertain(errors.Join(err, serr))
		}
		return notReached(fmt.Errorf("the cluster refused the %s: %w", g.op, err))
	default:
		return uncertain(err)
	}
	rec, found, err := c.readRecord(ctx, ns, id)
	if err != nil || !found {
		return uncertain(errors.Join(err, errors.New("the record cannot be read after the create")))
	}
	if rerr := rec.revoked(g.fence); rerr != nil {
		closed := rec.state != recActive
		established, reaped := "", ""
		switch {
		case closed && createdHere && g.reap != nil && uid != "":
			// Compensate only for the instance this call created, and only if the
			// record still shows nobody established it: the claim and a newer
			// writer's establishment are mutually exclusive.
			ok, cerr := c.claimReap(ctx, ns, id, g.op, uid)
			if cerr != nil {
				return errors.Join(rerr, uncertain(cerr))
			}
			if ok {
				if derr := g.reap(uid); derr != nil {
					return errors.Join(rerr, uncertain(derr))
				}
				reaped = uid
			}
		case !closed:
			established = uid // a newer writer's object; recorded, never deleted
		}
		if serr := c.settleMarker(ctx, ns, id, g.op, token, established, reaped); serr != nil {
			return errors.Join(rerr, uncertain(serr))
		}
		return rerr
	}
	if err := c.settleMarker(ctx, ns, id, g.op, token, uid, ""); err != nil {
		if createdHere {
			// This call's create was applied. A refused establishment does not
			// undo it, so it is never "not reached". The instance conflicts with
			// the one the record established, which is exclusively this call's to
			// account for: delete it by its UID (never by identity) so a lost
			// object is not silently replaced.
			var derr error
			if g.reap != nil && uid != "" && errors.Is(err, ErrUnverified) {
				derr = g.reap(uid)
			}
			return uncertain(errors.Join(err, derr))
		}
		if errors.Is(err, ErrUnverified) {
			return notReached(err) // adopted, not created: no effect of this call
		}
		return uncertain(err)
	}
	return nil
}
