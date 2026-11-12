package event_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	testTenant     = id.TenantID("tenant-a")
	testProject    = id.ProjectID("project-a")
	otherTenant    = id.TenantID("tenant-b")
	otherProject   = id.ProjectID("project-b")
	testExecution  = id.ExecutionID("exec-1")
	testPrincipal  = id.PrincipalID("principal-exec")
	otherPrincipal = id.PrincipalID("principal-other")
	testCredential = "kodex.exec-1.credential-material"
	testOperation  = "event.publish"
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: otherTenant, ProjectID: otherProject}
}

// executionPrincipal is the running container action's authenticated execution
// credential. It is the only principal allowed to publish a custom
// observation.
func executionPrincipal() identity.Principal {
	return identity.Principal{
		ID:          testPrincipal,
		Kind:        identity.KindExecution,
		Scope:       testScope(),
		Enabled:     true,
		ExecutionID: testExecution,
	}
}

func humanPrincipal() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("principal-human"),
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

func servicePrincipal() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("svc-git"),
		Kind:    identity.KindService,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

func testSource() event.Source {
	return event.Source{Kind: event.SourceExecution, PrincipalID: testPrincipal}
}

func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want fault class %q", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok || got != want {
		t.Fatalf("fault class = %q (found=%v), want %q (err: %v)", got, ok, want, err)
	}
}

type fakeClock struct {
	now time.Time
}

func (clock fakeClock) Now() time.Time { return clock.now }

// idSequence supplies deterministic event and receipt identities so a retry
// can be observed generating a fresh identity while the committed identity is
// retained.
type idSequence struct {
	mu       sync.Mutex
	events   int
	receipts int
}

func (sequence *idSequence) nextEventID() (id.EventID, error) {
	sequence.mu.Lock()
	defer sequence.mu.Unlock()
	sequence.events++
	return id.EventID(fmt.Sprintf("event-%d", sequence.events)), nil
}

func (sequence *idSequence) nextReceiptID() (id.ReceiptID, error) {
	sequence.mu.Lock()
	defer sequence.mu.Unlock()
	sequence.receipts++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", sequence.receipts)), nil
}

type fakeCredentials struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
}

func (credentials *fakeCredentials) Resolve(_ context.Context, _ string) (identity.Principal, error) {
	credentials.mu.Lock()
	defer credentials.mu.Unlock()
	if credentials.err != nil {
		return identity.Principal{}, credentials.err
	}
	return credentials.principal, nil
}

// fakeAuthority returns one current fact per read in input order. A denied
// check models a revoked actor, a non-running action or an action past its
// deadline: the commit boundary refuses.
type fakeAuthority struct {
	mu     sync.Mutex
	denied map[authorize.AuthorityCheck]bool
	err    error
	calls  int
}

func (authority *fakeAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.calls++
	if authority.err != nil {
		return nil, authority.err
	}
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		facts[index] = authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Allowed:     !authority.denied[read.Check],
		}
	}
	return facts, nil
}

func (authority *fakeAuthority) deny(check authorize.AuthorityCheck) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.denied[check] = true
}

func (authority *fakeAuthority) setError(err error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.err = err
}

func (authority *fakeAuthority) callCount() int {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.calls
}

// fakeIngressPort is the in-memory adapter model: it validates the unit,
// deduplicates by the durable receipt through the production receipt.Match,
// commits the event row, receipt and audit together, and answers an identical
// retry with the original durable result. Scripted faults model a lost response
// after commit and a refusal before any effect.
type fakeIngressPort struct {
	mu       sync.Mutex
	receipts map[receipt.Key]receipt.Record
	events   map[id.EventID]event.Envelope
	units    []event.IngressUnit
	calls    int
	commits  int

	failAfterCommit    error
	refuseBeforeCommit error
}

func newFakeIngressPort() *fakeIngressPort {
	return &fakeIngressPort{
		receipts: map[receipt.Key]receipt.Record{},
		events:   map[id.EventID]event.Envelope{},
	}
}

