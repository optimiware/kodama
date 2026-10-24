package app

// The incremental business-loop assembly (T-075): evidence-to-defect mapping
// (T-040), real gate notifications (T-041), SQL flow summaries (T-042) and the
// business dependency graph (T-045), joined to the M2 composition before the
// full QA image/blueprint milestone join.
//
// Three pieces, kept apart so each is tested without a service:
//
//   - BusinessLoopConfig is the installation's own statement of the ChatOps
//     destination policy and canonical link origin, and of the QA mapping
//     principal. It is strict JSON read from the secrets directory
//     (business-loop.json) and empty by default: without it no notification is
//     planned or sent and no defect is mapped (fail closed, never an allow-all
//     default). The dependency graph and the flow summaries need no installation
//     statement and are always selected by the production constructor.
//   - AssembleBusinessLoop is a pure composition over injected services: the
//     dependency and ChatOps routes join the public router, the ChatOps delivery
//     worker is the one worker, and the slice names the event consumers the
//     composition must route (ChatOps only). Production refuses a development or
//     test adapter at construction.
//   - NewCore selects the PostgreSQL-backed adapters and the identity-backed
//     authority (business_loop_core.go). Doubles are injected only by tests.
//
// What this does not claim. Human completion stays explicit: nothing here marks
// work done, decides a gate or lets a QA caller approve; a notification carries a
// link to the current record and no authority, and following it asks the existing
// route to authorize the follower now. The mapping service has no route or
// trigger (the real browser image/profile and the scan that calls it are T-069);
// mapping fixtures are not exploratory QA. The flow-summary list contribution
// needs a hook in the work list handler (internal/work/http, outside this task's
// paths), so it is recorded as not wired. The real joins (actual gate, outbox,
// NATS and delivery behavior) are T-044's business_loop_assembly,
// business_loop_gate_links and business_loop_event_replay; M3 acceptance, the
// release/hosted decision gates and the Supabase blueprint (T-047) are untouched.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	chatopshttp "github.com/optimiware/kodama/internal/integrations/chatops/http"
	"github.com/optimiware/kodama/internal/integrations/chatops/webhook"
	"github.com/optimiware/kodama/internal/metrics"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/work/dependencies"
	dependencieshttp "github.com/optimiware/kodama/internal/work/dependencies/http"
)

// ---- installation configuration -----------------------------------------------

// BusinessLoopConfig is the installation's ChatOps and QA mapping statement. The
// zero value configures nothing.
type BusinessLoopConfig struct {
	// QA names the principal and the declared names the evidence-to-defect
	// mapping runs with. Absent, no mapping service is built.
	QA *QAMappingConfig `json:"qa,omitempty"`
	// ChatOps selects the notification destination policy and the canonical
	// origin of review links. Absent, no destination can be configured and no
	// notification is planned or sent.
	ChatOps *ChatOpsConfig `json:"chatops,omitempty"`
}

// QAMappingConfig is the QA half.
type QAMappingConfig struct {
	// MappingPrincipal is the identity-module principal defects are filed as: a
	// user-typed principal bound in each project with read, create and edit-spec
	// work permission (the adapter narrows it further). It is never a decision
	// authority: revoking its binding stops all mapping.
	MappingPrincipal string `json:"mappingPrincipal"`
	// ReportOutput is the declared artifact output name of the QA report.
	ReportOutput string `json:"reportOutput"`
	// DeploymentInput is the pinned run input naming the deployment under test.
	DeploymentInput string `json:"deploymentInput"`
}

// ChatOpsConfig is the ChatOps half.
type ChatOpsConfig struct {
	// Origin is the canonical https origin of the API as people reach it; every
	// review link is built from it, never from a request or an event.
	Origin string `json:"origin"`
	// Ports lists the permitted destination ports (empty: 443 only).
	Ports []int `json:"ports,omitempty"`
	// Hosts optionally restricts destinations to exact names or "*.suffix"
	// patterns (empty: any host that passes the address policy at dial time).
	Hosts []string `json:"hosts,omitempty"`
	// PrivateEndpoints are the exact, scoped private-network exceptions. There is
	// no CIDR, wildcard or allow-all form.
	PrivateEndpoints []PrivateEndpointConfig `json:"privateEndpoints,omitempty"`
	// CAFile adds private trust roots for the receivers (PEM).
	CAFile string `json:"caFile,omitempty"`
}

