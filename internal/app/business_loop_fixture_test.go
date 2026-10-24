package app

// Fixtures of the business-loop composition (T-075): the dependency graph, the
// ChatOps store and sender, the flow-summary reader and the QA ports as small
// deterministic in-memory adapters behind the modules' own consumer-owned ports.
// Every double flags itself development-only so `production` refuses it at
// construction. None of it is evidence of SQL, transaction, broker or network
// behavior: the real joins (the actual gate, outbox, JetStream delivery and
// replay) are T-044's business_loop_assembly, business_loop_gate_links and
// business_loop_event_replay. The modules' own PostgreSQL tests qualify the real
// stores.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/webhook"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/metrics"
	metricsauth "github.com/optimiware/kodama/internal/metrics/auth"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
)

const (
	// devOrigin is the canonical API origin of the fixture's review links.
	devOrigin = "https://kodama.example.test"
	// devHookHost is the one receiver host the fixture's destination policy allows.
	devHookHost = "hooks.example.test"
	// devMapper is the QA mapping principal of the fixture installation.
	devMapper = "u_qa_mapper"
)

// ---- dependency graph ------------------------------------------------------------

// memDeps is the dependency graph over the work memory store: a Repository (the
// use cases' persistence port) and the work module's DependencyGuard, so a
// ready → active transition reads the same links the routes write. The
// dependent's version is the work item's own plus the number of link changes (a
// virtual version: the memory store cannot be bumped without a work mutation).
type memDeps struct {
	mu    sync.Mutex
	items work.Repository
	links map[string]map[domain.WorkItemID]memLink
	bump  map[string]domain.Version
}

type memLink struct {
	at time.Time
	by work.Actor
}

func newMemDeps() *memDeps {
	return &memDeps{links: map[string]map[domain.WorkItemID]memLink{}, bump: map[string]domain.Version{}}
}

func (*memDeps) DevelopmentOnly() bool { return true }

func depKey(scope domain.Scope, id domain.WorkItemID) string {
	return string(scope.TenantID) + "/" + string(scope.ProjectID) + "/" + string(id)
}

var _ dependencies.Repository = (*memDeps)(nil)

