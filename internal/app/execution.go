package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	artifactshttp "github.com/optimiware/kodama/internal/artifacts/http"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/logs"
	logshttp "github.com/optimiware/kodama/internal/logs/http"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/results"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	resultshttp "github.com/optimiware/kodama/internal/results/http"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/execbridge"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
)

// ExecutionInputs are the collaborators of the container execution slice:
// everything between the server, the executor, the entrypoint's runtime API and
// the result and log interfaces. AssembleExecution is pure composition over
// them, so the worker lifecycle, the constructor selection and the guard
// bindings are tested with injected adapters and no service; NewCore supplies
// the PostgreSQL, S3 and Kubernetes-backed ones.
type ExecutionInputs struct {
	Env    Env
	Log    *slog.Logger
	Tuning Tuning

	// Deps are the execution use cases' collaborators. Store, Runtime and
	// Planner are bound by the assembly and must be left unset: a caller cannot
	// bypass the selection or the guard by pre-binding them.
	Deps    execution.Dependencies
	Options execution.Options

	// Plain is the trusted execution store: dispatch, cancellation,
	// observation and reconciliation. Guarded is the same module's store for the
	// runtime result path, which repeats the session check under the run lock
	// (T-062). They must be different stores.
	Plain   execution.Store
	Guarded execution.Store

	// Runtime is the selected workload runtime and its backend seams.
	Runtime RuntimeSet

	// Authority authenticates and binds the bearer of /result and /source;
	// Evidence reads artifact evidence; Source proposes workspace publication.
	Authority results.Authority
	Evidence  results.Evidence
	Source    results.Source

	// Auth configures runtime admission. Jobs must be left unset: the assembly
	// binds the selected runtime's observer, so admission verifies a claimed
	// Job and Pod through the same backend that started them. Late, when set,
	// is bound to the resulting service so the runtime can register admission
	// credentials with it.
	Auth runtimeauth.Config
	Late *LateRegistrar
	// Artifacts serves the runtime artifact routes.
	Artifacts artifactshttp.RuntimeService

	// Logs is the log read API. Capture, when set, is invoked before a settled
	// workload is removed so its output is retained first.
	Logs    logshttp.Opener
	Capture LogCapture

	// Environments resolves a step's declared environment at admission (T-072,
	// delivery). It is accepted only when Plain binds environments in its
	// admission transaction (execution.EnvironmentStore): a resolver over a store
	// that cannot revalidate the pin would retain configuration for a run that is
	// then refused.
	Environments execution.EnvironmentResolver
	// AgentPlanner, when set, replaces the selected runtime's planner with its
	// agent-aware decoration (agents Chain.Planner built over Runtime.Planner).
	// Leave it unset unless the agent step kind is enabled in Deps.
	AgentPlanner execution.Planner
	// Deployments serves the runtime deployment callbacks (deployment.call) under
	// /runtime/v1/attempts/{attemptId}/deployments; nil leaves them unserved and
	// recorded as disabled.
	Deployments deliveryhttp.RuntimeService
	// Secrets serves resolveSecret (the runtime secret route, T-097/T-101); nil
	// leaves the operation unserved and `runtime-secret-resolution` recorded
	// as disabled.
	Secrets SecretResolver

	// Shared is the run_shared half of the slice (T-116): the persistence and
	// token seams of the resource subjects. It is supplied exactly when the
	// selected runtime contributes a SharedRuntime; one without the other is a
	// construction error, and Deps.Shared must be left unset (the assembly binds
	// the resource port of the SAME backend as the runtime).
	Shared *SharedInputs
}

