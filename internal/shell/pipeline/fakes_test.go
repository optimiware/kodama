package pipeline_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// testNow is the injected authoritative time for every management command.
var testNow = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type sequentialIDs struct {
	mu   sync.Mutex
	next int
}

func (source *sequentialIDs) NewEventID() (id.EventID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.EventID(fmt.Sprintf("event-%d", source.next)), nil
}

func (source *sequentialIDs) NewReceiptID() (id.ReceiptID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", source.next)), nil
}

func unexpectedCall(name string) error {
	return fault.New(fault.InternalFailure, "unexpected port call "+name)
}

// fakeResolver records every registry read and lets each test pin the answer.
// The callback is invoked without the mutex held so a callback may inspect
// other fakes.
type fakeResolver struct {
	mu      sync.Mutex
	calls   []pipeline.ResolveRequest
	resolve func(context.Context, pipeline.ResolveRequest) (pipeline.ResolvedImage, error)
}

func (resolver *fakeResolver) Resolve(ctx context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	resolver.mu.Lock()
	resolver.calls = append(resolver.calls, request)
	callback := resolver.resolve
	resolver.mu.Unlock()
	if callback != nil {
		return callback(ctx, request)
	}
	return pipeline.ResolvedImage{Reference: request.Reference, Digest: digestFor(request.Reference)}, nil
}

func (resolver *fakeResolver) callCount() int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return len(resolver.calls)
}

func (resolver *fakeResolver) requests() []pipeline.ResolveRequest {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return append([]pipeline.ResolveRequest(nil), resolver.calls...)
}

// fakeReader is the scoped definition read seam. Each callback is optional; an
// unset callback is an unexpected call rather than a silent zero value.
type fakeReader struct {
	mu       sync.Mutex
	getCalls int
	byName   int
	list     int

	getFn    func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error)
	byNameFn func(context.Context, tenancy.Scope, string) (pipeline.Definition, error)
	listFn   func(context.Context, tenancy.Scope) ([]pipeline.Definition, error)
}

func (reader *fakeReader) Get(ctx context.Context, scope tenancy.Scope, pipelineID pipeline.PipelineID) (pipeline.Definition, error) {
	reader.mu.Lock()
	reader.getCalls++
	callback := reader.getFn
	reader.mu.Unlock()
	if callback == nil {
		return pipeline.Definition{}, unexpectedCall("Reader.Get")
	}
	return callback(ctx, scope, pipelineID)
}

func (reader *fakeReader) ByName(ctx context.Context, scope tenancy.Scope, name string) (pipeline.Definition, error) {
	reader.mu.Lock()
	reader.byName++
	callback := reader.byNameFn
	reader.mu.Unlock()
	if callback == nil {
		return pipeline.Definition{}, unexpectedCall("Reader.ByName")
	}
	return callback(ctx, scope, name)
}

func (reader *fakeReader) List(ctx context.Context, scope tenancy.Scope) ([]pipeline.Definition, error) {
	reader.mu.Lock()
	reader.list++
	callback := reader.listFn
	reader.mu.Unlock()
	if callback == nil {
		return nil, unexpectedCall("Reader.List")
	}
	return callback(ctx, scope)
}

func (reader *fakeReader) counts() (get, byName, list int) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.getCalls, reader.byName, reader.list
}

// fakeChangePort records every commit unit and returns the configured result.
type fakeChangePort struct {
	mu     sync.Mutex
	calls  []pipeline.ChangeUnit
	change func(context.Context, pipeline.ChangeUnit) (pipeline.ChangeResult, error)
}

func (port *fakeChangePort) Change(ctx context.Context, unit pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
	port.mu.Lock()
	port.calls = append(port.calls, cloneChangeUnit(unit))
	callback := port.change
	port.mu.Unlock()
	if callback != nil {
		return callback(ctx, unit)
	}
	return defaultChangeResult(unit), nil
}

func (port *fakeChangePort) callCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return len(port.calls)
}

func (port *fakeChangePort) captured() []pipeline.ChangeUnit {
	port.mu.Lock()
	defer port.mu.Unlock()
	return append([]pipeline.ChangeUnit(nil), port.calls...)
}

func defaultChangeResult(unit pipeline.ChangeUnit) pipeline.ChangeResult {
	pipelineID := unit.PipelineID
	if pipelineID == "" {
		pipelineID = pipeline.PipelineID("pipeline-created")
	}
	return pipeline.ChangeResult{PipelineID: pipelineID, Version: unit.ExpectedVersion + 1}
}

