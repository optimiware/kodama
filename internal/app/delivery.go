package app

// The manual agent → review → delivery slice (T-072).
//
// Two pure compositions, over injected adapters and no service, so constructor
// selection, authority, worker lifecycle and routing are unit-tested without a
// database (NewCore supplies the PostgreSQL-backed ones):
//
//   - BindDelivery builds the delivery-owned ports execution consumes (the
//     deployment-bound cleanup authorization, the retained cleanup environment,
//     the deployment effect reconciler and, where the store can bind it, the
//     environment resolver) so they reach execution.Dependencies instead of the
//     M1 "unbound" state.
//   - AssembleDelivery registers the human routes of agent profiles, gates and
//     deployment records and starts the gate reconciliation and the
//     deployment-bound cleanup scheduler of T-066/T-092.
//
// What this does not claim: the real SQL/deployment joins and the Podman/kind
// checkpoint are T-073, T-067 and T-038; a live model, provider or wrapper is
// T-031/T-085. Nothing here widens authority: every port is injected, every
// refusal is a construction error, and an absent optional capability disables
// exactly what needs it (recorded in DeliverySlice.Disabled).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	agentshttp "github.com/optimiware/kodama/internal/agents/http"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/gates"
	gateshttp "github.com/optimiware/kodama/internal/gates/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
)

// CleanupDefinitionName is the name of the project pipeline definition every
// deployment-bound cleanup run is admitted against (T-092 convention: it
// declares the string input deployment_id and the tools image's delete.sh
// reads it). The newest registered version of that name is the one run.
const CleanupDefinitionName = "kodama-cleanup"

// minCleanupSweep bounds how often the cleanup scheduler scans deployment
// records when the repair interval is shorter: a scan reads every project.
const minCleanupSweep = 30 * time.Second

// ---- execution bindings --------------------------------------------------------

// DeliveryPortInputs are the delivery-owned collaborators of the execution
// dependencies.
type DeliveryPortInputs struct {
	Env Env
	// Delivery is the delivery use-case layer: cleanup authorization, the retained
	// configuration view and the environment snapshots.
	Delivery interface {
		deliveryadapters.CleanupBoundary
		execadapters.RetainedViewer
		execadapters.SnapshotResolver
	}
	// Records is the scoped deployment record read of the effect reconciler.
	Records deliveryadapters.DeploymentRecords
	// Runs is the execution store: it resolves a cleanup run's deployment binding
	// and the declared environment of a step.
	Runs execution.Store
	// Bindings is the cleanup-run reader the delivery service was constructed with
	// (CleanupBindings): the one built from Runs.
	Bindings delivery.CleanupBindings
	// AdmitsEnvironments states whether the execution store binds environments in
	// its admission transaction (execution.EnvironmentStore). Only then are the
	// environment resolver and the cleanup environment bound: without the
	// transactional binding an environment-declaring run is refused by execution
	// itself, and nothing is retained for it.
	AdmitsEnvironments bool
	// Tools are the trusted qualifications of the delivery scripts' replay
	// protocol (installation configuration). Without one the effect reconciler
	// speaks for no step: every uncertain launch stays blocked.
	Tools []ToolQualification
}

// DeliveryPorts are the bound execution collaborators.
type DeliveryPorts struct {
	Cleanup             execution.DeploymentCleanup
	CleanupEnvironments execution.DeploymentEnvironments
	EffectReconciler    execution.EffectReconciler
	// Environments is nil unless the store binds environments.
	Environments execution.EnvironmentResolver
}

// CleanupBindings builds the delivery reader of a cleanup run's deployment over
// the execution store. It is the single instance handed both to the delivery
// service (the cleanup-result callback proves its binding through it) and to
// the effect reconciler.
func CleanupBindings(runs execution.Store) (delivery.CleanupBindings, error) {
	rb, err := execution.NewCleanupBindings(runs)
	if err != nil {
		return nil, err
	}
	return deliverycleanup.NewBindings(rb)
}