// LogCapture retains an attempt's log before its workload goes away: it
// observes the wrapper's verified upload and, only where a live reader exists,
// collects the controller fallback. *logs.Controller satisfies it.
type LogCapture interface {
	Observe(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (*logs.Record, error)
	Collect(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (*logs.Record, error)
}

// ExecutionSlice is the assembled container execution slice.
type ExecutionSlice struct {
	// Trusted runs dispatch, cancellation, observation and reconciliation over
	// the plain store and the selected runtime.
	Trusted *execution.Service
	// Guarded commits runtime results over the guarded store. Only the result
	// path uses it.
	Guarded *execution.Service
	Results *results.Service
	// Runtime serves the runtime API under /runtime/v1.
	Runtime http.Handler
	// RuntimeV2 serves the run_shared runtime API under /runtime/v2; nil unless
	// run_shared is composed.
	RuntimeV2 http.Handler
	// Policies is the installation's policy source when run_shared is composed.
	Policies execution.Policies
	// Auth is the runtime-authority service: the gateway and MCP proxy of the
	// model/tool slice (T-039) verify the attempt's token through it.
	Auth *runtimeauth.Service
	// RegisterPublic adds the slice's public routes (logs) to the API router.
	RegisterPublic func(*httpapi.Router) error
	// Workers are the slice's background loops, in start order.
	Workers []NamedWorker
	// ReconcileTenant is the execution repair pass the event reconciler runs.
	ReconcileTenant func(ctx context.Context, tenant domain.TenantID) error
	// Backend is the pinned runtime identity.
	Backend string
	// Routes and Disabled record what the slice serves and what it deliberately
	// does not.
	Routes   []string
	Disabled []string
}

// AssembleExecution validates the inputs and builds the slice. Every refusal
// is a construction error: nothing is started and a missing or unguarded
// dependency never degrades to an allow-all default.
func AssembleExecution(in ExecutionInputs) (*ExecutionSlice, error) {
	missing := func(what string) error { return fmt.Errorf("%w: %s is required", execution.ErrNotConfigured, what) }
	if !in.Env.Valid() {
		return nil, ErrEnvUnknown
	}
	if in.Log == nil {
		in.Log = slog.New(slog.DiscardHandler)
	}
	switch {
	case in.Plain == nil:
		return nil, missing("the trusted execution store")
	case in.Guarded == nil:
		return nil, missing("the guarded execution store of the result path")
	case sameStore(in.Plain, in.Guarded):
		return nil, fmt.Errorf("%w: the result path must use the guarded store, not the trusted one", execution.ErrNotConfigured)
	case in.Deps.Store != nil || in.Deps.Runtime != nil || in.Deps.Planner != nil:
		return nil, fmt.Errorf("%w: the assembly binds the store, runtime and planner itself", execution.ErrNotConfigured)
	case in.Authority == nil, in.Evidence == nil, in.Source == nil:
		return nil, missing("the result authority, artifact evidence and source coordinator")
	case in.Auth.Jobs != nil:
		return nil, fmt.Errorf("%w: the assembly binds the Job observer of the selected runtime", execution.ErrNotConfigured)
	case in.Auth.Verifier == nil, in.Artifacts == nil:
		return nil, missing("the runtime token verifier and artifact routes")
	case in.Logs == nil:
		return nil, missing("the log service")
	case in.Deps.Shared != nil:
		return nil, fmt.Errorf("%w: the assembly binds the run_shared resource port of the selected runtime itself", execution.ErrNotConfigured)
	case in.Environments != nil && !admitsEnvironments(in.Plain):
		return nil, fmt.Errorf("%w: an environment resolver needs a store that binds environments in its admission transaction", execution.ErrNotConfigured)
	}
	// Every injected adapter passes the construction guard: production refuses
	// a memory store, a memory peer and any other development or test adapter.
	for _, adapter := range []any{
		in.Plain, in.Guarded, in.Auth.Store, in.Auth.Attempts,
		in.Deps.Authority, in.Deps.Work, in.Deps.Configuration, in.Deps.Sources, in.Deps.CancelledWork, in.Deps.EffectReconciler,
		in.Authority, in.Evidence, in.Source, in.Artifacts, in.Logs, in.Capture,
		in.Deps.Cleanup, in.Deps.CleanupEnvironments, in.Environments, in.AgentPlanner, in.Deployments, in.Secrets,
	} {
		if adapter == nil {
			continue
		}
		if _, err := Admit(in.Env, PortStore, adapter); err != nil {
			return nil, err
		}
	}
	set, err := AdmitRuntime(in.Env, in.Runtime)
	if err != nil {
		return nil, err
	}
	if err := admitShared(in, set); err != nil {
		return nil, err
	}

	// ---- runtime admission over the selected backend's Job observer ----
	authCfg := in.Auth
	authCfg.Jobs = set.Jobs
	if in.Auth.Resources != nil {
		return nil, fmt.Errorf("%w: the assembly binds the resource subjects of the selected runtime", execution.ErrNotConfigured)
	}
	if set.Shared != nil {
		authCfg.Resources = &runtimeauth.ResourceConfig{
			Store: in.Shared.Store, State: in.Shared.State, Workloads: set.Shared.Workloads, Tokens: in.Shared.Tokens, Verifier: in.Shared.Verifier,
		}
	}
	if authCfg.Logger == nil {
		authCfg.Logger = in.Log
	}
	auth, err := runtimeauth.NewService(authCfg)
	if err != nil {
		return nil, fmt.Errorf("runtime admission: %w", err)
	}
	if in.Late != nil {
		in.Late.Bind(auth)
	}

	// ---- execution: one service per store, both over the selected runtime ----
	trustedDeps := in.Deps
	trustedDeps.Store, trustedDeps.Runtime, trustedDeps.Planner = in.Plain, set.Runtime, set.Planner
	if in.AgentPlanner != nil {
		trustedDeps.Planner = in.AgentPlanner
	}
	if set.Shared != nil {
		trustedDeps.Shared = &execution.SharedConfig{Resources: set.Shared.Resources, Policies: set.Shared.Policies}
		if trustedDeps.Definitions != nil {
			trustedDeps.Definitions = retireLegacy{Definitions: trustedDeps.Definitions, policies: set.Shared.Policies}
		}
	}
	trustedDeps.Requirements = requirementsFor(in.Env, in.Deps.Requirements)
	if in.Capture != nil {
		trustedDeps.Runtime = captureRuntime{Runtime: set.Runtime, capture: in.Capture, live: set.Live != nil, log: in.Log}
	}
	newService := func(d execution.Dependencies) (*execution.Service, error) {
		if in.Environments != nil {
			return execution.NewServiceWithEnvironments(d, in.Options, in.Environments)
		}
		return execution.NewService(d, in.Options)
	}
	trusted, err := newService(trustedDeps)
	if err != nil {
		return nil, fmt.Errorf("execution service: %w", err)
	}
	guardedDeps := trustedDeps
	guardedDeps.Store = in.Guarded
	guarded, err := newService(guardedDeps)
	if err != nil {
		return nil, fmt.Errorf("guarded execution service: %w", err)
	}

	// ---- results: the commit goes through the guarded service; the run reads
	// the trusted store (reads take no session) ----
	finalizer, err := resultsadapters.NewExecution(guarded, in.Plain)
	if err != nil {
		return nil, err
	}
	res, err := results.NewService(results.Dependencies{
		Authority: in.Authority, Contexts: finalizer, Evidence: in.Evidence, Finalizer: finalizer, Source: in.Source,
	}, results.Options{Logger: in.Log})
	if err != nil {
		return nil, fmt.Errorf("results service: %w", err)
	}

	apiCfg := runtimeAPIConfig{
		Auth: auth, Results: res, Verifier: authCfg.Verifier, Artifacts: in.Artifacts, Deployments: in.Deployments, Secrets: in.Secrets,
		RateLimit: in.Tuning.RateLimitPerMinute, Log: in.Log,
	}
	var resourceSecrets runtimehttp.ResourceSecretResolver
	if set.Shared != nil {
		if in.Shared.Secrets != nil {
			rs, err := newResourceSecrets(in.Shared.Secrets, auth)
			if err != nil {
				return nil, err
			}
			resourceSecrets = rs
		}
		// The begin and the preparation receipt are recorded through the guarded
		// service: its store carries the runtime-grant fence in the recording
		// transaction (execution.GrantFence), and the bridge refuses without the
		// session recheck.
		apiCfg.V2 = &runtimeV2Config{
			Service: auth, Begins: execbridge.Begins{Exec: guarded, Recheck: auth}, Receipts: execbridge.Receipts{Exec: guarded, Recheck: auth},
			ResourceSecrets: resourceSecrets,
		}
	}
	runtimeAPI, runtimeV2, err := newRuntimeAPI(apiCfg)
	if err != nil {
		return nil, err
	}

	var register func(*httpapi.Router) error
	logHandler, err := logshttp.NewHandler(in.Logs, in.Tuning.Logs.WriteTimeout)
	if err != nil {
		return nil, err
	}
	register = logHandler.RegisterRoutes

	// ---- workers ----
	s := &ExecutionSlice{
		Trusted: trusted, Guarded: guarded, Results: res, Runtime: runtimeAPI, RuntimeV2: runtimeV2, Auth: auth, RegisterPublic: register, Backend: set.Runtime.Capabilities().Backend,
		ReconcileTenant: func(ctx context.Context, tenant domain.TenantID) error {
			_, err := trusted.ReconcileTenant(ctx, tenant)
			return err
		},
		Routes: []string{"runtime-admission", "runtime-context", "step-result-callbacks", "source-proposals", "runtime-artifacts", "runtime-artifact-reads", "logs"},
		Disabled: []string{
			"runtime-secret-resolution", "gates", "agents", "delivery", "cleanup-runs", "git-webhooks", "chatops",
		},
	}
	if in.Deployments != nil {
		s.Routes = append(s.Routes, "deployment-callbacks")
	}
	if set.Shared != nil {
		s.Policies = set.Shared.Policies
		s.Routes = append(s.Routes, "runtime-v2")
		if resourceSecrets == nil {
			s.Disabled = append(s.Disabled, "service-secret-resolution")
		}
	}
	if in.Secrets != nil {
		s.Routes = append(s.Routes, capSecretResolution)
		s.Disabled = slices.DeleteFunc(s.Disabled, func(n string) bool { return n == capSecretResolution })
	}
	tenants := in.Deps.Tenants
	if tenants == nil {
		return nil, missing("the tenant source of the dispatch loop")
	}
	s.Workers = append(s.Workers, NamedWorker{Name: "dispatch", Run: func(ctx context.Context) error {
		return loop(ctx, in.Tuning.DispatchInterval, func(ctx context.Context) error { return dispatchPass(ctx, trusted, tenants) }, in.Log, "dispatch")
	}})
	if set.Workers != nil {
		extra, err := set.Workers(RuntimeHooks{Observations: trusted, Tenants: tenants})
		if err != nil {
			return nil, fmt.Errorf("runtime workers: %w", err)
		}
		s.Workers = append(s.Workers, extra...)
	}
	return s, nil
}

// LateRegistrar breaks the construction cycle between the runtime, which
// registers admission credentials, and runtime admission, whose Job observer is
// the runtime. It refuses every registration until bound, so a runtime started
// before admission exists fails closed.
type LateRegistrar struct {
	mu sync.RWMutex
	r  bootstrap.Registrar
}

var _ bootstrap.Registrar = (*LateRegistrar)(nil)

// Bind sets the registrar once the runtime-authority service exists.
func (l *LateRegistrar) Bind(r bootstrap.Registrar) {
	l.mu.Lock()
	l.r = r
	l.mu.Unlock()
}

// Register implements bootstrap.Registrar.
func (l *LateRegistrar) Register(ctx context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	l.mu.RLock()
	r := l.r
	l.mu.RUnlock()
	if r == nil {
		return runtimeauth.Registration{}, fmt.Errorf("%w: runtime admission is not bound", runtimeauth.ErrUnavailable)
	}
	return r.Register(ctx, cmd)
}

// RegisterResource registers a run resource's admission credential with the
// bound runtime authority, with the same refusal until it is bound (and for a
// registrar that has no resource subjects).
func (l *LateRegistrar) RegisterResource(ctx context.Context, cmd runtimeauth.ResourceRegisterCommand) error {
	l.mu.RLock()
	r := l.r
	l.mu.RUnlock()
	rr, ok := r.(interface {
		RegisterResource(context.Context, runtimeauth.ResourceRegisterCommand) error
	})
	if !ok {
		return fmt.Errorf("%w: runtime admission of run resources is not bound", runtimeauth.ErrUnavailable)
	}
	return rr.RegisterResource(ctx, cmd)
}

// RegisterV2 registers the admission credential of a run_shared attempt on the v2
// wire, with the same refusal until bound (and for a registrar without the v2
// wire). The Kubernetes runtime registers its run_shared attempts through it.
func (l *LateRegistrar) RegisterV2(ctx context.Context, cmd runtimeauth.RegisterCommandV2) (runtimeauth.Registration, error) {
	l.mu.RLock()
	r := l.r
	l.mu.RUnlock()
	r2, ok := r.(bootstrap.RegistrarV2)
	if !ok {
		return runtimeauth.Registration{}, fmt.Errorf("%w: runtime admission of run_shared attempts is not bound", runtimeauth.ErrUnavailable)
	}
	return r2.RegisterV2(ctx, cmd)
}

var _ kube.RegistrarV2 = (*LateRegistrar)(nil)

// requirementsFor is the installation's runtime requirement policy. Production
// and test keep execution's default (a pinned image run in isolation within
// resource limits); only development, whose Process runtime is a host
// fixture, drops it, and production refuses that runtime before this applies.
func requirementsFor(env Env, configured execution.Requirements) execution.Requirements {
	if configured != nil {
		return configured
	}
	if env == Development {
		return func(pipeline.Step) execution.Guarantees { return 0 }
	}
	return nil // execution.DefaultRequirements
}

// admitsEnvironments reports whether the store revalidates declared
// environments inside its admission transaction.
func admitsEnvironments(s execution.Store) bool {
	e, ok := s.(execution.EnvironmentStore)
	return ok && e.AdmitsEnvironments()
}

func sameStore(a, b execution.Store) (same bool) {
	defer func() {
		if recover() != nil {
			same = false // uncomparable dynamic types are never the same pointer
		}
	}()
	return a == b
}

// maxDispatchPerTenant bounds one dispatch pass per tenant so a deep backlog
// of one tenant cannot starve the others or the repair loops.
const maxDispatchPerTenant = 100

// dispatchPass claims and starts runnable attempts of every tenant. The claim
// is a committed transaction and the runtime call happens outside it
// (execution.DispatchNext); a failed start is repaired by the reconciler with
// the same attempt identity, never by a new attempt.
func dispatchPass(ctx context.Context, svc *execution.Service, tenants execution.Tenants) error {
	ids, err := tenants.TenantIDs(ctx)
	if err != nil {
		return fmt.Errorf("tenant enumeration: %w", err)
	}
	var errs []error
	for _, tenant := range ids {
		for i := 0; i < maxDispatchPerTenant && ctx.Err() == nil; i++ {
			more, err := svc.DispatchNext(ctx, tenant)
			if err != nil {
				errs = append(errs, err)
				break
			}
			if !more {
				break
			}
		}
	}
	return errors.Join(errs...)
}

// captureRuntime retains an attempt's log before its settled workload is
// removed. The capture is bounded by the controller's own limits; a failure
// that may still resolve answers ErrBusy so execution repeats the removal on a
// later pass, and one that cannot (the controller recorded explicit
// unavailable evidence) lets the removal proceed, since the evidence is the
// outcome and holding the workload forever would not improve it.
type captureRuntime struct {
	execution.Runtime
	capture LogCapture
	live    bool
	log     *slog.Logger
}

func (c captureRuntime) Remove(ctx context.Context, w execution.Workload) error {
	_, err := c.capture.Observe(ctx, w.Scope, w.AttemptID)
	switch {
	case err == nil:
	case errors.Is(err, logs.ErrNoLog) && c.live:
		rec, cerr := c.capture.Collect(ctx, w.Scope, w.AttemptID)
		if cerr != nil {
			if errors.Is(cerr, logs.ErrNotFound) {
				break
			}
			c.log.Warn("log capture failed before removal", "attempt", string(w.AttemptID), "error", cerr)
			return fmt.Errorf("%w: the log capture is not settled", execution.ErrBusy)
		}
		if rec != nil && rec.State != logs.StateRetained && rec.State != logs.StateUnavailable {
			return fmt.Errorf("%w: the log capture is in progress", execution.ErrBusy)
		}
	case errors.Is(err, logs.ErrNoLog), errors.Is(err, logs.ErrNotFound):
		// Nothing to retain: the wrapper uploaded no log and no live reader can.
	default:
		c.log.Warn("log observation failed before removal", "attempt", string(w.AttemptID), "error", err)
		return fmt.Errorf("%w: the log observation is not settled", execution.ErrBusy)
	}
	return c.Runtime.Remove(ctx, w)
}

// ---- the runtime API ---------------------------------------------------------

type runtimeAPIConfig struct {
	Auth      runtimehttp.Service
	Results   resultshttp.Service
	Verifier  runtimeauth.TokenVerifier
	Artifacts artifactshttp.RuntimeService
	// Deployments is optional: the deployment.call callbacks of the delivery
	// module, authenticated again by the service for the route's attempt.
	Deployments deliveryhttp.RuntimeService
	// Secrets is optional: the runtime secret route behind resolveSecret.
	Secrets   SecretResolver
	RateLimit int
	Log       *slog.Logger
	// V2 is set exactly when run_shared is composed: it adds the /runtime/v2
	// routes and fences the v1 shared routes with a v1 wire guard, so a run_shared
	// attempt cannot reach them on /runtime/v1.
	V2 *runtimeV2Config
}

// runtimeV2Config configures the /runtime/v2 handler.
type runtimeV2Config struct {
	Service         *runtimeauth.Service
	Begins          runtimehttp.BeginRecorder
	Receipts        runtimehttp.ReceiptRecorder
	ResourceSecrets runtimehttp.ResourceSecretResolver
}

// runtimeV2Prefix is the run_shared runtime API's path prefix.
const runtimeV2Prefix = "/runtime/v2"

// runtimePrefix is the runtime API's path prefix; the frozen contract's
// BasePath is runtimePrefix + "/attempts/{attemptId}".
const runtimePrefix = "/runtime/v1"

// newRuntimeAPI composes the runtime routes of the frozen contract. Each
// operation is served by exactly one owner; an operation without a handler
// (resolveSecret until the secret route is supplied) and any other path answer
// the contract's not_found envelope, so an unwired route is observable, never a
// silent success.
func newRuntimeAPI(c runtimeAPIConfig) (v1 http.Handler, v2 http.Handler, err error) {
	h, err := newRuntimeV1(c)
	if err != nil {
		return nil, nil, err
	}
	if c.V2 == nil {
		return h.mux, nil, nil
	}
	v2, err = newRuntimeV2(c, h)
	if err != nil {
		return nil, nil, err
	}
	return h.mux, v2, nil
}

// runtimeV1 is the v1 runtime API and the shared-body handlers the v2 routes
// reuse: one implementation serves both wires.
type runtimeV1 struct {
	mux http.Handler
	// shared are the v1 owners' handlers of the operations whose bodies v2
	// shares unchanged (runtimehttp.SharedOps), unguarded.
	shared map[string]http.Handler
}

func newRuntimeV1(c runtimeAPIConfig) (*runtimeV1, error) {
	auth, err := runtimehttp.NewHandler(runtimehttp.Config{Service: c.Auth, Logger: c.Log})
	if err != nil {
		return nil, err
	}
	resultsAPI, err := resultshttp.NewHandler(resultshttp.Config{Service: c.Results, Logger: c.Log})
	if err != nil {
		return nil, err
	}
	artifactRoutes, err := artifactshttp.NewRuntime(c.Artifacts, artifactshttp.DefaultActions())
	if err != nil {
		return nil, err
	}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: runtimeAuthenticator{verifier: c.Verifier}, Authorizer: runtimeAuthorizer{},
		RateLimiter: newRateLimiter(c.RateLimit, nil), Prefix: runtimePrefix, Logger: c.Log,
	})
	if err != nil {
		return nil, err
	}
	if err := artifactRoutes.RegisterRoutes(router); err != nil {
		return nil, fmt.Errorf("register runtime artifact routes: %w", err)
	}
	if reader, ok := c.Artifacts.(artifactshttp.RuntimeReadService); ok {
		readRoutes, err := artifactshttp.NewRuntimeRead(reader, artifactshttp.DefaultActions())
		if err != nil {
			return nil, err
		}
		if err := readRoutes.RegisterRoutes(router); err != nil {
			return nil, fmt.Errorf("register runtime artifact read routes: %w", err)
		}
	}
	var deployRoutes []deliveryhttp.RouteInfo
	if c.Deployments != nil {
		rt, err := deliveryhttp.NewRuntime(c.Deployments, deliveryhttp.DefaultRuntimeActions())
		if err != nil {
			return nil, err
		}
		if err := rt.RegisterRoutes(router); err != nil {
			return nil, fmt.Errorf("register runtime deployment routes: %w", err)
		}
		deployRoutes = deliveryhttp.RuntimeRoutes(deliveryhttp.DefaultRuntimeActions())
	}
	owner := map[string]http.Handler{}
	if c.Secrets != nil {
		h, err := newSecretHandler(c.Secrets, c.Log)
		if err != nil {
			return nil, err
		}
		owner["resolveSecret"] = h
	}
	for _, op := range runtimev1.Operations {
		switch op.ID {
		case "admit", "renewToken", "getContext":
			owner[op.ID] = auth
		case "proposeSource", "submitResult":
			owner[op.ID] = resultsAPI
		case "reserveArtifact", "completeArtifact":
			owner[op.ID] = router
		}
	}
	shared := map[string]http.Handler{}
	for _, id := range runtimehttp.SharedOps {
		if h, ok := owner[id]; ok {
			shared[id] = h
		}
	}
	if c.V2 != nil {
		// With run_shared composed, an attempt of a run_shared run belongs to the
		// v2 wire: the v1 routes whose bodies v2 shares are fenced so such an
		// attempt cannot reach them here (context_version_mismatch, nothing
		// changed), exactly as a v1 attempt cannot reach the v2 ones.
		guard, err := runtimehttp.NewWireGuard(c.V2.Service, runtimeauth.WireV1, c.Log)
		if err != nil {
			return nil, err
		}
		for id, h := range shared {
			owner[id] = guard.Wrap(id, h)
		}
	}
	mux := http.NewServeMux()
	for _, op := range runtimev1.Operations {
		h, ok := owner[op.ID]
		if !ok {
			continue // resolveSecret without a secret route: not wired (see ExecutionSlice.Disabled)
		}
		mux.Handle(op.Method+" "+runtimev1.BasePath+op.Path, h)
	}
	for _, route := range deployRoutes {
		// The delivery routes are not part of the frozen runtime contract: the
		// delivery module owns them (api/delivery.yaml) and they are mounted only
		// when the service is wired.
		mux.Handle(route.Method+" "+runtimePrefix+route.Path, router)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The auth handler answers every other path with the contract's
		// not_found / invalid_request envelope and names nothing internal.
		auth.ServeHTTP(w, r)
	})
	return &runtimeV1{mux: mux, shared: shared}, nil
}