// PrivateEndpointConfig is one exact private destination of one tenant (and
// optionally one project).
type PrivateEndpointConfig struct {
	Tenant  string `json:"tenant"`
	Project string `json:"project,omitempty"`
	// Address is "ip:port".
	Address string `json:"address"`
}

// Configured reports whether the installation selected anything.
func (c BusinessLoopConfig) Configured() bool { return c.QA != nil || c.ChatOps != nil }

// LoadBusinessLoop reads the strict JSON document.
func LoadBusinessLoop(path string) (BusinessLoopConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return BusinessLoopConfig{}, err
	}
	var c BusinessLoopConfig
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return BusinessLoopConfig{}, fmt.Errorf("business loop file: %w", err)
	}
	if dec.More() {
		return BusinessLoopConfig{}, errors.New("business loop file: trailing content")
	}
	return c, nil
}

// Validate checks the document against the environment and the operational
// bounds: nothing is guessed, a malformed entry stops startup, and production
// refuses loopback private endpoints.
func (c BusinessLoopConfig) Validate(env Env, tune Tuning) error {
	var errs []error
	if q := c.QA; q != nil {
		if q.MappingPrincipal == "" || len(q.MappingPrincipal) > 256 {
			errs = append(errs, errors.New("the QA mapping needs its principal (1 to 256 bytes)"))
		}
		if q.ReportOutput == "" || q.DeploymentInput == "" {
			errs = append(errs, errors.New("the QA mapping needs the declared report output and the pinned deployment input names"))
		}
	}
	if co := c.ChatOps; co != nil {
		if _, err := co.linkBuilder(); err != nil {
			errs = append(errs, err)
		}
		if _, err := co.policy(env); err != nil {
			errs = append(errs, err)
		}
		if err := tune.ChatOps.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c ChatOpsConfig) linkBuilder() (chatops.LinkBuilder, error) {
	return chatops.NewLinkBuilder(c.Origin)
}

func (c ChatOpsConfig) policy(env Env) (*webhook.Policy, error) {
	cfg := webhook.PolicyConfig{Ports: slices.Clone(c.Ports), Hosts: slices.Clone(c.Hosts)}
	for _, e := range c.PrivateEndpoints {
		ap, err := netip.ParseAddrPort(e.Address)
		if err != nil {
			return nil, fmt.Errorf("chatops private endpoint %q: %w", e.Address, err)
		}
		if env == Production && (ap.Addr().IsLoopback() || ap.Addr().IsUnspecified()) {
			return nil, &AdapterError{Env: Production, Port: PortStore, Adapter: "chatops-loopback-endpoint", Reason: "a loopback private destination is a test fixture"}
		}
		cfg.PrivateEndpoints = append(cfg.PrivateEndpoints, webhook.PrivateEndpoint{
			Scope: domain.Scope{TenantID: domain.TenantID(e.Tenant), ProjectID: domain.ProjectID(e.Project)}, Addr: ap,
		})
	}
	return webhook.NewPolicy(cfg)
}

// ChatOpsTuning are the operational values of the notification worker. The
// module has no defaults of its own (T-041 leaves them to this assembly); these
// are explicit development and test values, not production selections: O-11
// leaves retention and recovery objectives to be qualified.
type ChatOpsTuning struct {
	Retry chatops.RetryPolicy
	// Interval is the period of the polling pass, Batch the claims per tenant per
	// pass and Concurrency the simultaneous sends within a pass.
	Interval    time.Duration
	Batch       int
	Concurrency int
	// Lease is how long a claim stays exclusive; AttemptTimeout, the deadline of
	// one send, is shorter so a send ends before another worker may reclaim.
	Lease          time.Duration
	AttemptTimeout time.Duration
	// MaxDestinationsPerProject bounds configured destinations.
	MaxDestinationsPerProject int
}

// DefaultChatOpsTuning returns the documented development values.
func DefaultChatOpsTuning() ChatOpsTuning {
	return ChatOpsTuning{
		Retry:    chatops.RetryPolicy{Initial: 30 * time.Second, Max: 15 * time.Minute, MaxAttempts: 8},
		Interval: 2 * time.Second, Batch: 20, Concurrency: 4,
		Lease: 2 * time.Minute, AttemptTimeout: 30 * time.Second, MaxDestinationsPerProject: 20,
	}
}

// Validate checks the values the module's constructors would refuse, so a bad
// tuning stops startup with one message instead of at the first worker pass.
func (t ChatOpsTuning) Validate() error {
	var errs []error
	if err := t.Retry.Validate(); err != nil {
		errs = append(errs, err)
	}
	if t.Interval <= 0 || t.Batch < 1 || t.Concurrency < 1 {
		errs = append(errs, errors.New("the ChatOps worker needs a positive interval, batch and concurrency"))
	}
	if t.Lease <= 0 || t.AttemptTimeout <= 0 || t.AttemptTimeout >= t.Lease {
		errs = append(errs, errors.New("the ChatOps attempt timeout must be positive and shorter than the claim lease"))
	}
	if t.MaxDestinationsPerProject < 1 || t.MaxDestinationsPerProject > 1000 {
		errs = append(errs, errors.New("the ChatOps destination limit must be 1 to 1000"))
	}
	return errors.Join(errs...)
}

// ---- the slice ---------------------------------------------------------------------

// ChatOpsParts are the selected ChatOps collaborators. The service and the
// worker are selected together: a worker without management routes could not be
// configured and routes without a worker would queue notifications nobody sends.
type ChatOpsParts struct {
	Service chatopshttp.Service
	Worker  *chatops.Worker
	// Consumer is the event-bus contribution the planner is bound to
	// (chatops.ConsumerSpec) and Durable the broker consumer name that routes
	// events to it.
	Consumer events.ConsumerSpec
	Durable  string
}

// BusinessLoopInputs are the collaborators of the slice.
type BusinessLoopInputs struct {
	Env     Env
	Log     *slog.Logger
	Cursors *httpapi.CursorCodec

	// Dependencies is the business dependency graph's use-case layer (T-045).
	Dependencies *dependencies.Service
	// Flow is the SQL flow-summary read model (T-042).
	Flow *metrics.Service
	// QA is the evidence-to-defect mapping (T-040); nil when the installation
	// named no mapping principal.
	QA *qa.Service
	// ChatOps is nil when the installation named no canonical origin.
	ChatOps *ChatOpsParts

	// Adapters lists every adapter below the services (stores, authorities,
	// binders, the outbound client), for the construction guard of the
	// environment: production refuses a development or test one.
	Adapters []any
}

// BusinessLoopConsumer names one event consumer the composition routes.
type BusinessLoopConsumer struct {
	Spec    events.ConsumerSpec
	Durable string
}

// BusinessLoopSlice is the assembled slice.
type BusinessLoopSlice struct {
	// RegisterPublic adds the dependency and ChatOps management routes.
	RegisterPublic func(*httpapi.Router) error
	// Workers are the loops, in start order.
	Workers []NamedWorker
	// Consumers are the event consumers the composition provisions and repairs.
	Consumers []BusinessLoopConsumer
	// Routes names what is registered; Disabled what is deliberately not wired.
	Routes   []string
	Disabled []string

	// The services, exported for the integration evidence and for the callers that
	// have no route yet (the QA mapping is called by the control plane only).
	Dependencies *dependencies.Service
	Flow         *metrics.Service
	QA           *qa.Service
	ChatOps      chatopshttp.Service
}

// Capability names recorded as not wired by the business-loop slice.
const (
	capFlowList   = "flow-summary-list"
	capQA         = "exploratory-qa"
	capChatOps    = "chatops"
	capChatCreds  = "chatops-authenticated-destinations"
	capChatFences = "chatops-restore-fence"
)

// AssembleBusinessLoop validates the inputs and builds the slice. A missing
// required service is a construction error; an absent optional one disables
// exactly what needs it and is recorded in Disabled.
func AssembleBusinessLoop(in BusinessLoopInputs) (*BusinessLoopSlice, error) {
	missing := func(what string) error { return fmt.Errorf("%w: %s is required", execution.ErrNotConfigured, what) }
	if !in.Env.Valid() {
		return nil, ErrEnvUnknown
	}
	switch {
	case in.Dependencies == nil:
		return nil, missing("the dependency service")
	case in.Flow == nil:
		return nil, missing("the flow summary service")
	case in.Cursors == nil:
		return nil, missing("the cursor codec")
	}
	if c := in.ChatOps; c != nil {
		switch {
		case c.Service == nil:
			return nil, missing("the ChatOps service")
		case c.Worker == nil:
			return nil, missing("the ChatOps delivery worker (it is selected with the service)")
		case c.Durable == "" || c.Consumer.Name == "":
			return nil, missing("the ChatOps event consumer")
		}
		if err := c.Consumer.Validate(); err != nil {
			return nil, fmt.Errorf("ChatOps event consumer: %w", err)
		}
	}
	guarded := []any{in.Dependencies, in.Flow}
	if in.QA != nil {
		guarded = append(guarded, in.QA)
	}
	if c := in.ChatOps; c != nil {
		guarded = append(guarded, c.Service, c.Worker)
	}
	for _, a := range append(guarded, in.Adapters...) {
		if a == nil {
			continue
		}
		if _, err := Admit(in.Env, PortStore, a); err != nil {
			return nil, err
		}
	}

	deps, err := dependencieshttp.NewHandler(in.Dependencies, in.Cursors)
	if err != nil {
		return nil, fmt.Errorf("dependency routes: %w", err)
	}
	s := &BusinessLoopSlice{
		Routes:       []string{"work-dependencies"},
		Dependencies: in.Dependencies, Flow: in.Flow, QA: in.QA,
		// The summaries are contributed to the items of the work list, whose handler
		// (T-011) has no hook for a contribution and is not owned by this task.
		Disabled: []string{capFlowList},
	}
	registrars := []routeRegistrar{deps}
	if c := in.ChatOps; c != nil {
		chat, err := chatopshttp.NewHandler(c.Service, in.Cursors)
		if err != nil {
			return nil, fmt.Errorf("ChatOps routes: %w", err)
		}
		registrars = append(registrars, chat)
		s.ChatOps = c.Service
		s.Routes = append(s.Routes, "chatops")
		s.Consumers = []BusinessLoopConsumer{{Spec: c.Consumer, Durable: c.Durable}}
		s.Workers = []NamedWorker{{Name: "chatops-delivery", Run: c.Worker.Run}}
		// Authenticated destinations need a recipient-bound credential source and the
		// restore fence an operator authority; neither is selected (T-061 qualifies
		// hosted custody), so the module refuses them rather than defaulting.
		s.Disabled = append(s.Disabled, capChatCreds, capChatFences)
	} else {
		s.Disabled = append(s.Disabled, capChatOps)
	}
	// There is no route or trigger for the mapping: a control-plane caller issues
	// Record after a QA attempt's accepted result, and the real browser image,
	// profile and the scan that calls it are T-069.
	s.Disabled = append(s.Disabled, capQA)
	s.RegisterPublic = func(rt *httpapi.Router) error {
		for _, r := range registrars {
			if err := r.RegisterRoutes(rt); err != nil {
				return err
			}
		}
		return nil
	}
	return s, nil
}

// DisabledCapabilitiesM3 is DisabledCapabilitiesM2 after the business-loop
// composition (T-075): ChatOps is no longer listed once it is wired and the
// slice adds its own refusals.
func DisabledCapabilitiesM3(slice *ExecutionSlice, d *DeliverySlice, m M2Slices, b *BusinessLoopSlice) []string {
	out := DisabledCapabilitiesM2(slice, d, m)
	if b == nil {
		return out
	}
	if b.ChatOps != nil {
		out = slices.DeleteFunc(out, func(name string) bool { return name == capChatOps })
	}
	for _, name := range b.Disabled {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}
