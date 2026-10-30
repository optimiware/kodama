package gitservice

import (
	"context"
	"sync"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// gate is the per-repository mutation lock. Receives and server-side ref
// operations hold it shared (Git's ref locks and the journal's file lock
// serialize them against each other); maintenance holds it exclusively, so it
// never overlaps a receive and a receive never starts during it. The locked
// reconciliation read (snapshot) takes neither side: it holds the gate's mutex,
// so no receive or operation can start while the values are read, and reports
// whether any was in progress (the contract's Quiescent).
//
// A waiting exclusive holder blocks new shared holders, so maintenance cannot
// be starved by a stream of receives.
type gate struct {
	// opMu serializes the server-side ref operations of the repository, so a
	// retry of an operation never closes the journal entry of its still-running
	// duplicate.
	opMu sync.Mutex

	mu        sync.Mutex
	shared    int
	exclusive bool
	waiting   int
	// observers counts the shared holders that only read (bundle creation): they
	// never change a ref, so the quiescence reported to them ignores each other.
	observers int
	// epoch counts admissions of holders that may change the repository (every
	// shared holder but an observer, and every exclusive holder), so a reader can
	// tell that nothing was admitted while it worked.
	epoch uint64
	// restoring is the restore fence (SetRestoreFence): while set, receives,
	// server ref operations and maintenance are refused and only the restore
	// replay is admitted. It is mirrored by a marker file under the data root
	// (restorefence.go), written before the flag is set and removed before it is
	// cleared, so a replacement worker starts fenced for a repository whose
	// fence was raised.
	restoring bool
	dirty     bool           // a Git process was terminated: stale locks may remain
	fenced    map[int]string // process groups that ended (or were killed) but are not verified gone: pgid to receive ID ("" for an operation)
	changed   chan struct{}  // closed and replaced whenever the state changes
}

// markDirty records that a terminated receive may have left lock files behind.
func (g *gate) markDirty() {
	g.mu.Lock()
	g.dirty = true
	g.mu.Unlock()
}

// errFenced is the refusal while a terminated Git process group of the
// repository is not yet verified gone: nothing may mutate the repository (or be
// repaired) until it is, because the group may still hold ref locks.
var errFenced = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "the repository is recovering from a terminated Git process; retry shortly"}

// fence records a group that is not verified gone.
func (g *gate) fence(pgid int, receiveID string) {
	g.mu.Lock()
	if g.fenced == nil {
		g.fenced = map[int]string{}
	}
	g.fenced[pgid] = receiveID
	g.dirty = true
	g.notify()
	g.mu.Unlock()
}

// unfence records that a group is verified gone and reports whether the
// repository is no longer fenced.
func (g *gate) unfence(pgid int) (clear bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fenced, pgid)
	g.notify()
	return len(g.fenced) == 0
}

// checkFence returns errFenced while the repository is fenced; a holder of the
// gate calls it again right before it observes, settles or changes refs.
func (g *gate) checkFence() error {
	if g.isFenced() {
		return errFenced
	}
	return nil
}

func (g *gate) isFenced() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.fenced) > 0
}

// receiveFenced reports whether a not-yet-gone group belongs to the receive.
func (g *gate) receiveFenced(receiveID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range g.fenced {
		if id == receiveID {
			return true
		}
	}
	return false
}

// takeDirty reports and clears the flag; the caller holds the gate exclusively.
func (g *gate) takeDirty() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.dirty
	g.dirty = false
	return d
}

func newGate() *gate { return &gate{changed: make(chan struct{})} }

// notify wakes every waiter; callers hold mu.
func (g *gate) notify() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// errRestoring is the refusal of a mutation while the repository is fenced for
// restore.
var errRestoring = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "the repository is being restored; retry later"}

// errRestoreRequired is the refusal of a bundle replay while the repository is
// not fenced for restore.
var errRestoreRequired = &gitv1.ContractError{Code: gitv1.CodeRestoreFenceRequired, Message: "a bundle is replayed only while the repository is fenced for restore"}

// sharedKind says what a shared holder does to the repository.
type sharedKind int

const (
	// sharedMutate: a receive or a server-side ref operation. Refused while the
	// repository is fenced for restore.
	sharedMutate sharedKind = iota
	// sharedConfig: initialization and reconfiguration. They never change a ref,
	// so they are admitted during a restore (the restore initializes the
	// repository before it replays a bundle, and repeats that step).
	sharedConfig
	// sharedObserve: a reader that must exclude maintenance (bundle creation).
	// Admitted during a restore; counted separately so it can detect a change.
	sharedObserve
)

// acquireShared admits a receive or a server-side ref operation.
func (g *gate) acquireShared(ctx context.Context) (func(), error) {
	return g.acquireSharedKind(ctx, sharedMutate)
}