// newRuntimeV2 composes the /runtime/v2 routes: admission, renewal and context
// of attempts and of run resources, the command-begin, the preparation receipt
// and the service secrets are served by the runtime authority over execution's
// committed state; the bodies shared with v1 (secrets, artifacts, source, result)
// are the v1 owners' handlers behind the handler's own v2 wire guard.
func newRuntimeV2(c runtimeAPIConfig, v1 *runtimeV1) (http.Handler, error) {
	if c.V2 == nil || c.V2.Service == nil || c.V2.Begins == nil || c.V2.Receipts == nil {
		return nil, fmt.Errorf("%w: the run_shared runtime routes need the runtime authority, the begin and the receipt recorders", execution.ErrNotConfigured)
	}
	return runtimehttp.NewV2Handler(runtimehttp.V2Config{
		Service: c.V2.Service, Begins: c.V2.Begins, Receipts: c.V2.Receipts, ResourceSecrets: c.V2.ResourceSecrets,
		Shared: v1.shared, Logger: c.Log,
	})
}

// runtimeAuthenticator verifies the runtime session token of the artifact
// routes and names its principal "runtime:<tenant>:<session>". It decides
// nothing about access: the artifact authority re-reads the durable grant, the
// signing generation and the attempt on every call.
type runtimeAuthenticator struct{ verifier runtimeauth.TokenVerifier }

