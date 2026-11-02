package podman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Unresolved creation requests.
//
// A creation request whose answer was not seen may still be completed by the
// engine, and no amount of waiting proves otherwise. The adapter does not try to
// settle it: it creates no placeholder, deletes nothing it did not create for the
// resource and infers nothing from clocks. While a request is outstanding and its
// object has not been observed, the resource is unresolved: Observe reports
// `unknown` (`create_unresolved`), Stop is not confirmed, Remove is pending and a
// workspace is not released. It resolves when the object is found (it is then
// adopted and handled normally, however late it lands) or when an operator
// explicitly retires the request (RetireUnresolvedRequest).

// holding says what holds an object's name.
type holding int

const (
	heldNone holding = iota
	// heldReal: the object itself (labels and instance UID are the record's).
	heldReal
	// heldForeign: anything else; never adopted, modified or removed.
	heldForeign
)

func labelsMatch(want, got map[string]string) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func labelArgsOf(labels map[string]string) []string {
	var args []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", k+"="+labels[k])
	}
	return args
}

// classify says what an engine object with these labels is to the record.
func (rec resRecord) classify(got map[string]string) holding {
	if labelsMatch(rec.labels(), got) {
		return heldReal
	}
	return heldForeign
}

// lookupName reports what holds the resource's name in the engine.
func (r *Resources) lookupName(ctx context.Context, rec resRecord) (holding, string, error) {
	var labels map[string]string
	var id string
	var found bool
	var err error
	switch rec.Kind {
	case execution.KindNetwork:
		var nw network
		if nw, found, err = r.e.network(ctx, rec.Name); err == nil {
			labels, id = nw.Labels, nw.ID
		}
	case execution.KindWorkspace:
		var vol volume
		if vol, found, err = r.e.volume(ctx, rec.Name); err == nil {
			labels, id = vol.Labels, vol.CreatedAt
		}
	default:
		var ct container
		if ct, found, err = r.e.container(ctx, rec.Name); err == nil {
			labels, id = ct.Config.Labels, ct.ID
		}
	}
	if err != nil || !found {
		return heldNone, "", err
	}
	return rec.classify(labels), id, nil
}

// reconcile resolves, by observation, an announced creation whose object the
// record does not hold: a real object that is ours is adopted and pinned (the
// outstanding request is thereby accounted for), anything else at the name is a
// collision and is never adopted or touched. An absent object leaves the request
// outstanding. It does nothing for a creation that was never announced or whose
// object is already bound.
func (r *Resources) reconcile(ctx context.Context, rec *resRecord) error {
	if rec.Removed || !rec.issued() || rec.Created {
		return nil
	}
	h, id, err := r.lookupName(ctx, *rec)
	if err != nil {
		return err
	}
	switch h {
	case heldReal:
		return r.bind(rec, id)
	case heldForeign:
		return execution.ErrResourceCollision
	}
	return nil
}

// ---- operator retirement ----

const ackFile = "request-retired"

var resourceNamePattern = regexp.MustCompile(`^kr-[0-9a-f]{24}$`)

// RetireUnresolvedRequest is the explicit operator action that ends an unresolved
// creation request. The adapter never infers that a request can no longer arrive
// and never creates a placeholder to find out; a resource whose creation was
// ambiguous stays unresolved (Observe `unknown`/`create_unresolved`, Stop not
// confirmed, Remove pending, a workspace holding a holder with an unresolved
// create not released) until an operator has
//
//  1. made sure no creation request for the resource can still reach the engine
//     (for a remote engine: restart the engine host and stop any client of this
//     installation that might still be running a request), and checked that the
//     engine holds no object of that name that belongs to the run, and
//  2. recorded this retirement, durably, for the resource's name (the name of the
//     run workspace covers the holders of that workspace; a service's name covers
//     its tcp-probe container).
//
// From then on the adapter treats the request as retired and completes the
// cleanup. It is the operator's assertion: an object a retired request creates
// afterwards is not tracked. stateDir is the state directory the adapter was
// configured with.
func RetireUnresolvedRequest(stateDir, name string) error {
	if !resourceNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q is not a run resource name", execution.ErrInvalid, name)
	}
	dir := filepath.Join(stateDir, "resources", name)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%w: the adapter holds no record of %s", execution.ErrNotFound, name)
	}
	// Each retirement advances the epoch of the name: it covers the requests
	// announced before it, never one issued afterwards.
	cur, err := (&resStore{root: filepath.Join(stateDir, "resources")}).epoch(name)
	if err != nil {
		return err
	}
	next := cur + 1
	return writeDurably(dir, filepath.Join(dir, ackFile), []byte(strconv.Itoa(next)+"\n"), os.Rename)
}

// epoch is the number of operator retirements recorded for the name. Only the
// genuine absence of the file means none (0). A file that cannot be read, or whose
// contents are not an epoch, is an error: no request is announced under a
// guessed epoch, and no request is treated as retired by one.
func (s *resStore) epoch(name string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(s.resDir(name), ackFile))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("%w: the retirement epoch of %s is unreadable: %w", execution.ErrUnavailable, name, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: the retirement epoch of %s is malformed", execution.ErrUnavailable, name)
	}
	return n, nil
}

