package factory

// Independent startup phase budgets.
//
// A factory start does three separable kinds of work: local setup and migration,
// infrastructure provisioning/readiness (a rootless kind cluster can legitimately
// take longer than three minutes), and building the composition. Sharing one
// context starts the infrastructure clock before the slow operation and hands
// an already-expired context to app construction. Each phase gets its own
// bounded, cancelable context derived from the test context, so infrastructure
// may consume its whole budget without starving the application phase, while
// cancelling the test still stops every later phase.

import (
	"context"
	"time"
)

// startupPhase names one bounded startup stage.
type startupPhase int

const (
	phaseSetup startupPhase = iota
	phaseInfrastructure
	phaseApplication
)

// PhaseBudgets bounds each startup phase independently.
type PhaseBudgets struct {
	Setup          time.Duration
	Infrastructure time.Duration
	Application    time.Duration
}

// DefaultPhaseBudgets returns the production startup bounds. The infrastructure
// budget covers the kind cluster's own readiness barrier; it is deliberately
// larger than the other two.
func DefaultPhaseBudgets() PhaseBudgets {
	return PhaseBudgets{
		Setup:          3 * time.Minute,
		Infrastructure: 15 * time.Minute,
		Application:    3 * time.Minute,
	}
}

// budget returns the bound for one phase, falling back to the default when a
// caller constructed an incomplete budget.
func (budgets PhaseBudgets) budget(phase startupPhase) time.Duration {
	var configured time.Duration
	switch phase {
	case phaseSetup:
		configured = budgets.Setup
	case phaseInfrastructure:
		configured = budgets.Infrastructure
	case phaseApplication:
		configured = budgets.Application
	}
	if configured <= 0 {
		switch phase {
		case phaseSetup:
			return DefaultPhaseBudgets().Setup
		case phaseInfrastructure:
			return DefaultPhaseBudgets().Infrastructure
		case phaseApplication:
			return DefaultPhaseBudgets().Application
		}
	}
	return configured
}

// phaseContexts derives one fresh bounded work context per phase from the test
// context. It keeps no context between calls: the application phase created
// after an exhausted infrastructure phase must still have a live deadline.
type phaseContexts struct {
	parent  context.Context
	budgets PhaseBudgets
}

// newPhaseContexts returns the production phase-context factory bound to the
// test context.
func newPhaseContexts(parent context.Context) *phaseContexts {
	return &phaseContexts{parent: parent, budgets: DefaultPhaseBudgets()}
}

// newPhaseContextsWithBudgets is the deterministic test seam: the caller
// chooses short phase bounds without changing the production defaults.
func newPhaseContextsWithBudgets(parent context.Context, budgets PhaseBudgets) *phaseContexts {
	return &phaseContexts{parent: parent, budgets: budgets}
}

// context derives a fresh bounded context for one phase. Every call starts a
// new deadline measured from now and remains a child of the test context, so
// parent cancellation still stops later phases.
func (phases *phaseContexts) context(phase startupPhase) (context.Context, context.CancelFunc) {
	return context.WithTimeout(phases.parent, phases.budgets.budget(phase))
}

// startupPhaseRunner sequences the bounded startup phases. Every phase derives
// its context through the same factory, so the sequencing (fresh context per
// phase, derived from the test context) is one seam a test can drive with short
// budgets and stub phase work. start() uses it for the infrastructure,
// application and seed phases; the setup phase spans several independent calls
// and obtains its context through context directly.
type startupPhaseRunner struct {
	phases *phaseContexts
}

// newStartupPhaseRunner returns the production runner bound to the test
// context and the default budgets.
func newStartupPhaseRunner(parent context.Context) *startupPhaseRunner {
	return &startupPhaseRunner{phases: newPhaseContexts(parent)}
}

// newStartupPhaseRunnerWithBudgets is the deterministic test seam: the caller
// chooses short phase bounds without changing the production defaults.
func newStartupPhaseRunnerWithBudgets(parent context.Context, budgets PhaseBudgets) *startupPhaseRunner {
	return &startupPhaseRunner{phases: newPhaseContextsWithBudgets(parent, budgets)}
}

// context derives the bounded context for one phase whose work spans several
// independent calls rather than one callback.
func (runner *startupPhaseRunner) context(phase startupPhase) (context.Context, context.CancelFunc) {
	return runner.phases.context(phase)
}

// run derives a fresh bounded context for phase and runs fn with it. A phase
// whose context has already ended is not run at all: a cancelled test context
// must stop every later phase, and an already-exhausted phase budget must not
// hand an expired context to real work.
func (runner *startupPhaseRunner) run(phase startupPhase, fn func(context.Context) error) error {
	ctx, cancel := runner.phases.context(phase)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}
