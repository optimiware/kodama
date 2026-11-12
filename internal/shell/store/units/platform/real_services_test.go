package platform_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// The tests in this file drive the REAL capability orchestration
// (verify.Verifier, verify.Reporter and event.Ingress) into the REAL
// store-backed adapters over the memory store. They are the end-to-end shape
// proof: the adapters must accept every principal kind the real services
// produce, not only the execution-actor shape the capability tests happened to
// build.

// fixedPrincipals resolves every credential to one configured actor. The real
// verifier performs the enablement/scope checks itself; this fake only stands
// in for the credential boundary.
type fixedPrincipals struct {
	principal identity.Principal
	err       error
}

func (fixed fixedPrincipals) Resolve(context.Context, string) (identity.Principal, error) {
	if fixed.err != nil {
		return identity.Principal{}, fixed.err
	}
	return fixed.principal, nil
}

// fixedBindings answers with one binding that always names the query's resource
// and calling service and carries the configured version, so the adapter's
// binding-version guard is the fact under test.
type fixedBindings struct {
	version uint64
}

func (fixed fixedBindings) ReadBinding(_ context.Context, query verify.BindingQuery) (resource.Binding, error) {
	return resource.Binding{
		ID:               resource.BindingID(testBinding),
		ResourceID:       query.ResourceID,
		Scope:            query.Scope,
		EnforcingService: service.ServiceID(query.ServiceID),
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          fixed.version,
	}, nil
}

// emptyEvidence returns no prior request facts: these attempts request no
// approval, so the real verifier never consults it.
type emptyEvidence struct{}

func (emptyEvidence) ReadEvidence(context.Context, verify.EvidenceQuery) ([]verify.Evidence, error) {
	return nil, nil
}

// fixedClock is the deterministic transaction clock.
type fixedClock struct{}

func (fixedClock) Now() time.Time { return fixedNow }

// decisionAuthorizer is the real Authorizer seam with a fixed answer, so a deny
// and an allow case exercise the real Reporter/Ingress decision branch.
type decisionAuthorizer struct {
	decision authorize.Decision
}

func (authorizer decisionAuthorizer) Authorize(
	context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope,
) (authorize.Decision, error) {
	return authorizer.decision, nil
}

// allowedAuthority answers every read with the fact its read names, so the
// real Reporter/Ingress current-authority confirmation succeeds. A check named
// in denied is answered as not allowed.
type allowedAuthority struct {
	denied map[authorize.AuthorityCheck]bool
}

func (reader allowedAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		facts = append(facts, authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     !reader.denied[read.Check],
		})
	}
	return facts, nil
}

// fixedVerifications returns one already-committed verification record, so the
// real Reporter validates its linkage against the record the real Verifier
// committed through the real RecordAdapter.
type fixedVerifications struct {
	record verify.Record
}

func (reader fixedVerifications) ReadVerification(_ context.Context, scope tenancy.Scope, _ verify.VerificationID) (verify.Record, error) {
	if reader.record.Scope != scope {
		return verify.Record{}, fault.New(fault.NotFound, "verification record is not visible in this scope")
	}
	return reader.record, nil
}

// sequenceIDs allocates deterministic, valid event and receipt identities. The
// counter is shared so a replay test can hand back the same identities.
type sequenceIDs struct {
	nextEvent   int
	nextReceipt int
}

func (ids *sequenceIDs) NewEventID() (id.EventID, error) {
	ids.nextEvent++
	return id.EventID(fmt.Sprintf("event-%d", ids.nextEvent)), nil
}

func (ids *sequenceIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.nextReceipt++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.nextReceipt)), nil
}