// BindDelivery builds the ports. A missing input is a construction error, never
// a default; an adapter flagged development-only is refused in production.
func BindDelivery(in DeliveryPortInputs) (DeliveryPorts, error) {
	missing := func(what string) error { return fmt.Errorf("%w: %s is required", execution.ErrNotConfigured, what) }
	switch {
	case !in.Env.Valid():
		return DeliveryPorts{}, ErrEnvUnknown
	case in.Delivery == nil:
		return DeliveryPorts{}, missing("the delivery service")
	case in.Records == nil:
		return DeliveryPorts{}, missing("the deployment records")
	case in.Runs == nil:
		return DeliveryPorts{}, missing("the execution store of the deployment effect reconciler")
	case in.Bindings == nil:
		return DeliveryPorts{}, missing("the cleanup-run bindings")
	}
	for _, a := range []any{in.Delivery, in.Records, in.Runs, in.Bindings} {
		if _, err := Admit(in.Env, PortStore, a); err != nil {
			return DeliveryPorts{}, err
		}
	}
	cleanup, err := deliveryadapters.NewCleanup(in.Delivery)
	if err != nil {
		return DeliveryPorts{}, err
	}
	effects, err := deliveryadapters.NewEffects(in.Records, in.Bindings)
	if err != nil {
		return DeliveryPorts{}, err
	}
	tools, err := NewToolQualifications(in.Tools)
	if err != nil {
		return DeliveryPorts{}, err
	}
	ports := DeliveryPorts{Cleanup: cleanup, EffectReconciler: deploymentEffects{inner: effects, runs: in.Runs, bindings: in.Bindings, tools: tools}}
	if in.AdmitsEnvironments {
		if ports.CleanupEnvironments, err = execadapters.NewCleanupEnvironments(in.Delivery); err != nil {
			return DeliveryPorts{}, err
		}
		if ports.Environments, err = execadapters.NewDeliveryEnvironments(in.Delivery); err != nil {
			return DeliveryPorts{}, err
		}
	}
	return ports, nil
}

// ToolProtocol names the replay protocol of a delivery script (T-065).
type ToolProtocol string

const (
	// ToolApply is apply.sh: it registers the deployment intent before the first
	// change, replays a settled action without effects and settles an unknown
	// outcome by observing the recorded objects.
	ToolApply ToolProtocol = "apply"
	// ToolCleanup is delete.sh: it deletes only the recorded owned objects and
	// observes them first.
	ToolCleanup ToolProtocol = "cleanup"
)

// ToolQualification is the installation's trusted statement that one pinned
// image and entrypoint implements a delivery script's replay protocol. Pipeline
// definitions accept arbitrary container commands, so neither a declared
// environment nor a deployment-bound cleanup run says anything about what a
// step does: only an exact match of the step's frozen image and command does.
type ToolQualification struct {
	Protocol ToolProtocol `json:"protocol"`
	// Image is the digest-pinned image reference, matched exactly.
	Image string `json:"image"`
	// Command is the entrypoint, matched exactly.
	Command []string `json:"command"`
	// Args, when set, must match exactly; when empty the step's arguments are not
	// part of the qualification (a script's own arguments are its input).
	Args []string `json:"args,omitempty"`
}

// ToolQualifications is the validated set.
type ToolQualifications struct{ list []ToolQualification }

// NewToolQualifications validates the qualifications: an unknown protocol, an
// image that is not digest-pinned, or an empty command is refused.
func NewToolQualifications(in []ToolQualification) (ToolQualifications, error) {
	out := make([]ToolQualification, 0, len(in))
	for i, q := range in {
		switch {
		case q.Protocol != ToolApply && q.Protocol != ToolCleanup:
			return ToolQualifications{}, fmt.Errorf("delivery tool %d: unknown protocol %q", i, q.Protocol)
		case !strings.Contains(q.Image, "@sha256:") || strings.ContainsAny(q.Image, " \t\r\n"):
			return ToolQualifications{}, fmt.Errorf("delivery tool %d: the image must be digest-pinned", i)
		case len(q.Command) == 0 || q.Command[0] == "":
			return ToolQualifications{}, fmt.Errorf("delivery tool %d: a command is required", i)
		}
		q.Command, q.Args = slices.Clone(q.Command), slices.Clone(q.Args)
		out = append(out, q)
	}
	return ToolQualifications{list: out}, nil
}