func (a runtimeAuthenticator) Authenticate(ctx context.Context, r *http.Request) (httpapi.Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	claims, err := a.verifier.Verify(ctx, token)
	switch {
	case err == nil:
	case errors.Is(err, runtimeauth.ErrInvalidCredential):
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	default:
		return httpapi.Principal{}, err // an outage is never access
	}
	return httpapi.Principal{ID: resultsadapters.RuntimePrincipal(claims.Tenant, claims.Session)}, nil
}

// runtimeAuthorizer allows exactly the artifact actions to a runtime principal.
// Every other action, and every human principal, is denied: the runtime router
// has no route for them, and a future one is closed until it is listed here.
type runtimeAuthorizer struct{}

func (runtimeAuthorizer) Authorize(_ context.Context, p httpapi.Principal, action httpapi.Action, t httpapi.Target) error {
	a, d := artifactshttp.DefaultActions(), deliveryhttp.DefaultRuntimeActions()
	if !strings.HasPrefix(p.ID, "runtime:") {
		return httpapi.ErrDenied
	}
	switch action {
	case a.Reserve, a.Complete, a.Read:
		if t.Resource.Kind != artifactshttp.ResourceAttempt {
			return httpapi.ErrDenied
		}
	case d.Register, d.Lookup, d.Read, d.Outcome, d.CleanupResult:
		// The deployment service decides deployment.call for the attempt itself;
		// the router only keeps every other action closed.
		if t.Resource.Kind != deliveryhttp.ResourceAttempt {
			return httpapi.ErrDenied
		}
	default:
		return httpapi.ErrDenied
	}
	if domain.AttemptID(t.Resource.ID).Validate() != nil {
		return httpapi.ErrDenied
	}
	return nil
}

// ---- rules ---------------------------------------------------------------------