func (port *fakeIngressPort) Ingest(_ context.Context, unit event.IngressUnit) (event.IngressResult, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.calls++

	if port.refuseBeforeCommit != nil {
		err := port.refuseBeforeCommit
		port.refuseBeforeCommit = nil
		return event.IngressResult{}, err
	}
	if err := unit.Validate(); err != nil {
		return event.IngressResult{}, err
	}
	if stored, ok := port.receipts[unit.Receipt.Key]; ok {
		outcome, err := receipt.Match(&stored, unit.Receipt)
		if err != nil {
			return event.IngressResult{}, err
		}
		if outcome == receipt.OutcomeReplay {
			return event.IngressResult{
				EventID:   id.EventID(string(stored.Result)),
				ReceiptID: stored.ID,
				Replayed:  true,
			}, nil
		}
	}

	port.receipts[unit.Receipt.Key] = unit.Receipt
	port.events[unit.Event.ID] = unit.Event
	port.units = append(port.units, unit)
	port.commits++

	result := event.IngressResult{EventID: unit.Event.ID, ReceiptID: unit.Receipt.ID}
	if port.failAfterCommit != nil {
		err := port.failAfterCommit
		port.failAfterCommit = nil
		return event.IngressResult{}, err
	}
	return result, nil
}

func (port *fakeIngressPort) callCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return port.calls
}

func (port *fakeIngressPort) commitCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return port.commits
}

func (port *fakeIngressPort) eventCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return len(port.events)
}

func (port *fakeIngressPort) unitAt(index int) event.IngressUnit {
	port.mu.Lock()
	defer port.mu.Unlock()
	if index < 0 || index >= len(port.units) {
		return event.IngressUnit{}
	}
	return port.units[index]
}

type ingressHarness struct {
	credentials *fakeCredentials
	authority   *fakeAuthority
	authorizer  authorize.Authorizer
	port        *fakeIngressPort
	clock       fakeClock
	ids         *idSequence
	ingress     *event.Ingress
}

func newIngressHarness(t *testing.T) *ingressHarness {
	t.Helper()
	harness := &ingressHarness{
		credentials: &fakeCredentials{principal: executionPrincipal()},
		authority:   &fakeAuthority{denied: map[authorize.AuthorityCheck]bool{}},
		authorizer:  authorize.NewMatrix(),
		port:        newFakeIngressPort(),
		clock:       fakeClock{now: fixedNow},
		ids:         &idSequence{},
	}
	ingress, err := event.NewIngress(event.IngressConfig{
		Credentials:  harness.credentials,
		Authority:    harness.authority,
		Authorizer:   harness.authorizer,
		Port:         harness.port,
		Clock:        harness.clock,
		NewEventID:   harness.ids.nextEventID,
		NewReceiptID: harness.ids.nextReceiptID,
	})
	if err != nil {
		t.Fatalf("NewIngress: %v", err)
	}
	harness.ingress = ingress
	return harness
}

func publishAttempt() event.PublishAttempt {
	return event.PublishAttempt{
		Credential: testCredential,
		CallerKey:  "caller-key-1",
		Content:    receipt.Content{MediaType: "application/json", Bytes: []byte(`{"note":"built"}`)},
		Type:       string(event.TypeCustomObservation),
		SubjectID:  "subject-1",
		Data:       []byte(`{"detail":"built"}`),
	}
}

// expectedIngressReads is exactly the event.publish registry row for the
// supplied actor kind: membership always, execution_running only when the
// actor is an execution, with the operation/grant context that lets an adapter
// re-evaluate the grant instead of guessing from a check name.
func expectedIngressReads(principal identity.Principal) []authorize.AuthorityRead {
	operation, ok := authorize.LookupOperation(authorize.OperationName(testOperation))
	if !ok {
		panic("event.publish is not a registered operation")
	}
	checks := []authorize.AuthorityCheck{authorize.CheckMembership}
	if principal.Kind == identity.KindExecution {
		checks = append(checks, authorize.CheckExecutionRunning)
	}
	reads := make([]authorize.AuthorityRead, 0, len(checks))
	for _, check := range checks {
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       principal.Scope,
			PrincipalID: principal.ID,
		}
		if check == authorize.CheckExecutionRunning {
			read.TargetID = string(principal.ExecutionID)
		}
		reads = append(reads, read)
	}
	return reads
}