// Qualifies reports whether the step's frozen command is a qualified
// implementation of the protocol.
func (t ToolQualifications) Qualifies(p ToolProtocol, s pipeline.Step) bool {
	if s.Kind != pipeline.KindContainer || s.Image == "" {
		return false
	}
	for _, q := range t.list {
		if q.Protocol == p && q.Image == s.Image && slices.Equal(q.Command, s.Command) && (len(q.Args) == 0 || slices.Equal(q.Args, s.Args)) {
			return true
		}
	}
	return false
}

// deploymentEffects lets the delivery adapter speak only for the steps that
// implement its script protocol: an uncertain launch is judged by the
// deployment record only when the step's own pinned image and entrypoint are
// qualified as apply (and the step declares an environment) or, in a
// deployment-bound cleanup run, as cleanup. Every other step, including any
// other step of those runs, is "still unknown" and stays blocked, never
// replayed on the strength of a missing or requested record
// (internal/delivery/adapters/README.md).
type deploymentEffects struct {
	inner    execution.EffectReconciler
	runs     execution.Store
	bindings delivery.CleanupBindings
	tools    ToolQualifications
}

func (d deploymentEffects) Reconcile(ctx context.Context, q execution.EffectQuery) (execution.ReconciliationEvidence, error) {
	if err := q.Scope.Validate(); err != nil || q.RunID.Validate() != nil {
		return execution.ReconciliationEvidence{}, execution.Invalidf("an effect query needs a valid scope and run")
	}
	_, cleanup, err := d.bindings.Binding(ctx, q.Scope, q.RunID)
	if err != nil {
		return execution.ReconciliationEvidence{}, execution.Unavailablef(err, "the cleanup binding is unavailable")
	}
	agg, err := d.runs.Get(ctx, q.Scope, q.RunID)
	switch {
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return stillUnknown(), nil
	case err != nil:
		return execution.ReconciliationEvidence{}, execution.Unavailablef(err, "the run is unavailable")
	}
	st := agg.Step(q.StepID)
	if st == nil {
		return stillUnknown(), nil
	}
	if cleanup {
		if !d.tools.Qualifies(ToolCleanup, st.Spec) {
			return stillUnknown(), nil
		}
	} else if st.Spec.Environment == "" || !d.tools.Qualifies(ToolApply, st.Spec) {
		return stillUnknown(), nil
	}
	return d.inner.Reconcile(ctx, q)
}

func stillUnknown() execution.ReconciliationEvidence {
	return execution.ReconciliationEvidence{Verdict: execution.VerdictStillUnknown, Source: deliveryadapters.EffectSource}
}

// decoratedStore re-exposes what an embedding decorator of the execution store
// hides: the environment binding of the store it wraps (without which execution
// would treat a binding store as one that cannot revalidate environments) and
// its development flag (so the construction guard still sees a memory store
// under the gate and agent decorators).
type decoratedStore struct {
	execution.Store
	inner execution.Store
}

var _ execution.EnvironmentStore = decoratedStore{}

func (d decoratedStore) AdmitsEnvironments() bool { return admitsEnvironments(d.inner) }
func (d decoratedStore) DevelopmentOnly() bool    { return IsDevelopmentAdapter(d.inner) }

// Decorated marks outer as a decoration of inner: outer's transitions run on
// top of inner's, and inner's optional capabilities stay visible.
func Decorated(outer, inner execution.Store) execution.Store {
	return decoratedStore{Store: outer, inner: inner}
}

// ---- the slice -------------------------------------------------------------------