// fakeAuthorizer applies the declared registry grant matrix. Tests can override
// it to force an unavailable authorizer.
type authorizeCall struct {
	Principal authorize.Principal
	Operation authorize.OperationName
	Scope     tenancy.Scope
}

type fakeAuthorizer struct {
	mu        sync.Mutex
	calls     []authorizeCall
	authorize func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error)
}

func (authorizer *fakeAuthorizer) Authorize(ctx context.Context, principal authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Decision, error) {
	authorizer.mu.Lock()
	authorizer.calls = append(authorizer.calls, authorizeCall{Principal: principal, Operation: operation, Scope: scope})
	callback := authorizer.authorize
	authorizer.mu.Unlock()
	if callback != nil {
		return callback(ctx, principal, operation, scope)
	}
	return registryDecision(principal, operation), nil
}

func (authorizer *fakeAuthorizer) callCount() int {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return len(authorizer.calls)
}

func (authorizer *fakeAuthorizer) captured() []authorizeCall {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return append([]authorizeCall(nil), authorizer.calls...)
}

func registryDecision(principal authorize.Principal, operation authorize.OperationName) authorize.Decision {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.DecisionDeny
	}
	switch principal.Kind {
	case identity.KindHuman:
		if slices.ContainsFunc(registered.Requires.HumanRoles, principal.HasRole) {
			return authorize.DecisionAllow
		}
	case identity.KindService:
		if slices.ContainsFunc(registered.Requires.ServiceRoles, principal.HasRole) {
			return authorize.DecisionAllow
		}
	}
	return authorize.DecisionDeny
}

// fakeAuthorityReader returns one allowed fact per requested read by default.
// A test can override the callback to simulate a revoked membership or a stale
// version observed inside the commit.
type fakeAuthorityReader struct {
	mu    sync.Mutex
	calls [][]authorize.AuthorityRead
	read  func(context.Context, []authorize.AuthorityRead) ([]authorize.AuthorityFact, error)
}

func (reader *fakeAuthorityReader) Read(ctx context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	reader.mu.Lock()
	reader.calls = append(reader.calls, append([]authorize.AuthorityRead(nil), reads...))
	callback := reader.read
	reader.mu.Unlock()
	if callback != nil {
		return callback(ctx, reads)
	}
	return allowedFacts(reads), nil
}

func (reader *fakeAuthorityReader) callCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return len(reader.calls)
}

func (reader *fakeAuthorityReader) captured() [][]authorize.AuthorityRead {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([][]authorize.AuthorityRead(nil), reader.calls...)
}

func allowedFacts(reads []authorize.AuthorityRead) []authorize.AuthorityFact {
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		facts[index] = authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     true,
		}
	}
	return facts
}

// versionAwareAuthority answers every requested read from current store state.
// A versioned pipeline read reports the store's actual version, so a
// pre-commit version check would observe the advanced version after a committed
// retry. Membership can be revoked for a principal to model current-auth loss.
type versionAwareAuthority struct {
	mu      sync.Mutex
	store   *fakeStore
	revoked map[id.PrincipalID]bool
	reads   [][]authorize.AuthorityRead
}

func newVersionAwareAuthority(store *fakeStore) *versionAwareAuthority {
	return &versionAwareAuthority{store: store, revoked: make(map[id.PrincipalID]bool)}
}

func (reader *versionAwareAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	reader.mu.Lock()
	reader.reads = append(reader.reads, append([]authorize.AuthorityRead(nil), reads...))
	revoked := make(map[id.PrincipalID]bool, len(reader.revoked))
	maps.Copy(revoked, reader.revoked)
	reader.mu.Unlock()
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		fact := authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     true,
		}
		switch read.Check {
		case authorize.CheckPipelineVersion:
			fact.Version = reader.store.currentVersion(pipeline.PipelineID(read.TargetID))
		case authorize.CheckMembership:
			fact.Allowed = !revoked[read.PrincipalID]
		}
		facts[index] = fact
	}
	return facts, nil
}

func (reader *versionAwareAuthority) revokeMembership(principal id.PrincipalID) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.revoked[principal] = true
}

func (reader *versionAwareAuthority) lastReads() []authorize.AuthorityRead {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.reads) == 0 {
		return nil
	}
	return append([]authorize.AuthorityRead(nil), reader.reads[len(reader.reads)-1]...)
}

// barrier releases every waiter only after the configured number of arrivals.
type barrier struct {
	mu      sync.Mutex
	arrived int
	want    int
	ready   chan struct{}
}

func newBarrier(want int) *barrier {
	return &barrier{want: want, ready: make(chan struct{})}
}