func cloneUnit(unit event.IngressUnit) event.IngressUnit {
	clone := unit
	clone.Event.Data = append([]byte(nil), unit.Event.Data...)
	clone.Receipt.Result = append([]byte(nil), unit.Receipt.Result...)
	clone.Audit = append([]event.Audit(nil), unit.Audit...)
	clone.Authority = append([]authorize.AuthorityRead(nil), unit.Authority...)
	clone.PostCommit = append([]event.PostCommitEffect(nil), unit.PostCommit...)
	return clone
}

// validIngressUnit publishes one observation and returns the committed unit.
func validIngressUnit(t *testing.T) event.IngressUnit {
	t.Helper()
	harness := newIngressHarness(t)
	if _, err := harness.ingress.Publish(context.Background(), publishAttempt()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if harness.port.commitCount() != 1 {
		t.Fatalf("commits = %d, want 1", harness.port.commitCount())
	}
	return harness.port.unitAt(0)
}

// validHumanIngressUnit publishes one observation as an engineer human and
// returns the committed unit.
func validHumanIngressUnit(t *testing.T) event.IngressUnit {
	t.Helper()
	harness := newIngressHarness(t)
	harness.credentials.principal = humanPrincipal()
	if _, err := harness.ingress.Publish(context.Background(), publishAttempt()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if harness.port.commitCount() != 1 {
		t.Fatalf("commits = %d, want 1", harness.port.commitCount())
	}
	return harness.port.unitAt(0)
}

// pendingEnvelope builds one already-committed durable event row for dispatcher
// tests without depending on the constructor under test.
func pendingEnvelope(eventID id.EventID) event.Envelope {
	return event.Envelope{
		ID:        eventID,
		Type:      event.TypeCustomObservation,
		Source:    testSource(),
		At:        fixedNow,
		Scope:     testScope(),
		SubjectID: "subject-1",
	}
}

type pendingQuery struct {
	after event.Sequence
	limit int
}

type markCall struct {
	eventID  id.EventID
	sequence event.Sequence
}

// fakeOutbox is the event table as truth: only committed, not-yet-published
// rows are returned, in sequence order, bounded by the requested limit. A row
// can be reserved (given its durable sequence) while it is still invisible to
// readers, modelling a transaction that has not committed yet.
type fakeOutbox struct {
	mu              sync.Mutex
	rows            []event.PendingEvent
	hidden          map[event.Sequence]bool
	published       map[event.Sequence]bool
	markFailures    map[event.Sequence]int
	pendingErr      error
	pendingErrCalls int
	queries         []pendingQuery
	marks           []markCall
	attemptedMarks  []markCall
}

func newFakeOutbox(eventIDs ...id.EventID) *fakeOutbox {
	outbox := &fakeOutbox{
		hidden:       map[event.Sequence]bool{},
		published:    map[event.Sequence]bool{},
		markFailures: map[event.Sequence]int{},
	}
	for index, eventID := range eventIDs {
		outbox.rows = append(outbox.rows, event.PendingEvent{
			Sequence: event.Sequence(index + 1),
			Event:    pendingEnvelope(eventID),
		})
	}
	return outbox
}

// addRowAt records a durable event row with an explicit sequence. A row that is
// not visible models a transaction that reserved its sequence at insert but has
// not committed yet, so sequence order and commit-visibility order disagree.
func (outbox *fakeOutbox) addRowAt(sequence event.Sequence, eventID id.EventID, visible bool) {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	outbox.rows = append(outbox.rows, event.PendingEvent{
		Sequence: sequence,
		Event:    pendingEnvelope(eventID),
	})
	sort.SliceStable(outbox.rows, func(left, right int) bool {
		return outbox.rows[left].Sequence < outbox.rows[right].Sequence
	})
	if !visible {
		outbox.hidden[sequence] = true
	}
}

// reveal models a reserved row's transaction committing: the durable sequence
// was already assigned, but the row was not readable until now.
func (outbox *fakeOutbox) reveal(sequence event.Sequence) {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	delete(outbox.hidden, sequence)
}

func (outbox *fakeOutbox) Pending(_ context.Context, after event.Sequence, limit int) ([]event.PendingEvent, error) {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	outbox.queries = append(outbox.queries, pendingQuery{after: after, limit: limit})
	if outbox.pendingErr != nil && outbox.pendingErrCalls != 0 {
		if outbox.pendingErrCalls > 0 {
			outbox.pendingErrCalls--
		}
		return nil, outbox.pendingErr
	}
	pending := make([]event.PendingEvent, 0, limit)
	for _, row := range outbox.rows {
		if row.Sequence <= after || outbox.published[row.Sequence] || outbox.hidden[row.Sequence] {
			continue
		}
		if len(pending) == limit {
			break
		}
		pending = append(pending, row)
	}
	return pending, nil
}

func (outbox *fakeOutbox) MarkPublished(_ context.Context, eventID id.EventID, sequence event.Sequence) error {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	outbox.attemptedMarks = append(outbox.attemptedMarks, markCall{eventID: eventID, sequence: sequence})
	if remaining := outbox.markFailures[sequence]; remaining > 0 {
		outbox.markFailures[sequence] = remaining - 1
		return fault.New(fault.DependencyUnavailable, "acknowledgement lost")
	}
	outbox.published[sequence] = true
	outbox.marks = append(outbox.marks, markCall{eventID: eventID, sequence: sequence})
	return nil
}

func (outbox *fakeOutbox) markedSequences() []event.Sequence {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	sequences := make([]event.Sequence, len(outbox.marks))
	for index, mark := range outbox.marks {
		sequences[index] = mark.sequence
	}
	return sequences
}

func (outbox *fakeOutbox) queriedAfter() []event.Sequence {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	after := make([]event.Sequence, len(outbox.queries))
	for index, query := range outbox.queries {
		after[index] = query.after
	}
	return after
}

func (outbox *fakeOutbox) lastLimit() int {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	if len(outbox.queries) == 0 {
		return 0
	}
	return outbox.queries[len(outbox.queries)-1].limit
}

func (outbox *fakeOutbox) committedIDs() []id.EventID {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	identities := make([]id.EventID, len(outbox.rows))
	for index, row := range outbox.rows {
		identities[index] = row.Event.ID
	}
	return identities
}

// fakePublisher records every delivery attempt. A delivery that the scripted
// broker refuses is not recorded as delivered, so a retry can be compared with
// the failed attempt.
type fakePublisher struct {
	mu         sync.Mutex
	attempts   []event.Envelope
	deliveries []event.Envelope
	failures   map[id.EventID]int
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{failures: map[id.EventID]int{}}
}

func (publisher *fakePublisher) Publish(_ context.Context, envelope event.Envelope) error {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.attempts = append(publisher.attempts, envelope)
	if remaining := publisher.failures[envelope.ID]; remaining > 0 {
		publisher.failures[envelope.ID] = remaining - 1
		return fault.New(fault.DependencyUnavailable, "broker unavailable")
	}
	publisher.deliveries = append(publisher.deliveries, envelope)
	return nil
}

func (publisher *fakePublisher) failOnce(eventID id.EventID) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.failures[eventID]++
}

func (publisher *fakePublisher) deliveredIDs() []id.EventID {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	identities := make([]id.EventID, len(publisher.deliveries))
	for index, envelope := range publisher.deliveries {
		identities[index] = envelope.ID
	}
	return identities
}

func (publisher *fakePublisher) delivered() []event.Envelope {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	return append([]event.Envelope(nil), publisher.deliveries...)
}

func (publisher *fakePublisher) attempted() []event.Envelope {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	return append([]event.Envelope(nil), publisher.attempts...)
}

func newDispatcher(t *testing.T, outbox event.Outbox, publisher event.Publisher, batchSize int, idle func(context.Context) error) *event.Dispatcher {
	t.Helper()
	dispatcher, err := event.NewDispatcher(event.DispatcherConfig{
		Outbox:    outbox,
		Publisher: publisher,
		BatchSize: batchSize,
		Idle:      idle,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher
}
