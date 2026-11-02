// Package kernelcontracts is the one shared contract suite every
// runtime.StateStore adapter must pass (internal/kernel/runtime). It states the
// port's policy as executable cases: claims, fences and takeover, optimistic
// versions, the outbox committed with state, cancellation, cleanup, wait
// decisions, signal identity routing, scope enforcement and the rebuilt run
// view, plus the durable evidence the engine recovers from: a succeeded step's
// declared outputs, the release of a claim without ending the run, and the
// scoped identity of secret material that a replacement engine revokes. The memory adapter (internal/shell/runstate) and, later, the
// PostgreSQL adapter run the same cases; a backend adds its own tests for what
// the suite cannot show (row security, lock behaviour, restart recovery).
//
// An adapter supplies a Factory that builds a fresh, empty Subject for each
// case around the suite's controllable Clock. Subject is the port plus the two
// hooks the suite needs and the port does not offer: reading committed outbox
// events and failing the next write at commit time.
//
// The suite is itself under test: a deliberately broken adapter must fail it
// (see the package tests), so a case that passes vacuously is a defect.
package kernelcontracts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Subject is the adapter under test.
type Subject interface {
	runtime.StateStore
	// Outbox returns the events committed for one run in commit order. A run in
	// another scope or one that does not exist is a not-found error.
	Outbox(ctx context.Context, scope domain.Scope, id domain.RunID) ([]runtime.Event, error)
	// FailNextCommit makes the next write that reaches its commit point fail
	// with err (reachable through errors.Is) and persist nothing at all. A nil
	// err clears a pending fault. A write that fails before its commit point
	// does not consume it.
	FailNextCommit(err error)
}

// Factory builds a fresh, empty Subject whose own time decisions (a
// cancellation) use clock.
type Factory func(t TB, clock runtime.Clock) Subject

// TB is the part of testing.TB the cases use, so the suite can also run under a
// recorder that reports failures instead of failing a real test.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Group names a family of cases so an acceptance test can run just its own.
type Group string

// Case groups.
const (
	GroupAdmission Group = "admission"
	GroupClaims    Group = "claims"
	GroupCommits   Group = "commits"
	GroupCancel    Group = "cancel"
	GroupCleanup   Group = "cleanup"
	GroupWaits     Group = "waits"
	GroupScope     Group = "scope"
	GroupViews     Group = "views"
	// GroupOutputs: durable step outputs. GroupRelease: giving up a claim without
	// ending the run. GroupMaterialization: the durable identity of secret material.
	GroupOutputs         Group = "outputs"
	GroupRelease         Group = "release"
	GroupMaterialization Group = "materialization"
)

// Case is one named behaviour of the port.
type Case struct {
	Name  string
	Group Group
	Run   func(t TB, e *Env)
}

// Clock is the suite's controllable time. It implements runtime.Clock and is
// safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock at the suite's fixed start time.
func NewClock() *Clock { return &Clock{now: t0} }

// Now returns the current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Set moves the clock to an instant.
func (c *Clock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// Cases returns every case in a stable order.
func Cases() []Case {
	var all []Case
	for _, g := range [][]Case{admissionCases(), claimCases(), commitCases(), cancelCases(), cleanupCases(), waitCases(), scopeCases(), viewCases(),
		outputsCases(), releaseCases(), materializationCases(), taskCases()} {
		all = append(all, g...)
	}
	return all
}

// Check runs one case against a fresh Subject built by f.
func Check(t TB, f Factory, c Case) {
	t.Helper()
	clock := NewClock()
	e := &Env{T: t, S: f(t, clock), Clock: clock, Ctx: context.Background()}
	c.Run(t, e)
}

// Run runs every case as a subtest.
func Run(t *testing.T, f Factory) {
	t.Helper()
	RunGroup(t, f)
}

// RunGroup runs the cases of the given groups, or every case when none is given.
func RunGroup(t *testing.T, f Factory, groups ...Group) {
	t.Helper()
	for _, c := range Cases() {
		if len(groups) > 0 && !hasGroup(groups, c.Group) {
			continue
		}
		t.Run(c.Name, func(t *testing.T) { Check(t, f, c) })
	}
}

func hasGroup(groups []Group, g Group) bool {
	for _, x := range groups {
		if x == g {
			return true
		}
	}
	return false
}