// DeliveryInputs are the collaborators of the manual agent/review/delivery
// slice.
type DeliveryInputs struct {
	Env     Env
	Log     *slog.Logger
	Tuning  Tuning
	Cursors *httpapi.CursorCodec

	// Slice is the assembled execution slice: cleanup runs are admitted through
	// its trusted service, never through a public route.
	Slice *ExecutionSlice

	// Profiles serves the agent-profile routes. Gates and GateDecisions serve the
	// human gate routes (GateDecisions joins a decision to the keyed
	// transaction); Delivery and Deployments serve environments and deployment
	// records.
	Profiles      *agents.Service
	Gates         *gates.Service
	GateDecisions gateshttp.Decider
	Delivery      *delivery.Service
	Deployments   deliveryhttp.Registrar

	// Cleanup scheduling (T-066/T-092). Records defaults to the delivery
	// service's record boundary. Scopes and Definitions are the composition's.
	Records     deliverycleanup.Records
	Scopes      deliverycleanup.Scopes
	Definitions deliverycleanup.Definitions
	Policy      deliverycleanup.Policy
	Clock       func() time.Time
	// CleanupEvery overrides the sweep interval (default Tuning.CleanupSweep, or
	// max(ReconcileInterval, 30s) when that is unset).
	CleanupEvery time.Duration

	// Capabilities that need a join this assembly cannot supply are recorded
	// here by the caller (for example the transaction-bound deployment reader of
	// deployment gates) and reported unchanged in Disabled.
	Unavailable []string
}

// DeliverySlice is the assembled slice.
type DeliverySlice struct {
	// RegisterPublic adds the agent-profile, gate and delivery routes.
	RegisterPublic func(*httpapi.Router) error
	// Workers are the slice's background loops, in start order.
	Workers []NamedWorker
	// Scheduler is the deployment-bound cleanup scheduler.
	Scheduler *deliverycleanup.Scheduler
	// Routes names what is registered; Disabled what is deliberately not wired.
	Routes   []string
	Disabled []string
}

// AssembleDelivery validates the inputs and builds the slice. Every refusal is
// a construction error: nothing starts and a missing port never degrades to an
// allow-all default.
func AssembleDelivery(in DeliveryInputs) (*DeliverySlice, error) {
	missing := func(what string) error { return fmt.Errorf("%w: %s is required", execution.ErrNotConfigured, what) }
	if !in.Env.Valid() {
		return nil, ErrEnvUnknown
	}
	if in.Log == nil {
		in.Log = slog.New(slog.DiscardHandler)
	}
	switch {
	case in.Slice == nil || in.Slice.Trusted == nil:
		return nil, missing("the assembled execution slice")
	case in.Cursors == nil:
		return nil, missing("the cursor codec")
	case in.Profiles == nil:
		return nil, missing("the agent profile service")
	case in.Gates == nil, in.GateDecisions == nil:
		return nil, missing("the gate service and its keyed decider")
	case in.Delivery == nil, in.Deployments == nil:
		return nil, missing("the delivery service and its keyed registrar")
	case in.Scopes == nil, in.Definitions == nil:
		return nil, missing("the cleanup scopes and definition source")
	case in.Tuning.ReconcileInterval <= 0:
		return nil, missing("a positive reconcile interval")
	}
	for _, a := range []any{in.Profiles, in.Gates, in.GateDecisions, in.Delivery, in.Deployments, in.Records, in.Scopes, in.Definitions} {
		if a == nil {
			continue
		}
		if _, err := Admit(in.Env, PortStore, a); err != nil {
			return nil, err
		}
	}

	profiles, err := agentshttp.NewHandler(in.Profiles, in.Cursors)
	if err != nil {
		return nil, fmt.Errorf("agent profile routes: %w", err)
	}
	gateHandler, err := gateshttp.NewHandler(gateshttp.Config{Service: in.Gates, Decider: in.GateDecisions, Cursors: in.Cursors, Clock: in.Clock})
	if err != nil {
		return nil, fmt.Errorf("gate routes: %w", err)
	}
	deliveryHandler, err := deliveryhttp.NewHandler(in.Delivery, in.Deployments, in.Cursors, deliveryhttp.DefaultActions())
	if err != nil {
		return nil, fmt.Errorf("delivery routes: %w", err)
	}

	records := in.Records
	if records == nil {
		if records, err = deliverycleanup.NewRecords(in.Delivery); err != nil {
			return nil, err
		}
	}
	sched, err := deliverycleanup.New(deliverycleanup.Config{
		Scopes: in.Scopes, Records: records, Runs: in.Slice.Trusted, Definitions: in.Definitions,
		Policy: in.Policy, Clock: in.Clock, Logger: in.Log,
	})
	if err != nil {
		return nil, fmt.Errorf("cleanup scheduler: %w", err)
	}

	every := in.CleanupEvery
	if every == 0 {
		every = in.Tuning.CleanupSweep
	}
	if every == 0 {
		every = max(in.Tuning.ReconcileInterval, minCleanupSweep)
	}
	if every < 0 {
		return nil, errors.New("app: the cleanup sweep interval must not be negative")
	}
	s := &DeliverySlice{
		Scheduler: sched,
		RegisterPublic: func(rt *httpapi.Router) error {
			for _, h := range []routeRegistrar{profiles, gateHandler, deliveryHandler} {
				if err := h.RegisterRoutes(rt); err != nil {
					return err
				}
			}
			return nil
		},
		Routes:   []string{"agent-profiles", "gates", "environments", "deployments"},
		Disabled: append([]string(nil), in.Unavailable...),
	}
	s.Workers = []NamedWorker{
		{Name: "gate-reconciler", Run: func(ctx context.Context) error {
			return loop(ctx, in.Tuning.ReconcileInterval, func(ctx context.Context) error {
				_, err := in.Gates.ReconcileAll(ctx)
				return err
			}, in.Log, "gate reconciliation")
		}},
		{Name: "deployment-cleanup", Run: func(ctx context.Context) error {
			return loop(ctx, every, func(ctx context.Context) error {
				rep, err := sched.Tick(ctx)
				for _, it := range rep.Items {
					if it.Outcome == deliverycleanup.OutcomeAdmitted {
						in.Log.Info("deployment cleanup admitted", "tenant", string(it.Scope.TenantID), "project", string(it.Scope.ProjectID), "run", string(it.RunID), "round", it.Round)
					}
				}
				return err
			}, in.Log, "deployment cleanup")
		}},
	}
	return s, nil
}