func (g *gate) acquireSharedKind(ctx context.Context, kind sharedKind) (func(), error) {
	for {
		g.mu.Lock()
		if len(g.fenced) > 0 {
			g.mu.Unlock()
			return nil, errFenced
		}
		if g.restoring && kind == sharedMutate {
			g.mu.Unlock()
			return nil, errRestoring
		}
		if !g.exclusive && g.waiting == 0 {
			g.shared++
			if kind == sharedObserve {
				g.observers++
				g.mu.Unlock()
				return g.releaser(func() { g.shared--; g.observers-- }), nil
			}
			g.epoch++
			g.mu.Unlock()
			return g.releaser(func() { g.shared-- }), nil
		}
		wait := g.changed
		g.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (g *gate) epochNow() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.epoch
}

// exclusiveKind says why an exclusive holder is admitted.
type exclusiveKind int

const (
	// exclusiveMaintenance: refused while fenced or fenced for restore.
	exclusiveMaintenance exclusiveKind = iota
	// exclusiveRepair removes the locks a terminated Git left; it checks the
	// fence itself and runs during a restore too (it changes no ref).
	exclusiveRepair
	// exclusiveReplay: the restore replay and the entry to or exit from the
	// restore fence. Refused while fenced. The replay additionally requires the
	// restore fence (requireRestoring).
	exclusiveReplay
	exclusiveFence
)

// acquireExclusive admits maintenance once no shared holder remains; it refuses
// while the repository is fenced.
func (g *gate) acquireExclusive(ctx context.Context) (func(), error) {
	return g.acquireExclusiveKind(ctx, exclusiveMaintenance)
}

// acquireExclusiveMode is acquireExclusive; with checkFence false (the repair)
// the caller checks the fence itself once it holds the gate.
func (g *gate) acquireExclusiveMode(ctx context.Context, checkFence bool) (func(), error) {
	if !checkFence {
		return g.acquireExclusiveKind(ctx, exclusiveRepair)
	}
	return g.acquireExclusiveKind(ctx, exclusiveMaintenance)
}

func (g *gate) acquireExclusiveKind(ctx context.Context, kind exclusiveKind) (func(), error) {
	g.mu.Lock()
	g.waiting++
	for {
		// The fence is rechecked on every iteration: a request queued before a
		// group was killed (or before the restore fence was entered) must not run
		// once the repository is fenced.
		var refuse error
		switch {
		case kind != exclusiveRepair && len(g.fenced) > 0:
			refuse = errFenced
		case kind == exclusiveMaintenance && g.restoring:
			refuse = errRestoring
		}
		if refuse != nil {
			g.waiting--
			g.notify()
			g.mu.Unlock()
			return nil, refuse
		}
		if !g.exclusive && g.shared == 0 {
			g.waiting--
			g.exclusive = true
			g.epoch++
			g.mu.Unlock()
			return g.releaser(func() { g.exclusive = false }), nil
		}
		wait := g.changed
		g.mu.Unlock()
		select {
		case <-wait:
			g.mu.Lock()
		case <-ctx.Done():
			g.mu.Lock()
			g.waiting--
			g.notify()
			g.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

// acquireReplay admits a bundle replay: exclusively, and only while the
// repository is fenced for restore.
func (g *gate) acquireReplay(ctx context.Context) (func(), error) {
	release, err := g.acquireExclusiveKind(ctx, exclusiveReplay)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	restoring := g.restoring
	g.mu.Unlock()
	if !restoring {
		release()
		return nil, errRestoreRequired
	}
	return release, nil
}

// setRestoring enters or leaves the restore fence and reports whether it
// changed. Entering waits for the running receives and operations (no new one
// is admitted meanwhile); leaving waits for a running replay. persist records
// the new state durably and runs, under the exclusive gate, before the flag
// changes: a failure leaves the fence as it was.
func (g *gate) setRestoring(ctx context.Context, on bool, persist func(on bool) error, marked func() bool) (changed bool, err error) {
	g.mu.Lock()
	same := g.restoring == on
	g.mu.Unlock()
	if same && (on || marked == nil || !marked()) {
		return false, nil // already in the state, and no stale marker to remove
	}
	// Leaving takes the exclusive gate only when the flag is set or a marker is
	// left (by a failed entry): a no-op leave must not stall new receives.
	release, err := g.acquireExclusiveKind(ctx, exclusiveFence)
	if err != nil {
		return false, err
	}
	defer release()
	g.mu.Lock()
	same = g.restoring == on
	g.mu.Unlock()
	if persist != nil && !(same && on) {
		if err := persist(on); err != nil {
			return false, err
		}
	}
	if same {
		return false, nil
	}
	g.mu.Lock()
	g.restoring = on
	g.notify()
	g.mu.Unlock()
	return true, nil
}

func (g *gate) isRestoring() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.restoring
}

func (g *gate) releaser(undo func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			undo()
			g.notify()
			g.mu.Unlock()
		})
	}
}

// snapshot runs fn while no receive, operation or maintenance can start and
// tells it whether none was in progress.
func (g *gate) snapshot(fn func(quiescent bool) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn(!g.exclusive && g.shared == g.observers && len(g.fenced) == 0)
}

// observe is snapshot for an observer that holds the gate (shared, observing):
// fn also receives the admission epoch to compare with epochNow once the
// observer's work is done. No holder can be admitted while fn runs.
func (g *gate) observe(fn func(quiescent bool, epoch uint64) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn(!g.exclusive && g.shared == g.observers && len(g.fenced) == 0, g.epoch)
}

// gates holds one gate per repository ID.
type gates struct {
	mu sync.Mutex
	m  map[string]*gate
}

func (g *gates) of(repositoryID string) *gate {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*gate{}
	}
	if x, ok := g.m[repositoryID]; ok {
		return x
	}
	x := newGate()
	g.m[repositoryID] = x
	return x
}