// applyRetirement takes an operator's retirement into the record.
func (r *Resources) applyRetirement(rec *resRecord) error {
	if rec.Unanswered == 0 && (rec.ProbeSlot == nil || rec.ProbeSlot.Unanswered == 0) {
		return nil // nothing outstanding: a retirement has nothing to end
	}
	epoch, err := r.st.epoch(rec.Name)
	if err != nil {
		return err
	}
	changed := false
	if rec.Unanswered != 0 && epoch > rec.Epoch {
		rec.retire()
		changed = true
	}
	if ps := rec.ProbeSlot; ps != nil && ps.Unanswered != 0 && epoch > ps.Epoch {
		ps.retire()
		changed = true
	}
	if changed {
		return r.st.save(*rec)
	}
	return nil
}

// ---- workspace holders ----

// holderMarker is the durable intent of a workload that uses a run workspace
// (a preparation or an attempt): written under the workspace's lock BEFORE its
// create is issued and kept across ambiguous answers and restarts. While a marker
// is unresolved the workspace is not released: the create it announces may still
// land, mount the volume or recreate it by name.
type holderMarker struct {
	Workload string       `json:"workload"` // the container name
	Kind     string       `json:"kind"`
	RunID    domain.RunID `json:"runId"`
	// Labels are the labels of the real container.
	Labels map[string]string `json:"labels"`
	// ID binds the container once its creation was answered.
	ID string `json:"id,omitempty"`
	// Epoch is the operator-retirement epoch of the workspace when the intent was
	// written: a retirement applies only to intents written before it.
	Epoch int `json:"epoch,omitempty"`
}

func (s *resStore) holdersDir(ws string) string { return filepath.Join(s.root, ws, "holders") }

func (s *resStore) holderFile(ws, workload string) string {
	return filepath.Join(s.holdersDir(ws), workload+".json")
}

func (s *resStore) saveHolder(ws string, m holderMarker) error {
	dir := s.holdersDir(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeDurably(dir, s.holderFile(ws, m.Workload), raw, os.Rename)
}

func (s *resStore) dropHolder(ws, workload string) error {
	err := os.Remove(s.holderFile(ws, workload))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *resStore) holders(ws string) ([]holderMarker, error) {
	entries, err := os.ReadDir(s.holdersDir(ws))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: reading the holders of %s: %w", execution.ErrUnavailable, ws, err)
	}
	var out []holderMarker
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.holdersDir(ws), e.Name()))
		var m holderMarker
		if err != nil || json.Unmarshal(raw, &m) != nil || m.Workload+".json" != e.Name() {
			return nil, fmt.Errorf("%w: a holder marker of %s is unreadable", execution.ErrUnavailable, ws)
		}
		out = append(out, m)
	}
	return out, nil
}

// holderHeld reports whether any workload of the workspace can still use it. A
// bound holder is held while its container uses the volume; an unbound one, whose
// create's answer was not seen, is held until the container is found (and bound)
// or an operator retired the request. A foreign object at the container's name is
// not ours and settles nothing: the intent stays. Anything that cannot be
// determined is an error, never "released".
func (r *Resources) holderHeld(ctx context.Context, ws string) (bool, error) {
	markers, err := r.st.holders(ws)
	if err != nil {
		return false, err
	}
	held := false
	for _, m := range markers {
		h, err := r.holderState(ctx, ws, m)
		if err != nil {
			return false, err
		}
		held = held || h
	}
	return held, nil
}

// retiredBy: a retirement of epoch `epoch` ends the intents announced under `under`.
func retiredBy(epoch, under int) bool { return epoch > under }

func (r *Resources) holderState(ctx context.Context, ws string, m holderMarker) (bool, error) {
	if m.ID == "" {
		ct, found, err := r.e.container(ctx, m.Workload)
		if err != nil {
			return false, err
		}
		switch {
		case found && labelsMatch(m.Labels, ct.Config.Labels):
			m.ID = ct.ID // the create landed: bound now
			if err := r.st.saveHolder(ws, m); err != nil {
				return false, err
			}
		case found:
			return true, nil // foreign: not ours, settles nothing
		default:
			// Outstanding: the create may still land, unless an operator retired it. An
			// epoch that cannot be read is neither: the intent stays, with an error.
			epoch, err := r.st.epoch(ws)
			if err != nil {
				return false, err
			}
			if !retiredBy(epoch, m.Epoch) {
				return true, nil
			}
			return false, r.st.dropHolder(ws, m.Workload)
		}
	}
	ct, found, err := r.e.container(ctx, m.ID)
	switch {
	case err != nil:
		return false, err
	case !found:
		return false, r.st.dropHolder(ws, m.Workload) // gone by its bound ID
	case usesVolume(ct.State.Status):
		return true, nil
	}
	return false, r.st.dropHolder(ws, m.Workload)
}