func (m *memDeps) owner(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (dependencies.Owner, error) {
	it, err := m.items.Get(ctx, scope, id)
	if err != nil {
		return dependencies.Owner{}, err
	}
	return dependencies.Owner{Scope: scope, ID: id, State: it.State, Version: it.Version + m.bump[depKey(scope, id)], ContentRevision: it.ContentRevision}, nil
}

// List implements dependencies.Repository.
func (m *memDeps) List(ctx context.Context, scope domain.Scope, q dependencies.ListQuery) (dependencies.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, err := m.owner(ctx, scope, q.Dependent)
	if err != nil {
		return dependencies.Page{}, err
	}
	var ids []string
	for id := range m.links[depKey(scope, q.Dependent)] {
		if string(id) > string(q.After) {
			ids = append(ids, string(id))
		}
	}
	sort.Strings(ids)
	page := dependencies.Page{OwnerVersion: o.Version}
	for _, id := range ids {
		if len(page.Items) == q.Limit {
			break
		}
		l := m.links[depKey(scope, q.Dependent)][domain.WorkItemID(id)]
		link, err := dependencies.NewLink(scope, q.Dependent, domain.WorkItemID(id))
		if err != nil {
			return dependencies.Page{}, err
		}
		page.Items = append(page.Items, dependencies.Prerequisite{Link: link, CreatedAt: l.at, CreatedBy: l.by})
	}
	return page, nil
}

type memDepsTx struct {
	m *memDeps
}

func (t memDepsTx) PrerequisiteExists(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error {
	_, err := t.m.items.Get(ctx, scope, id)
	return err
}

func (t memDepsTx) LinkExists(_ context.Context, l dependencies.Link) (bool, error) {
	_, ok := t.m.links[depKey(l.Scope, l.DependentID)][l.PrerequisiteID]
	return ok, nil
}

func (t memDepsTx) PrerequisiteCount(_ context.Context, scope domain.Scope, dependent domain.WorkItemID) (int, error) {
	return len(t.m.links[depKey(scope, dependent)]), nil
}

func (t memDepsTx) DependsOn(_ context.Context, scope domain.Scope, from, target domain.WorkItemID) (bool, error) {
	seen := map[domain.WorkItemID]bool{}
	queue := []domain.WorkItemID{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == target {
			return true, nil
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for next := range t.m.links[depKey(scope, cur)] {
			queue = append(queue, next)
		}
	}
	return false, nil
}

// Mutate implements dependencies.Repository: the version is compared before the
// change is planned, and a plan that reports no change writes nothing.
func (m *memDeps) Mutate(ctx context.Context, req dependencies.MutationRequest, fn dependencies.MutateFunc) (dependencies.MutationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, err := m.owner(ctx, req.Scope, req.Dependent)
	if err != nil {
		return dependencies.MutationResult{}, err
	}
	if req.Expected != o.Version {
		return dependencies.MutationResult{}, fmt.Errorf("%w: the resource changed", work.ErrVersionConflict)
	}
	change, err := fn(ctx, memDepsTx{m}, o)
	if errors.Is(err, work.ErrNoChange) {
		return dependencies.MutationResult{Owner: o}, nil
	}
	if err != nil {
		return dependencies.MutationResult{}, err
	}
	key := depKey(req.Scope, req.Dependent)
	switch change.Operation {
	case dependencies.OpAdd:
		if m.links[key] == nil {
			m.links[key] = map[domain.WorkItemID]memLink{}
		}
		m.links[key][change.Link.PrerequisiteID] = memLink{at: change.History.OccurredAt, by: req.Actor}
	case dependencies.OpRemove:
		delete(m.links[key], change.Link.PrerequisiteID)
	}
	m.bump[key]++
	return dependencies.MutationResult{Owner: change.Owner, Changed: true}, nil
}

// Guard is the work module's dependency guard over the same links: every direct
// prerequisite must be done, read when the transition is evaluated.
func (m *memDeps) Guard() work.DependencyGuard { return memDepGuard{m} }

type memDepGuard struct{ m *memDeps }

func (g memDepGuard) DependenciesSatisfied(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error {
	g.m.mu.Lock()
	var prereqs []domain.WorkItemID
	for p := range g.m.links[depKey(scope, id)] {
		prereqs = append(prereqs, p)
	}
	g.m.mu.Unlock()
	var statuses []work.PrerequisiteStatus
	for _, p := range prereqs {
		st := work.PrerequisiteStatus{ID: p}
		if it, err := g.m.items.Get(ctx, scope, p); err == nil {
			st.Found, st.State = true, it.State
		}
		statuses = append(statuses, st)
	}
	return work.DependenciesComplete(statuses)
}

// ---- ChatOps ------------------------------------------------------------------------

// memChatOps is ChatOps' persistence over memory: the management Store, the
// worker's WorkStore and the event-bus binding, driven by the module's real
// Planner and the real Delivery state machine.
type memChatOps struct {
	mu         sync.Mutex
	auth       identity.Authority
	planner    chatops.Planner
	clock      func() time.Time
	dests      map[chatops.DestinationID]chatops.Destination
	deliveries map[chatops.DeliveryID]*chatops.Delivery
	// Handled counts the events the consumer routed to the planner.
	Handled int
}

var (
	_ chatops.Store     = (*memChatOps)(nil)
	_ chatops.WorkStore = (*memChatOps)(nil)
)

func newMemChatOps(auth identity.Authority, planner chatops.Planner, clock func() time.Time) *memChatOps {
	return &memChatOps{auth: auth, planner: planner, clock: clock,
		dests: map[chatops.DestinationID]chatops.Destination{}, deliveries: map[chatops.DeliveryID]*chatops.Delivery{}}
}

func (*memChatOps) DevelopmentOnly() bool { return true }

// Binding is the memory twin of chatopspg.NewBinding: the same consumer spec,
// and a handler that selects the project's enabled destinations and records the
// planner's intents once (an existing delivery identity is never reopened).
func (m *memChatOps) Binding() eventstest.MemBinding {
	return eventstest.MemBinding{Spec: chatops.ConsumerSpec(), Handle: func(_ context.Context, _ memdb.Tx, env eventsv1.Envelope) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.Handled++
		if env.ProjectID == "" || !chatops.IsSupportedEvent(env.Type) {
			return nil
		}
		var dests []chatops.Destination
		for _, d := range m.dests {
			if d.Enabled && d.Selects(env.Type) {
				dests = append(dests, d)
			}
		}
		slices.SortFunc(dests, func(a, b chatops.Destination) int { return compareStrings(string(a.ID), string(b.ID)) })
		intents, err := m.planner.Plan(env, dests)
		if err != nil {
			return events.Permanent(err)
		}
		for _, in := range intents {
			if _, exists := m.deliveries[in.ID]; !exists {
				in := in
				m.deliveries[in.ID] = &in
			}
		}
		return nil
	}}
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Deliveries returns a snapshot of the stored deliveries, ordered by identity.
func (m *memChatOps) Deliveries() []chatops.Delivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []chatops.Delivery
	for _, d := range m.deliveries {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b chatops.Delivery) int { return compareStrings(string(a.ID), string(b.ID)) })
	return out
}

// CheckAccess implements chatops.Store over the identity authority, with the
// adapter-local permission mapping of the module.
func (m *memChatOps) CheckAccess(ctx context.Context, c chatops.Caller, scope domain.Scope, perm chatops.Permission) error {
	var p identity.Permission
	switch perm {
	case chatops.PermSettings:
		p = identity.PermProjectSettings
	case chatops.PermRead:
		p = identity.PermProjectRead
	default:
		return chatops.Fail(chatops.ErrDenied, nil, "denied")
	}
	err := m.auth.Authorize(ctx, identity.Actor{ID: c.Actor.ID, Type: identity.ActorUser}, identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return chatops.Fail(chatops.ErrNotFound, nil, "not found")
	case errors.Is(err, identity.ErrDenied):
		return chatops.Fail(chatops.ErrDenied, nil, "denied")
	}
	return chatops.Fail(chatops.ErrUnavailable, err, "unavailable")
}

func (m *memChatOps) CreateDestination(ctx context.Context, c chatops.Caller, d chatops.Destination, limit int) (chatops.Destination, error) {
	if err := m.CheckAccess(ctx, c, d.Scope, chatops.PermSettings); err != nil {
		return chatops.Destination{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.dests {
		if e.Scope == d.Scope {
			n++
			if e.Name == d.Name {
				return chatops.Destination{}, chatops.Fail(chatops.ErrNameTaken, nil, "name taken")
			}
		}
	}
	if n >= limit {
		return chatops.Destination{}, chatops.Fail(chatops.ErrLimit, nil, "limit")
	}
	m.dests[d.ID] = d
	return d, nil
}

func (m *memChatOps) GetDestination(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DestinationID) (chatops.Destination, error) {
	if err := m.CheckAccess(ctx, c, scope, chatops.PermSettings); err != nil {
		return chatops.Destination{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.dests[id]
	if !ok || d.Scope != scope {
		return chatops.Destination{}, chatops.Fail(chatops.ErrNotFound, nil, "not found")
	}
	return d, nil
}

func (m *memChatOps) ListDestinations(ctx context.Context, c chatops.Caller, scope domain.Scope, after chatops.DestinationID, limit int) ([]chatops.Destination, error) {
	if err := m.CheckAccess(ctx, c, scope, chatops.PermSettings); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []chatops.Destination
	for _, d := range m.dests {
		if d.Scope == scope && d.ID > after {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b chatops.Destination) int { return compareStrings(string(a.ID), string(b.ID)) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memChatOps) UpdateDestination(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DestinationID, expected int64, edit chatops.DestinationEdit, now time.Time) (chatops.Destination, error) {
	if err := m.CheckAccess(ctx, c, scope, chatops.PermSettings); err != nil {
		return chatops.Destination{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.dests[id]
	if !ok || d.Scope != scope {
		return chatops.Destination{}, chatops.Fail(chatops.ErrNotFound, nil, "not found")
	}
	if _, err := d.Edit(expected, edit, now); err != nil {
		return chatops.Destination{}, err
	}
	m.dests[id] = d
	return d, nil
}

func (m *memChatOps) ListDeliveries(ctx context.Context, c chatops.Caller, scope domain.Scope, f chatops.DeliveryFilter, _ chatops.DeliveryPosition, limit int) ([]chatops.Delivery, error) {
	if err := m.CheckAccess(ctx, c, scope, chatops.PermRead); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []chatops.Delivery
	for _, d := range m.deliveries {
		if d.Scope == scope && (f.State == "" || f.State == d.State) && (f.Destination == "" || f.Destination == d.DestinationID) {
			out = append(out, *d)
		}
	}
	slices.SortFunc(out, func(a, b chatops.Delivery) int { return compareStrings(string(a.ID), string(b.ID)) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memChatOps) GetDelivery(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DeliveryID) (chatops.DeliveryView, error) {
	if err := m.CheckAccess(ctx, c, scope, chatops.PermRead); err != nil {
		return chatops.DeliveryView{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deliveries[id]
	if !ok || d.Scope != scope {
		return chatops.DeliveryView{}, chatops.Fail(chatops.ErrNotFound, nil, "not found")
	}
	return chatops.DeliveryView{Delivery: *d, DestinationName: m.dests[d.DestinationID].Name}, nil
}

func (m *memChatOps) ReplayDelivery(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DeliveryID, expected int64, reason string, now time.Time) (chatops.Delivery, error) {
	if err := m.CheckAccess(ctx, c, scope, chatops.PermSettings); err != nil {
		return chatops.Delivery{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deliveries[id]
	if !ok || d.Scope != scope {
		return chatops.Delivery{}, chatops.Fail(chatops.ErrNotFound, nil, "not found")
	}
	if d.Version != expected {
		return chatops.Delivery{}, chatops.Fail(chatops.ErrVersionConflict, nil, "the delivery changed")
	}
	if err := d.Replay(now, reason, m.dests[d.DestinationID].BindingRevision); err != nil {
		return chatops.Delivery{}, err
	}
	return *d, nil
}

// ClaimDue implements chatops.WorkStore: due deliveries of the tenant whose
// destination is enabled are claimed under a new fencing token; one whose
// destination binding changed is refused instead of redirected.
func (m *memChatOps) ClaimDue(_ context.Context, tenant domain.TenantID, now time.Time, lease time.Duration, limit int, p chatops.RetryPolicy) (chatops.ClaimBatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []*chatops.Delivery
	for _, d := range m.deliveries {
		if d.Scope.TenantID == tenant && d.Due(now) {
			due = append(due, d)
		}
	}
	slices.SortFunc(due, func(a, b *chatops.Delivery) int { return compareStrings(string(a.ID), string(b.ID)) })
	var batch chatops.ClaimBatch
	for _, d := range due {
		if len(batch.Claims) == limit {
			break
		}
		dest, ok := m.dests[d.DestinationID]
		if !ok || !dest.Enabled {
			continue
		}
		if dest.BindingRevision != d.BindingRevision {
			if err := d.Refuse(now); err != nil {
				return batch, err
			}
			batch.Refused++
			continue
		}
		res, err := d.Claim(now, lease, p)
		if err != nil {
			return batch, err
		}
		if res.Exhausted {
			batch.Exhausted++
			continue
		}
		batch.Claims = append(batch.Claims, chatops.Claim{Delivery: *d, Destination: dest, Attempt: d.CycleAttempts, Token: d.ClaimToken, LeaseExpiresAt: d.LeaseExpiresAt})
	}
	return batch, nil
}

// Finish implements chatops.WorkStore under the claim's current fencing token.
func (m *memChatOps) Finish(_ context.Context, c chatops.Claim, o chatops.Outcome, now time.Time, p chatops.RetryPolicy) (chatops.Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deliveries[c.Delivery.ID]
	if !ok {
		return chatops.Delivery{}, chatops.Fail(chatops.ErrNotFound, nil, "not found")
	}
	var err error
	if o.Delivered {
		err = d.Succeed(c.Token, now, o)
	} else {
		_, err = d.Fail(c.Token, now, o, p)
	}
	return *d, err
}

// recSender is a deterministic receiver: it records every request and answers
// with the scripted outcomes, then with a delivered 204.
type recSender struct {
	mu       sync.Mutex
	Requests []chatops.SendRequest
	script   []chatops.Outcome
	// refuse, while set, answers every request with that outcome (an outage).
	refuse *chatops.Outcome
}

// Refuse makes every following request fail with o until Refuse(nil).
func (s *recSender) Refuse(o *chatops.Outcome) {
	s.mu.Lock()
	s.refuse = o
	s.mu.Unlock()
}

var _ chatops.Sender = (*recSender)(nil)

func (*recSender) DevelopmentOnly() bool { return true }

func (s *recSender) Send(_ context.Context, r chatops.SendRequest) chatops.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, r)
	if s.refuse != nil {
		return *s.refuse
	}
	if len(s.script) > 0 {
		o := s.script[0]
		s.script = s.script[1:]
		return o
	}
	return chatops.Outcome{Delivered: true, HTTPStatus: 204, Receipt: "rcpt-1"}
}

func (s *recSender) Sent() []chatops.SendRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Requests)
}

// ---- flow summaries -----------------------------------------------------------------

// flowReader answers a summary for each requested item that exists and counts
// the calls, so a test can prove the reader is never reached by a denied caller.
type flowReader struct {
	mu    sync.Mutex
	Calls int
	known map[domain.WorkItemID]bool
}

var _ metrics.SummaryReader = (*flowReader)(nil)

func (*flowReader) DevelopmentOnly() bool { return true }

func (f *flowReader) ReadSummaries(_ context.Context, scope domain.Scope, ids []domain.WorkItemID, w metrics.Window) ([]metrics.WorkSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	var out []metrics.WorkSummary
	for _, id := range ids {
		if f.known[id] {
			out = append(out, metrics.WorkSummary{Scope: scope, ItemID: id, Status: metrics.StatusNotStarted, Window: w})
		}
	}
	return out, nil
}

// ---- QA mapping ---------------------------------------------------------------------

// qaPorts are inert QA ports: any read through them fails the test, which proves
// a refused caller never reaches the execution, deployment, artifact or work
// modules.
type qaPorts struct{ touched func() }

func (*qaPorts) DevelopmentOnly() bool { return true }

func (p *qaPorts) Facts(context.Context, domain.Scope, domain.AttemptID) (qa.AttemptFacts, error) {
	p.touched()
	return qa.AttemptFacts{}, qa.Failf(qa.ErrNotFound, "attempt not found")
}

func (p *qaPorts) Deployment(context.Context, domain.Scope, string) (qa.DeploymentFacts, error) {
	p.touched()
	return qa.DeploymentFacts{}, qa.Failf(qa.ErrNotFound, "deployment not found")
}

func (p *qaPorts) Describe(context.Context, domain.Scope, domain.ArtifactID) (qa.EvidenceMeta, error) {
	p.touched()
	return qa.EvidenceMeta{}, qa.Failf(qa.ErrNotFound, "artifact not found")
}

func (p *qaPorts) Read(context.Context, domain.Scope, qa.EvidenceMeta, int64) ([]byte, error) {
	p.touched()
	return nil, qa.Failf(qa.ErrNotFound, "artifact not found")
}

func (p *qaPorts) Pin(context.Context, domain.Scope, domain.ArtifactID, string) error {
	p.touched()
	return qa.Failf(qa.ErrNotFound, "artifact not found")
}

func (p *qaPorts) Subject(context.Context, domain.Scope, domain.WorkItemID) (qa.SubjectFacts, error) {
	p.touched()
	return qa.SubjectFacts{}, qa.Failf(qa.ErrNotFound, "work item not found")
}

func (p *qaPorts) Verify(context.Context, qa.DefectDraft) error { p.touched(); return nil }

func (p *qaPorts) Open(context.Context, qa.DefectDraft) (qa.Defect, bool, error) {
	p.touched()
	return qa.Defect{}, false, qa.Failf(qa.ErrUnavailable, "unavailable")
}

func (p *qaPorts) Attach(context.Context, domain.Scope, domain.AttemptID, string, domain.WorkItemID, []qa.EvidenceLink) (qa.Defect, int, error) {
	p.touched()
	return qa.Defect{}, 0, qa.Failf(qa.ErrUnavailable, "unavailable")
}

// ---- the composition ------------------------------------------------------------------

// fastChatOps is the worker tuning of the fixture: short schedules that are not
// production values.
func fastChatOps() ChatOpsTuning {
	return ChatOpsTuning{
		Retry:    chatops.RetryPolicy{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond, MaxAttempts: 3},
		Interval: 5 * time.Millisecond, Batch: 10, Concurrency: 2, Lease: 2 * time.Second, AttemptTimeout: time.Second, MaxDestinationsPerProject: 5,
	}
}

// devBusinessLoop holds the business-loop fixtures and builds the slice exactly
// as NewCore does apart from the adapters: AssembleBusinessLoop is the production
// composition; the stores, sender and readers are deterministic doubles.
type devBusinessLoop struct {
	t       *testing.T
	Deps    *memDeps
	Chat    *memChatOps
	Sender  *recSender
	Flow    *flowReader
	Planner chatops.Planner

	QAPorts   *qaPorts
	QATouched atomic.Int64

	Worker  *chatops.Worker
	Service *chatops.Service
	FlowSvc *metrics.Service
	QASvc   *qa.Service
}

func newDevBusinessLoop(t *testing.T) *devBusinessLoop {
	t.Helper()
	links, err := chatops.NewLinkBuilder(devOrigin)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := chatops.NewPlanner(links, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &devBusinessLoop{t: t, Deps: newMemDeps(), Sender: &recSender{}, Flow: &flowReader{known: map[domain.WorkItemID]bool{}}, Planner: planner}
	b.Chat = newMemChatOps(nil, planner, nil)
	b.QAPorts = &qaPorts{touched: func() { b.QATouched.Add(1) }}
	return b
}

// inputs builds the slice inputs; assemble builds the slice from them.
func (b *devBusinessLoop) inputs(env Env, tune ChatOpsTuning, log *slog.Logger, cursors *httpapi.CursorCodec, tenants execution.Tenants, workAuthority work.Authority) (BusinessLoopInputs, error) {
	depSvc, err := dependencies.NewService(dependencies.Config{Repository: b.Deps, Authority: workAuthority})
	if err != nil {
		return BusinessLoopInputs{}, err
	}
	readAuth, err := metricsauth.NewWorkReadAuthorizer(workAuthority)
	if err != nil {
		return BusinessLoopInputs{}, err
	}
	if b.FlowSvc, err = metrics.NewService(metrics.Config{Reader: b.Flow, Authorizer: readAuth}); err != nil {
		return BusinessLoopInputs{}, err
	}
	if b.QASvc, err = qa.NewService(qa.Config{
		Attempts: b.QAPorts, Deployments: b.QAPorts, Artifacts: b.QAPorts, Defects: b.QAPorts, ReportOutput: "qa-report", DeploymentInput: "deployment",
	}); err != nil {
		return BusinessLoopInputs{}, err
	}
	policy, err := webhook.NewPolicy(webhook.PolicyConfig{Hosts: []string{devHookHost}})
	if err != nil {
		return BusinessLoopInputs{}, err
	}
	if b.Service, err = chatops.NewService(chatops.Config{Store: b.Chat, Policy: policy, MaxDestinationsPerProject: tune.MaxDestinationsPerProject}); err != nil {
		return BusinessLoopInputs{}, err
	}
	if b.Worker, err = chatops.NewWorker(chatops.WorkerConfig{
		Work: b.Chat, Tenants: tenants, Sender: b.Sender, Retry: tune.Retry, Interval: tune.Interval, Batch: tune.Batch,
		Lease: tune.Lease, AttemptTimeout: tune.AttemptTimeout, Concurrency: tune.Concurrency, Logger: log,
	}); err != nil {
		return BusinessLoopInputs{}, err
	}
	return BusinessLoopInputs{
		Env: env, Log: log, Cursors: cursors, Dependencies: depSvc, Flow: b.FlowSvc, QA: b.QASvc,
		ChatOps:  &ChatOpsParts{Service: b.Service, Worker: b.Worker, Consumer: chatops.ConsumerSpec(), Durable: chatopsDurable},
		Adapters: []any{b.Deps, b.Chat, b.Sender, b.Flow, b.QAPorts},
	}, nil
}

func (b *devBusinessLoop) assemble(env Env, tune ChatOpsTuning, log *slog.Logger, cursors *httpapi.CursorCodec, tenants execution.Tenants, workAuthority work.Authority) (*BusinessLoopSlice, error) {
	in, err := b.inputs(env, tune, log, cursors, tenants, workAuthority)
	if err != nil {
		return nil, err
	}
	return AssembleBusinessLoop(in)
}