func humanActorPrincipal() identity.Principal {
	return identity.Principal{
		ID:      testActor,
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

// testServicePrincipal is the enabled engineer service behind testService. It
// is the verified calling service of an attempt and, in the service-actor
// cases, the actor as well.
func testServicePrincipal() identity.Principal {
	return identity.Principal{
		ID:      testService,
		Kind:    identity.KindService,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

// executionActorPrincipal is the running execution actor every platform test
// resolves.
func executionActorPrincipal() identity.Principal {
	return identity.Principal{
		ID:          testPrincipal,
		Kind:        identity.KindExecution,
		Scope:       testScope(),
		Enabled:     true,
		Version:     1,
		ExecutionID: testExecution,
	}
}

// verificationAttempt builds the real verifier attempt one actor submits. The
// action identity is optional, exactly as Attempt allows.
func verificationAttempt(service identity.Principal, credential string, action id.ActionID) verify.Attempt {
	return verify.Attempt{
		Scope:      testScope(),
		Service:    service,
		Credential: credential,
		ResourceID: resource.ResourceID(testResource),
		Operation:  resource.OperationGitRefUpdate,
		Subject:    testSubject(),
		ActionID:   action,
	}
}

// realVerifier builds the real verifier with the real RecordAdapter and the
// real store-backed membership reader. The generated verification identity is
// unique per call so a retry is a new fact, not a duplicate insert.
func realVerifier(t *testing.T, s *memoryStoreAlias, actor identity.Principal) *verify.Verifier {
	t.Helper()
	records, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	memberships, err := identitystore.New(identitystore.Config{Executor: s, Scanner: s, Clock: fixedClock{}})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	next := 0
	verifier, err := verify.New(verify.Config{
		Principals: fixedPrincipals{principal: actor},
		Bindings:   fixedBindings{version: 7},
		Evidence:   emptyEvidence{},
		Membership: memberships,
		Records:    records,
		Clock:      fixedClock{},
		NewID: func() (verify.VerificationID, error) {
			next++
			return verify.VerificationID(fmt.Sprintf("verification-%d", next)), nil
		},
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	return verifier
}

// seedVerificationCommitFixtures seeds the durable facts every actor-kind
// verification commit re-reads: the calling service, the binding and the actor's
// own membership, service or execution row.
func seedVerificationCommitFixtures(t *testing.T, s *memoryStoreAlias, actor identity.Principal) {
	t.Helper()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	switch actor.Kind {
	case identity.KindHuman:
		seedMembership(t, s, testScope(), testMembership, actor.ID)
	case identity.KindService:
		// The actor service is the calling service here, so its row is already
		// seeded above.
		if actor.ID != testService {
			seedService(t, s, testScope(), actor.ID, "enabled")
		}
	case identity.KindExecution:
		seedExecution(t, s, testScope(), actor.ExecutionID, actor.ID, "running")
	}
}

// TestRealVerifierCommitsForEveryActorKind is a regression test: a human and a
// service actor can be verified through the real verifier, with or without an
// action id, and an execution actor keeps its execution/action guards. An
// adapter that required a target for execution_running and action_deadline
// would refuse every non-execution actor.
func TestRealVerifierCommitsForEveryActorKind(t *testing.T) {
	cases := []struct {
		name   string
		actor  identity.Principal
		action id.ActionID
	}{
		{name: "human without action", actor: humanActorPrincipal()},
		{name: "human with action", actor: humanActorPrincipal(), action: testAction},
		{name: "service without action", actor: testServicePrincipal()},
		{name: "service with action", actor: testServicePrincipal(), action: testAction},
		{name: "execution with action", actor: executionActorPrincipal(), action: testAction},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			seedVerificationCommitFixtures(t, s, testCase.actor)
			if testCase.action != "" {
				seedAction(t, s, testScope(), testCase.action, testExecution, "running", "")
			}
			verifier := realVerifier(t, s, testCase.actor)

			record, err := verifier.Verify(context.Background(),
				verificationAttempt(testServicePrincipal(), "actor-credential", testCase.action))
			if err != nil {
				t.Fatalf("real Verify for %s: %v", testCase.name, err)
			}
			if record.Result != verify.ResultApproved {
				t.Errorf("record result = %q, want approved", record.Result)
			}
			row, err := s.Read(context.Background(), testScope(), "verifications", string(record.ID))
			if err != nil {
				t.Fatalf("read committed verification: %v", err)
			}
			if got := row.Values["actor_id"]; got != string(testCase.actor.ID) {
				t.Errorf("actor_id = %v, want %q", got, testCase.actor.ID)
			}
			if got := row.Values["result"]; got != string(verify.ResultApproved) {
				t.Errorf("result = %v, want approved", got)
			}
			if got := row.Values["action_id"]; testCase.action == "" && got != nil {
				t.Errorf("action_id = %v, want NULL for an attempt without an action", got)
			}
		})
	}
}

// TestRealVerifierRefusesExecutionWithoutAction is the execution-authority
// regression. An execution actor's authority is its running execution, its
// running container action and that action's deadline together, and the
// execution credential is revoked while it waits for a request, so a
// verification that names no action has no deadline the commit boundary could
// guard. The real verifier must refuse it (invalid_input) and persist nothing.
// actor.verify is receipt-free, so the verification row, audit row and event are
// its complete durable footprint and requireNoVerificationState checks all
// three.
func TestRealVerifierRefusesExecutionWithoutAction(t *testing.T) {
	s := newStore()
	actor := executionActorPrincipal()
	seedVerificationCommitFixtures(t, s, actor)
	verifier := realVerifier(t, s, actor)

	_, err := verifier.Verify(context.Background(),
		verificationAttempt(testServicePrincipal(), "actor-credential", ""))
	requireClass(t, err, fault.InvalidInput)
	requireNoVerificationState(t, s)
}

// TestRealVerifierStaleGuardsPersistNothing asserts the real verifier's
// commit-boundary guards still refuse a binding version or action deadline that
// changed after the capability observed them, leaving no verification row.
func TestRealVerifierStaleGuardsPersistNothing(t *testing.T) {
	t.Run("stale binding version", func(t *testing.T) {
		s := newStore()
		seedVerificationCommitFixtures(t, s, humanActorPrincipal())
		// The verifier's binding read observed version 7; a concurrent edit
		// moved the durable row to version 8 before the commit.
		updateVersion(t, s, testScope(), "resource_bindings", testBinding, 7)
		verifier := realVerifier(t, s, humanActorPrincipal())

		_, err := verifier.Verify(context.Background(),
			verificationAttempt(testServicePrincipal(), "actor-credential", ""))
		requireClass(t, err, fault.VersionConflict)
		requireNoVerificationState(t, s)
	})

	t.Run("expired action deadline for a human actor", func(t *testing.T) {
		s := newStore()
		seedVerificationCommitFixtures(t, s, humanActorPrincipal())
		seedActionWithDeadline(t, s, testScope(), testAction, testExecution, "running", "", fixedNow)
		verifier := realVerifier(t, s, humanActorPrincipal())

		_, err := verifier.Verify(context.Background(),
			verificationAttempt(testServicePrincipal(), "actor-credential", testAction))
		requireClass(t, err, fault.DeadlineExpired)
		requireNoVerificationState(t, s)
	})

	t.Run("expired action deadline for an execution actor", func(t *testing.T) {
		s := newStore()
		seedVerificationCommitFixtures(t, s, executionActorPrincipal())
		seedActionWithDeadline(t, s, testScope(), testAction, testExecution, "running", "", fixedNow)
		verifier := realVerifier(t, s, executionActorPrincipal())

		_, err := verifier.Verify(context.Background(),
			verificationAttempt(testServicePrincipal(), "actor-credential", testAction))
		requireClass(t, err, fault.DeadlineExpired)
		requireNoVerificationState(t, s)
	})
}

// requireNoVerificationState asserts a refused commit persisted no verification
// row, receipt, audit row or event.
func requireNoVerificationState(t *testing.T, s *memoryStoreAlias) {
	t.Helper()
	for _, identity := range []string{"verification-1", "verification-2"} {
		if _, err := s.Read(context.Background(), testScope(), "verifications", identity); err == nil {
			t.Errorf("refused verification %q was persisted", identity)
		}
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after refusal, want 0", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after refusal, want 0", len(events))
	}
}

// TestRealReporterCommitsAndReplaysReport drives the real Reporter into the real
// ReportAdapter using the verification the real Verifier committed, then replays
// the identical report and asserts the original event is not duplicated.
func TestRealReporterCommitsAndReplaysReport(t *testing.T) {
	s := newStore()
	actor := executionActorPrincipal()
	seedVerificationCommitFixtures(t, s, actor)
	seedAction(t, s, testScope(), testAction, testExecution, "running", "")
	mustInsert(t, s, testScope(), "resources", testResource, map[string]any{
		"kind": "git", "version": uint64(1), "metadata": []byte(`{}`), "created_at": fixedNow,
	})

	verifier := realVerifier(t, s, actor)
	record, err := verifier.Verify(context.Background(),
		verificationAttempt(testServicePrincipal(), "actor-credential", testAction))
	if err != nil {
		t.Fatalf("real Verify: %v", err)
	}

	reports, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	ids := &sequenceIDs{}
	reporter, err := verify.NewReporter(verify.ReportConfig{
		Authorizer:    decisionAuthorizer{decision: authorize.DecisionAllow},
		Authority:     allowedAuthority{},
		Bindings:      fixedBindings{version: 7},
		Verifications: fixedVerifications{record: record},
		Reports:       reports,
		Clock:         fixedClock{},
		IDs:           ids,
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	command := verify.ReportCommand{
		Scope:          testScope(),
		Service:        testServicePrincipal(),
		SourceEventID:  "source-event-1",
		EventType:      event.ReportType("git.ref.updated"),
		VerificationID: record.ID,
		ResourceID:     resource.ResourceID(testResource),
		Operation:      resource.OperationGitRefUpdate,
		Subject:        testSubject(),
		Data:           []byte(`{"commit":"abc"}`),
	}

	first, err := reporter.Report(context.Background(), command)
	if err != nil {
		t.Fatalf("real Report: %v", err)
	}
	if first.Replayed {
		t.Error("first report was marked replayed")
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Fatalf("events = %d after first report, want 1", len(events))
	}

	second, err := reporter.Report(context.Background(), command)
	if err != nil {
		t.Fatalf("real Report replay: %v", err)
	}
	if !second.Replayed || second.EventID != first.EventID {
		t.Errorf("replayed report = %#v, want the original event %q", second, first.EventID)
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d after replay, want 1", len(events))
	}
}

// TestRealIngressCommitsForEverySourceKind drives the real event.Ingress into
// the real IngressAdapter for a human, service and execution source. The
// execution source's membership read is satisfied by its execution permission
// and adds no lock; its execution_running read guards the execution row.
func TestRealIngressCommitsForEverySourceKind(t *testing.T) {
	cases := []struct {
		name      string
		principal identity.Principal
	}{
		{name: "human source", principal: humanActorPrincipal()},
		{name: "service source", principal: testServicePrincipal()},
		{name: "execution source", principal: executionActorPrincipal()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			switch testCase.principal.Kind {
			case identity.KindHuman:
				seedMembership(t, s, testScope(), testMembership, testCase.principal.ID)
			case identity.KindService:
				seedService(t, s, testScope(), testCase.principal.ID, "enabled")
			case identity.KindExecution:
				seedExecution(t, s, testScope(), testCase.principal.ExecutionID, testCase.principal.ID, "running")
			}
			ingress := realIngress(t, s, testCase.principal, authorize.DecisionAllow, nil)
			attempt := ingressAttempt()

			first, err := ingress.Publish(context.Background(), attempt)
			if err != nil {
				t.Fatalf("real Publish for %s: %v", testCase.name, err)
			}
			if first.Replayed {
				t.Error("first publish was marked replayed")
			}
			if events := s.Events(testScope()); len(events) != 1 {
				t.Fatalf("events = %d after first publish, want 1", len(events))
			}

			second, err := ingress.Publish(context.Background(), attempt)
			if err != nil {
				t.Fatalf("real Publish replay for %s: %v", testCase.name, err)
			}
			if !second.Replayed || second.EventID != first.EventID {
				t.Errorf("replayed publish = %#v, want the original event %q", second, first.EventID)
			}
			if events := s.Events(testScope()); len(events) != 1 {
				t.Errorf("events = %d after replay, want 1", len(events))
			}
		})
	}
}

// TestRealIngressDenyPersistsNothing asserts the real Ingress authorizer deny
// branch and a denied current-authority fact both refuse before the adapter
// writes anything.
func TestRealIngressDenyPersistsNothing(t *testing.T) {
	t.Run("grant denied", func(t *testing.T) {
		s := newStore()
		seedMembership(t, s, testScope(), testMembership, testActor)
		ingress := realIngress(t, s, humanActorPrincipal(), authorize.DecisionDeny, nil)
		_, err := ingress.Publish(context.Background(), ingressAttempt())
		requireClass(t, err, fault.Forbidden)
		requireNoIngressState(t, s)
	})
	t.Run("authority fact denied", func(t *testing.T) {
		s := newStore()
		seedMembership(t, s, testScope(), testMembership, testActor)
		ingress := realIngress(t, s, humanActorPrincipal(), authorize.DecisionAllow,
			map[authorize.AuthorityCheck]bool{authorize.CheckMembership: true})
		_, err := ingress.Publish(context.Background(), ingressAttempt())
		requireClass(t, err, fault.Forbidden)
		requireNoIngressState(t, s)
	})
}

func requireNoIngressState(t *testing.T, s *memoryStoreAlias) {
	t.Helper()
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after refusal, want 0", len(events))
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after refusal, want 0", len(audits))
	}
	key := receipt.Key{
		TenantID:    testTenant,
		ProjectID:   testProject,
		PrincipalID: testActor,
		Operation:   "event.publish",
		CallerKey:   "caller-1",
	}
	if _, found := s.Receipt(key); found {
		t.Error("a refused ingress committed its receipt")
	}
}

// realIngress builds the real event.Ingress with the real IngressAdapter.
func realIngress(
	t *testing.T,
	s *memoryStoreAlias,
	principal identity.Principal,
	decision authorize.Decision,
	denied map[authorize.AuthorityCheck]bool,
) *event.Ingress {
	t.Helper()
	port, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	ids := &sequenceIDs{}
	ingress, err := event.NewIngress(event.IngressConfig{
		Credentials:  fixedPrincipals{principal: principal},
		Authority:    allowedAuthority{denied: denied},
		Authorizer:   decisionAuthorizer{decision: decision},
		Port:         port,
		Clock:        fixedClock{},
		NewEventID:   ids.NewEventID,
		NewReceiptID: ids.NewReceiptID,
	})
	if err != nil {
		t.Fatalf("NewIngress: %v", err)
	}
	return ingress
}

func ingressAttempt() event.PublishAttempt {
	return event.PublishAttempt{
		Credential: "actor-credential",
		CallerKey:  "caller-1",
		Content:    receipt.Content{MediaType: "application/json", Bytes: []byte(`{"observation":1}`)},
		Type:       string(event.TypeCustomObservation),
		SubjectID:  "subject-1",
	}
}

// TestRealIngressHumanUnionReadsBothGrants drives the REAL event.Ingress into
// the real adapter with a human whose tenant-wide row grants Engineer while a
// project row grants Reviewer. The project row only adds: if it shadowed
// the tenant-wide grant, event.publish would be refused.
func TestRealIngressHumanUnionReadsBothGrants(t *testing.T) {
	s := newStore()
	seedMembershipRoles(t, s, tenancy.Scope{TenantID: testTenant}, "membership-tenant", testActor, []tenancy.Role{tenancy.RoleEngineer})
	seedMembershipRoles(t, s, testScope(), "membership-project", testActor, []tenancy.Role{tenancy.RoleReviewer})
	ingress := realIngress(t, s, humanActorPrincipal(), authorize.DecisionAllow, nil)

	result, err := ingress.Publish(context.Background(), ingressAttempt())
	if err != nil {
		t.Fatalf("real Publish with the membership union: %v", err)
	}
	if result.EventID == "" {
		t.Error("the union commit returned no event id")
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want 1", len(events))
	}
}

// TestRealVerifierServiceUnionReadsTenantWideGrant drives the REAL verifier into
// the real record adapter for a service whose only service row is tenant-wide.
// The union reads the tenant-wide row and re-evaluates the service roles, so the
// project-scoped verification still commits; it also never grants Admin.
func TestRealVerifierServiceUnionReadsTenantWideGrant(t *testing.T) {
	s := newStore()
	seedService(t, s, tenancy.Scope{TenantID: testTenant}, testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	verifier := realVerifier(t, s, testServicePrincipal())

	record, err := verifier.Verify(context.Background(),
		verificationAttempt(testServicePrincipal(), "actor-credential", ""))
	if err != nil {
		t.Fatalf("real Verify with the tenant-wide service union: %v", err)
	}
	if record.Result != verify.ResultApproved {
		t.Errorf("record result = %q, want approved", record.Result)
	}
	if _, err := s.Read(context.Background(), testScope(), "verifications", string(record.ID)); err != nil {
		t.Errorf("verification row was not committed: %v", err)
	}
}

// TestRealVerifierHumanScopedRolesReadBothRows drives the REAL verifier through
// the real store-backed membership reader for a human whose tenant-wide row
// grants Viewer while the exact-project row grants Engineer. The verified roles
// are the union in the resource scope, and the record commit re-reads and pins
// both membership rows.
func TestRealVerifierHumanScopedRolesReadBothRows(t *testing.T) {
	s := newStore()
	seedVerificationCommitFixtures(t, s, humanActorPrincipal())
	seedMembershipRoles(t, s, tenancy.Scope{TenantID: testTenant}, "membership-tenant", testActor, []tenancy.Role{tenancy.RoleViewer})
	verifier := realVerifier(t, s, humanActorPrincipal())

	record, err := verifier.Verify(context.Background(),
		verificationAttempt(testServicePrincipal(), "actor-credential", ""))
	if err != nil {
		t.Fatalf("real Verify with the membership union: %v", err)
	}
	want := []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}
	if !reflect.DeepEqual(record.Actor.Roles, want) {
		t.Errorf("verified roles = %v, want the scoped union %v", record.Actor.Roles, want)
	}
	if _, err := s.Read(context.Background(), testScope(), "verifications", string(record.ID)); err != nil {
		t.Errorf("verification row was not committed: %v", err)
	}
}
