package factory

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// TestFactoryStartupGetsFreshPhaseBudget proves infrastructure provisioning and
// application construction receive independent bounded contexts. Infrastructure
// may legitimately consume its whole budget (a kind cluster can take longer
// than three minutes); the application phase must then start with a live
// deadline, not an already-expired parent.
// Cancelling the test context still stops the later phases, so the bounded
// contexts are derived from the test and not from an uncancellable background.
func TestFactoryStartupGetsFreshPhaseBudget(t *testing.T) {
	t.Run("expired infrastructure budget does not expire application startup", func(t *testing.T) {
		parent := t.Context()
		phases := newPhaseContextsWithBudgets(parent, PhaseBudgets{
			Setup:          50 * time.Millisecond,
			Infrastructure: 50 * time.Millisecond,
			Application:    time.Minute,
		})

		infrastructure, cancelInfrastructure := phases.context(phaseInfrastructure)
		defer cancelInfrastructure()
		select {
		case <-infrastructure.Done():
			if !errors.Is(infrastructure.Err(), context.DeadlineExceeded) {
				t.Fatalf("infrastructure context ended with %v; want its own deadline", infrastructure.Err())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("infrastructure phase never reached its own deadline; it is not bounded")
		}

		application, cancelApplication := phases.context(phaseApplication)
		defer cancelApplication()
		if err := application.Err(); err != nil {
			t.Fatalf("application startup inherited an expired context: %v", err)
		}
		// The application phase must have enough of its own budget to do work
		// after infrastructure completed: a shared, already-expired context
		// would make this work fail immediately.
		work, cancelWork := context.WithTimeout(application, 20*time.Millisecond)
		defer cancelWork()
		select {
		case <-work.Done():
			if !errors.Is(work.Err(), context.DeadlineExceeded) {
				t.Fatalf("application work ended with %v; want application's own deadline", work.Err())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("application work never ran against the application phase context")
		}
	})

	t.Run("parent cancellation stops later phases", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		phases := newPhaseContextsWithBudgets(parent, PhaseBudgets{
			Setup:          time.Minute,
			Infrastructure: time.Minute,
			Application:    time.Minute,
		})
		cancelParent()
		application, cancelApplication := phases.context(phaseApplication)
		defer cancelApplication()
		select {
		case <-application.Done():
			if !errors.Is(application.Err(), context.Canceled) {
				t.Fatalf("application context ended with %v; want parent cancellation", application.Err())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("a cancelled parent did not cancel the later phase; the phase context is not derived from the test context")
		}
	})

	t.Run("default infrastructure budget exceeds the old shared bound", func(t *testing.T) {
		budgets := DefaultPhaseBudgets()
		if budgets.Setup <= 0 || budgets.Application <= 0 {
			t.Fatalf("setup/application budgets = %s/%s; want positive", budgets.Setup, budgets.Application)
		}
		// A kind operation can take longer than three minutes. Infrastructure
		// must have a separately sufficient bound.
		if budgets.Infrastructure <= 3*time.Minute {
			t.Fatalf("infrastructure budget = %s; want more than three minutes", budgets.Infrastructure)
		}
		if budgets.Infrastructure < kubernetesReadyTimeout {
			t.Fatalf("infrastructure budget = %s; want at least the %s kind readiness bound", budgets.Infrastructure, kubernetesReadyTimeout)
		}
	})
}

// TestStartupPhaseSequencing drives the production startup sequencing through
// startupPhaseRunner with short budgets and stub phase work. It proves the
// ordering invariants start() relies on: an infrastructure phase may burn its
// whole budget without expiring application or seed startup, every phase
// context remains a child of the test context, and a cancelled test stops later
// phases instead of running them on an already-ended context.
func TestStartupPhaseSequencing(t *testing.T) {
	t.Run("exhausted infrastructure leaves application and seed live", func(t *testing.T) {
		parent := t.Context()
		const (
			setupBudget          = 20 * time.Millisecond
			infrastructureBudget = 40 * time.Millisecond
			applicationBudget    = 2 * time.Second
		)
		runner := newStartupPhaseRunnerWithBudgets(parent, PhaseBudgets{
			Setup:          setupBudget,
			Infrastructure: infrastructureBudget,
			Application:    applicationBudget,
		})

		var ran []startupPhase
		var seedCtx context.Context
		err := runner.run(phaseInfrastructure, func(ctx context.Context) error {
			ran = append(ran, phaseInfrastructure)
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("infrastructure phase context has no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > infrastructureBudget {
				t.Fatalf("infrastructure deadline is %s away; want at most %s", remaining, infrastructureBudget)
			}
			// Consume the whole infrastructure budget, as a slow kind cluster
			// legitimately may.
			<-ctx.Done()
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("infrastructure context ended with %v; want its own deadline", ctx.Err())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("infrastructure phase: %v", err)
		}

		err = runner.run(phaseApplication, func(ctx context.Context) error {
			ran = append(ran, phaseApplication)
			if err := ctx.Err(); err != nil {
				t.Fatalf("application phase inherited an expired context: %v", err)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("application phase context has no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > applicationBudget {
				t.Fatalf("application deadline is %s away; want a fresh budget of at most %s", remaining, applicationBudget)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("application phase: %v", err)
		}

		// Consume the setup context start() obtained early, then prove the
		// later seed gets a fresh setup-phase context instead of that spent one.
		consumedSetup, cancelConsumed := runner.context(phaseSetup)
		defer cancelConsumed()
		<-consumedSetup.Done()
		if !errors.Is(consumedSetup.Err(), context.DeadlineExceeded) {
			t.Fatalf("consumed setup context ended with %v; want its own deadline", consumedSetup.Err())
		}
		err = runner.run(phaseSetup, func(ctx context.Context) error {
			ran = append(ran, phaseSetup)
			seedCtx = ctx
			if ctx == consumedSetup {
				t.Fatalf("seed phase reused the already-spent setup context")
			}
			if err := ctx.Err(); err != nil {
				t.Fatalf("seed phase inherited an expired context: %v", err)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("seed phase context has no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > setupBudget {
				t.Fatalf("seed deadline is %s away; want a fresh budget of at most %s", remaining, setupBudget)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("seed phase: %v", err)
		}
		if seedCtx == nil {
			t.Fatalf("seed phase callback never ran")
		}
		want := []startupPhase{phaseInfrastructure, phaseApplication, phaseSetup}
		if fmt.Sprint(ran) != fmt.Sprint(want) {
			t.Fatalf("startup phases ran as %v; want %v", ran, want)
		}
	})

	t.Run("phase contexts are children of the test context", func(t *testing.T) {
		for _, phase := range []startupPhase{phaseSetup, phaseInfrastructure, phaseApplication} {
			t.Run(fmt.Sprintf("phase-%d", phase), func(t *testing.T) {
				parent, cancelParent := context.WithCancel(context.Background())
				defer cancelParent()
				runner := newStartupPhaseRunnerWithBudgets(parent, PhaseBudgets{
					Setup:          time.Minute,
					Infrastructure: time.Minute,
					Application:    time.Minute,
				})
				ctx, cancel := runner.context(phase)
				defer cancel()
				cancelParent()
				select {
				case <-ctx.Done():
					if !errors.Is(ctx.Err(), context.Canceled) {
						t.Fatalf("phase context ended with %v; want parent cancellation", ctx.Err())
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("cancelling the test context did not cancel phase %d; it is not derived from the test context", phase)
				}
			})
		}
	})

	t.Run("cancelled test context stops later phases", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		runner := newStartupPhaseRunnerWithBudgets(parent, PhaseBudgets{
			Setup:          time.Minute,
			Infrastructure: time.Minute,
			Application:    time.Minute,
		})
		cancelParent()
		ran := false
		err := runner.run(phaseApplication, func(context.Context) error {
			ran = true
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("application phase on a cancelled test context returned %v; want context.Canceled", err)
		}
		if ran {
			t.Fatalf("a later phase ran after the test context was cancelled")
		}
	})

	// A phase callback's error must reach start(): the application phase
	// returns app.New's error and the setup phase returns the OIDC seed's
	// error. A runner that ran the callback but discarded its error would turn
	// a failed composition or seed into an apparently successful start, so the
	// sentinel must survive for every phase.
	t.Run("phase callback error is returned for every phase", func(t *testing.T) {
		parent := t.Context()
		runner := newStartupPhaseRunnerWithBudgets(parent, PhaseBudgets{
			Setup:          time.Minute,
			Infrastructure: time.Minute,
			Application:    time.Minute,
		})
		sentinel := errors.New("phase work failed")
		for _, phase := range []startupPhase{phaseApplication, phaseSetup, phaseInfrastructure} {
			t.Run(fmt.Sprintf("phase-%d", phase), func(t *testing.T) {
				ran := false
				err := runner.run(phase, func(context.Context) error {
					ran = true
					return sentinel
				})
				if !ran {
					t.Fatalf("phase %d callback never ran", phase)
				}
				if !errors.Is(err, sentinel) {
					t.Fatalf("run(phase %d) returned %v; want the callback's %v", phase, err, sentinel)
				}
			})
		}
	})
}

// TestPhaseBudgetsFallBackToDefaults proves a zero or negative configured budget
// falls back to the documented default instead of producing an immediately
// expired context. An accepted zero budget would hand real startup work a
// context that is already done.
func TestPhaseBudgetsFallBackToDefaults(t *testing.T) {
	defaults := DefaultPhaseBudgets()
	want := map[startupPhase]time.Duration{
		phaseSetup:          defaults.Setup,
		phaseInfrastructure: defaults.Infrastructure,
		phaseApplication:    defaults.Application,
	}
	invalid := []PhaseBudgets{
		{},
		{Setup: -time.Second, Infrastructure: -time.Second, Application: -time.Second},
	}
	for phase, expected := range want {
		for _, budgets := range invalid {
			if got := budgets.budget(phase); got != expected {
				t.Fatalf("budget(phase %d) for %+v = %s; want default %s", phase, budgets, got, expected)
			}
			runner := newStartupPhaseRunnerWithBudgets(context.Background(), budgets)
			ctx, cancel := runner.context(phase)
			if err := ctx.Err(); err != nil {
				cancel()
				t.Fatalf("a zero/negative configured budget produced an already-ended phase %d context: %v", phase, err)
			}
			deadline, ok := ctx.Deadline()
			cancel()
			if !ok || time.Until(deadline) < expected/2 {
				t.Fatalf("phase %d deadline is %s away; want close to the default %s", phase, time.Until(deadline), expected)
			}
		}
	}
}

// TestFactoryStartupPhaseWiring pins the context identifier each real startup
// call site receives. The sequencing seam alone cannot stop a call site from
// discarding its phase context parameter, so this test parses server.go and
// kubernetes.go and asserts the four phase-sensitive calls use exactly the
// callback parameter (or the newKubernetesRuntime ctx parameter) they are
// handed. Replacing any of them with setupCtx or context.Background() fails
// here.
func TestFactoryStartupPhaseWiring(t *testing.T) {
	server := parseFactoryFile(t, "server.go")
	startFunction := factoryFunction(t, server, "start")
	callbacks := factoryPhaseCallbacks(t, startFunction.Body)
	for _, phase := range []string{"phaseInfrastructure", "phaseApplication", "phaseSetup"} {
		if _, ok := callbacks[phase]; !ok {
			t.Fatalf("start() does not run %s through the phase runner", phase)
		}
	}

	// The runner's parent must be the test context, never a background context.
	runners := factoryCalls(startFunction.Body, func(call *ast.CallExpr) bool {
		return factoryIdentCall(call, "newStartupPhaseRunner")
	})
	if len(runners) != 1 {
		t.Fatalf("start() constructs %d phase runners; want exactly 1", len(runners))
	}
	if got := factoryExpressionName(runners[0].Args[0]); got != "t.Context()" {
		t.Fatalf("phase runner parent = %s; want the test context t.Context()", got)
	}

	// app.New must receive the application callback's parameter.
	apps := factoryCalls(startFunction.Body, func(call *ast.CallExpr) bool {
		return factorySelectorCall(call, "app", "New")
	})
	if len(apps) != 1 {
		t.Fatalf("start() calls app.New %d times; want exactly 1", len(apps))
	}
	if got, want := factoryExpressionName(apps[0].Args[0]), callbacks["phaseApplication"]; got != want {
		t.Fatalf("app.New context = %s; want the application phase parameter %s", got, want)
	}

	// newKubernetesRuntime must receive the infrastructure callback's parameter.
	kubers := factoryCalls(startFunction.Body, func(call *ast.CallExpr) bool {
		return factoryIdentCall(call, "newKubernetesRuntime")
	})
	if len(kubers) != 1 {
		t.Fatalf("start() calls newKubernetesRuntime %d times; want exactly 1", len(kubers))
	}
	if got, want := factoryExpressionName(kubers[0].Args[1]), callbacks["phaseInfrastructure"]; got != want {
		t.Fatalf("newKubernetesRuntime context = %s; want the infrastructure phase parameter %s", got, want)
	}

	// The seed must receive the fresh seed callback parameter, not the setup
	// context start() obtained early.
	seeds := factoryCalls(startFunction.Body, func(call *ast.CallExpr) bool {
		return factorySelectorCall(call, "", "SeedTenantWideMembership")
	})
	if len(seeds) != 1 {
		t.Fatalf("start() calls SeedTenantWideMembership %d times; want exactly 1", len(seeds))
	}
	if got, want := factoryExpressionName(seeds[0].Args[0]), callbacks["phaseSetup"]; got != want {
		t.Fatalf("seed context = %s; want the seed phase parameter %s", got, want)
	}

	// CreateCluster must receive newKubernetesRuntime's own ctx parameter.
	kubernetes := parseFactoryFile(t, "kubernetes.go")
	runtimeFunction := factoryFunction(t, kubernetes, "newKubernetesRuntime")
	if runtimeFunction.Type.Params == nil || len(runtimeFunction.Type.Params.List) != 6 {
		t.Fatalf("newKubernetesRuntime has %d parameter groups; want 6", len(runtimeFunction.Type.Params.List))
	}
	ctxParam := runtimeFunction.Type.Params.List[1].Names[0].Name
	clusters := factoryCalls(runtimeFunction.Body, func(call *ast.CallExpr) bool {
		return factorySelectorCall(call, "kind", "CreateCluster")
	})
	if len(clusters) != 1 {
		t.Fatalf("newKubernetesRuntime calls CreateCluster %d times; want exactly 1", len(clusters))
	}
	if got := factoryExpressionName(clusters[0].Args[0]); got != ctxParam {
		t.Fatalf("CreateCluster context = %s; want newKubernetesRuntime's %s parameter", got, ctxParam)
	}
}

func parseFactoryFile(t *testing.T, name string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

func factoryFunction(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	t.Fatalf("%s: function %s not found", file.Name.Name, name)
	return nil
}

// factoryPhaseCallbacks maps each phase runner callback to the name of its
// context parameter.
func factoryPhaseCallbacks(t *testing.T, body *ast.BlockStmt) map[string]string {
	t.Helper()
	callbacks := map[string]string{}
	for _, call := range factoryCalls(body, func(call *ast.CallExpr) bool {
		selector, ok := call.Fun.(*ast.SelectorExpr)
		return ok && selector.Sel.Name == "run" && len(call.Args) == 2
	}) {
		phase, ok := call.Args[0].(*ast.Ident)
		if !ok {
			continue
		}
		literal, ok := call.Args[1].(*ast.FuncLit)
		if !ok || literal.Type.Params == nil || len(literal.Type.Params.List) != 1 ||
			len(literal.Type.Params.List[0].Names) != 1 {
			t.Fatalf("phase %s callback must take exactly one named context parameter", phase.Name)
		}
		callbacks[phase.Name] = literal.Type.Params.List[0].Names[0].Name
	}
	return callbacks
}

func factoryCalls(node ast.Node, match func(*ast.CallExpr) bool) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(node, func(current ast.Node) bool {
		if call, ok := current.(*ast.CallExpr); ok && match(call) {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func factoryIdentCall(call *ast.CallExpr, name string) bool {
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && identifier.Name == name
}

func factorySelectorCall(call *ast.CallExpr, qualifier, name string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	if qualifier == "" {
		return true
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == qualifier
}

func factoryExpressionName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return factoryExpressionName(value.X) + "." + value.Sel.Name
	case *ast.CallExpr:
		return factoryExpressionName(value.Fun) + "()"
	}
	return fmt.Sprintf("%T", expression)
}