func (b *barrier) Wait() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.want {
		close(b.ready)
	}
	b.mu.Unlock()
	<-b.ready
}

// fakeStore is a deterministic in-memory pipeline store. It implements both the
// ChangePort commit seam and the scoped Reader, so a retry sees the state the
// first commit produced. Receipt identity is checked before any version or name
// constraint, exactly as a real store must.
type fakeStore struct {
	mu sync.Mutex

	next      int
	defs      map[pipeline.PipelineID]pipeline.Definition
	owners    map[pipeline.PipelineID]tenancy.Scope
	names     map[string]pipeline.PipelineID
	receipts  map[receipt.Key]storedReceipt
	attempts  int
	commits   int
	replays   int
	changes   []pipeline.ChangeUnit
	barrier   *barrier
	loseFirst bool
	failFirst bool
}

type storedReceipt struct {
	contentHash string
	result      pipeline.ChangeResult
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		defs:     make(map[pipeline.PipelineID]pipeline.Definition),
		owners:   make(map[pipeline.PipelineID]tenancy.Scope),
		names:    make(map[string]pipeline.PipelineID),
		receipts: make(map[receipt.Key]storedReceipt),
	}
}

func nameKey(scope tenancy.Scope, name string) string {
	return string(scope.TenantID) + "\x00" + string(scope.ProjectID) + "\x00" + name
}

func (store *fakeStore) Change(_ context.Context, unit pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
	if store.barrier != nil {
		store.barrier.Wait()
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.attempts++
	store.changes = append(store.changes, cloneChangeUnit(unit))

	if stored, ok := store.receipts[unit.Receipt.Key]; ok {
		if stored.contentHash != unit.Receipt.ContentHash {
			return pipeline.ChangeResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
		}
		result := stored.result
		result.Replayed = true
		store.replays++
		return result, nil
	}
	if store.failFirst && store.attempts == 1 {
		return pipeline.ChangeResult{}, fault.New(fault.DependencyUnavailable, "commit did not start")
	}

	var result pipeline.ChangeResult
	switch unit.Kind {
	case pipeline.ChangeApply:
		if unit.PipelineID == "" {
			key := nameKey(unit.Owner, unit.Name)
			if _, exists := store.names[key]; exists {
				return pipeline.ChangeResult{}, fault.New(fault.VersionConflict, "pipeline name already exists")
			}
			store.next++
			pipelineID := pipeline.PipelineID(fmt.Sprintf("pipeline-%d", store.next))
			store.defs[pipelineID] = storedDefinition(unit, pipelineID, 1)
			store.owners[pipelineID] = unit.Owner
			store.names[key] = pipelineID
			result = pipeline.ChangeResult{PipelineID: pipelineID, Version: 1}
			break
		}
		current, ok := store.defs[unit.PipelineID]
		if !ok {
			return pipeline.ChangeResult{}, fault.New(fault.NotFound, "pipeline is not present")
		}
		if current.Version != unit.ExpectedVersion {
			return pipeline.ChangeResult{}, fault.New(fault.VersionConflict, "stale pipeline version")
		}
		next := current.Version + 1
		if unit.Name != current.Name {
			delete(store.names, nameKey(unit.Owner, current.Name))
			key := nameKey(unit.Owner, unit.Name)
			if _, exists := store.names[key]; exists {
				return pipeline.ChangeResult{}, fault.New(fault.VersionConflict, "pipeline name already exists")
			}
			store.names[key] = unit.PipelineID
		}
		store.defs[unit.PipelineID] = storedDefinition(unit, unit.PipelineID, next)
		result = pipeline.ChangeResult{PipelineID: unit.PipelineID, Version: next}
	case pipeline.ChangeUpdate:
		current, ok := store.defs[unit.PipelineID]
		if !ok {
			return pipeline.ChangeResult{}, fault.New(fault.NotFound, "pipeline is not present")
		}
		if current.Version != unit.ExpectedVersion {
			return pipeline.ChangeResult{}, fault.New(fault.VersionConflict, "stale pipeline version")
		}
		next := current.Version + 1
		if unit.Name != current.Name {
			delete(store.names, nameKey(unit.Owner, current.Name))
			key := nameKey(unit.Owner, unit.Name)
			if _, exists := store.names[key]; exists {
				return pipeline.ChangeResult{}, fault.New(fault.VersionConflict, "pipeline name already exists")
			}
			store.names[key] = unit.PipelineID
		}
		store.defs[unit.PipelineID] = storedDefinition(unit, unit.PipelineID, next)
		result = pipeline.ChangeResult{PipelineID: unit.PipelineID, Version: next}
	case pipeline.ChangeDelete:
		current, ok := store.defs[unit.PipelineID]
		if !ok {
			return pipeline.ChangeResult{}, fault.New(fault.NotFound, "pipeline is not present")
		}
		if current.Version != unit.ExpectedVersion {
			return pipeline.ChangeResult{}, fault.New(fault.VersionConflict, "stale pipeline version")
		}
		next := current.Version + 1
		delete(store.defs, unit.PipelineID)
		delete(store.owners, unit.PipelineID)
		delete(store.names, nameKey(unit.Owner, current.Name))
		result = pipeline.ChangeResult{PipelineID: unit.PipelineID, Version: next}
	default:
		return pipeline.ChangeResult{}, fault.New(fault.InvalidInput, "unknown change kind")
	}

	store.commits++
	store.receipts[unit.Receipt.Key] = storedReceipt{contentHash: unit.Receipt.ContentHash, result: result}
	if store.loseFirst && store.commits == 1 {
		return pipeline.ChangeResult{}, fault.New(fault.DependencyUnavailable, "response lost after commit")
	}
	return result, nil
}

func (store *fakeStore) Get(_ context.Context, scope tenancy.Scope, pipelineID pipeline.PipelineID) (pipeline.Definition, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	definition, ok := store.defs[pipelineID]
	if !ok || store.owners[pipelineID] != scope {
		return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present")
	}
	return cloneDefinition(definition), nil
}

func (store *fakeStore) ByName(_ context.Context, scope tenancy.Scope, name string) (pipeline.Definition, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	pipelineID, ok := store.names[nameKey(scope, name)]
	if !ok {
		return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present")
	}
	return cloneDefinition(store.defs[pipelineID]), nil
}

func (store *fakeStore) List(_ context.Context, scope tenancy.Scope) ([]pipeline.Definition, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	definitions := make([]pipeline.Definition, 0, len(store.defs))
	for pipelineID, definition := range store.defs {
		if store.owners[pipelineID] == scope {
			definitions = append(definitions, cloneDefinition(definition))
		}
	}
	sort.Slice(definitions, func(left, right int) bool { return definitions[left].ID < definitions[right].ID })
	return definitions, nil
}

func (store *fakeStore) counts() (attempts, commits, replays int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.attempts, store.commits, store.replays
}

func (store *fakeStore) currentVersion(pipelineID pipeline.PipelineID) uint64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.defs[pipelineID].Version
}