// ---- production adapters of the cleanup scheduler ----------------------------------

// ProjectScopes is the SQL-free port of the project enumeration: the project
// scopes of the installation in a stable order after a cursor.
type ProjectScopes func(ctx context.Context, after *domain.Scope, limit int) ([]domain.Scope, error)

// Scopes implements deliverycleanup.Scopes.
func (p ProjectScopes) Scopes(ctx context.Context, after *domain.Scope, limit int) ([]domain.Scope, error) {
	return p(ctx, after, limit)
}

// definitionLookup is the part of the pipeline service the cleanup definition
// source needs; the repository's list is ordered by creation.
type definitionLookup interface {
	List(ctx context.Context, scope domain.Scope, q pipeline.ListQuery) ([]pipeline.Definition, error)
}

// maxDefinitionPages bounds the walk to the newest version of the cleanup
// definition: a project that registered more versions than this is refused, not
// guessed.
const maxDefinitionPages = 50

// NamedCleanupDefinitions resolves a project's cleanup definition as the newest
// registered version of CleanupDefinitionName. No definition is
// deliverycleanup.ErrNoDefinition (the scheduler reports it and repeats on the
// next sweep).
type NamedCleanupDefinitions struct {
	Repo definitionLookup
	Name string
}

var _ deliverycleanup.Definitions = NamedCleanupDefinitions{}

// CleanupDefinition implements deliverycleanup.Definitions.
func (n NamedCleanupDefinitions) CleanupDefinition(ctx context.Context, scope domain.Scope) (domain.DefinitionID, error) {
	name := n.Name
	if name == "" {
		name = CleanupDefinitionName
	}
	const page = 100
	var newest *pipeline.Definition
	var after *pipeline.Position
	for i := 0; ; i++ {
		if i >= maxDefinitionPages {
			return "", fmt.Errorf("app: more than %d versions of the %q definition", maxDefinitionPages*page, name)
		}
		defs, err := n.Repo.List(ctx, scope, pipeline.ListQuery{Name: name, After: after, Limit: page})
		if err != nil {
			return "", err
		}
		for k := range defs {
			if defs[k].Scope != scope || defs[k].Name != name {
				continue // a definition that is not this project's own is never run
			}
			if newest == nil || defs[k].Version >= newest.Version {
				d := defs[k]
				newest = &d
			}
		}
		if len(defs) < page {
			break
		}
		last := defs[len(defs)-1]
		after = &pipeline.Position{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if newest == nil {
		return "", deliverycleanup.ErrNoDefinition
	}
	return newest.ID, nil
}