func (store *fakeStore) captured() []pipeline.ChangeUnit {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]pipeline.ChangeUnit(nil), store.changes...)
}

func cloneChangeUnit(unit pipeline.ChangeUnit) pipeline.ChangeUnit {
	cloned := unit
	cloned.YAML = append([]byte(nil), unit.YAML...)
	cloned.PushTriggers = cloneTestPushTriggers(unit.PushTriggers)
	cloned.Audit = append([]event.Audit(nil), unit.Audit...)
	cloned.Events = append([]event.Envelope(nil), unit.Events...)
	cloned.Authority = append([]authorize.AuthorityRead(nil), unit.Authority...)
	cloned.PostCommit = append([]event.PostCommitEffect(nil), unit.PostCommit...)
	return cloned
}

func cloneDefinition(definition pipeline.Definition) pipeline.Definition {
	definition.YAML = append([]byte(nil), definition.YAML...)
	definition.PushTriggers = cloneTestPushTriggers(definition.PushTriggers)
	if definition.Push != nil {
		push := *definition.Push
		push.Refs = append([]string(nil), definition.Push.Refs...)
		definition.Push = &push
	}
	return definition
}

func storedDefinition(unit pipeline.ChangeUnit, pipelineID pipeline.PipelineID, version uint64) pipeline.Definition {
	stored := pipeline.Definition{
		ID:           pipelineID,
		Name:         unit.Name,
		YAML:         append([]byte(nil), unit.YAML...),
		Version:      version,
		PushTriggers: cloneTestPushTriggers(unit.PushTriggers),
	}
	if len(stored.PushTriggers) > 0 {
		push := stored.PushTriggers[0]
		stored.Push = &push
	}
	return stored
}

func cloneTestPushTriggers(triggers []pipeline.PushTrigger) []pipeline.PushTrigger {
	if triggers == nil {
		return nil
	}
	cloned := make([]pipeline.PushTrigger, len(triggers))
	for index, trigger := range triggers {
		cloned[index] = pipeline.PushTrigger{
			Repository: trigger.Repository,
			Service:    trigger.Service,
			Refs:       append([]string(nil), trigger.Refs...),
		}
	}
	return cloned
}
