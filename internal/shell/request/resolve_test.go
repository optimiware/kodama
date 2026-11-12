package request_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func newResolveFixture(t *testing.T, assignmentText string, outputs ...string) (*testService, tenancy.Scope, kernel.Execution, request.Assignment) {
	t.Helper()
	scope := projectScope("tenant-1", "project-1")
	subject := literalSubject(map[string]string{"operation": "git.ref.update"})
	aggregate := runningRequestExecution(requestAction("act-0", "review", 0, 2*time.Hour, assignmentText, subject, outputs...))
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	assignment, err := request.ParseAssignment(assignmentText)
	requireNoError(t, err, "ParseAssignment")
	return fixture, scope, aggregate, assignment
}

func resolveCommand(scope tenancy.Scope, aggregate kernel.Execution, actionIndex int, actor authorize.Principal, decision request.Decision, response kernel.Outputs) request.ResolveCommand {
	return request.ResolveCommand{
		Actor:                    actor,
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[actionIndex].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[actionIndex].Version,
		Decision:                 decision,
		Response:                 response,
	}
}

func currentResolveCommand(t *testing.T, fixture *testService, scope tenancy.Scope, aggregate kernel.Execution, actionIndex int, actor authorize.Principal, decision request.Decision, response kernel.Outputs) request.ResolveCommand {
	t.Helper()
	current, err := fixture.store.Load(context.Background(), scope, aggregate.ID)
	requireNoError(t, err, "load current execution")
	return resolveCommand(scope, current, actionIndex, actor, decision, response)
}

func TestResolveBuildsAuthorizedUnit(t *testing.T) {
	fixture, scope, aggregate, assignment := newResolveFixture(t, "role/reviewer", "approved")
	fixture.clock.now = testNow.Add(time.Minute)
	// The resolve grant admits every human membership role; the assigned
	// reviewer is a separate in-commit assignment fact.
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleViewer)
	response := kernel.Outputs{"approved": "yes"}

	result, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, response))
	requireNoError(t, err, "Resolve")
	if result.Replayed {
		t.Fatal("fresh resolution reported a replay")
	}
	if result.Decision != request.DecisionPass {
		t.Fatalf("result decision = %q; want pass", result.Decision)
	}
	if fixture.resolutions.callCount() != 1 {
		t.Fatalf("resolve port calls = %d; want one", fixture.resolutions.callCount())
	}
	unit := fixture.resolutions.captured()[0]
	if unit.Owner != scope || unit.ExecutionID != aggregate.ID || unit.ActionID != aggregate.Actions[0].ID {
		t.Fatalf("resolve unit target = %+v/%q/%q", unit.Owner, unit.ExecutionID, unit.ActionID)
	}
	if unit.ExpectedExecutionVersion != aggregate.Version || unit.ExpectedActionVersion != aggregate.Actions[0].Version {
		t.Fatalf("resolve versions = %d/%d; want %d/%d",
			unit.ExpectedExecutionVersion, unit.ExpectedActionVersion, aggregate.Version, aggregate.Actions[0].Version)
	}
	if unit.Decision != request.DecisionPass {
		t.Fatalf("unit decision = %q; want pass", unit.Decision)
	}
	if unit.Response["approved"] != "yes" {
		t.Fatalf("unit response = %+v; want approved=yes", unit.Response)
	}
	if unit.Actor != actor.ID {
		t.Fatalf("unit actor = %q; want the verified %q", unit.Actor, actor.ID)
	}
	if unit.Assignment != assignment {
		t.Fatalf("unit assignment = %+v; want %+v", unit.Assignment, assignment)
	}
	if !unit.At.Equal(fixture.clock.now) {
		t.Fatalf("unit time = %v; want authoritative %v", unit.At, fixture.clock.now)
	}
	if unit.IdempotencyKey == "" {
		t.Fatal("resolve unit has no request-scoped retry identity")
	}
	if unit.Receipt.ID == "" || unit.Receipt.Key.Operation != "execution.request.resolve" {
		t.Fatalf("resolve receipt = %+v; want execution.request.resolve", unit.Receipt)
	}
	if unit.Receipt.Key.PrincipalID != actor.ID || unit.Receipt.Key.CallerKey != unit.IdempotencyKey {
		t.Fatalf("receipt key = %+v; want principal %q and caller key %q", unit.Receipt.Key, actor.ID, unit.IdempotencyKey)
	}
	if unit.Receipt.ContentHash == "" || !unit.Receipt.At.Equal(fixture.clock.now) {
		t.Fatalf("resolve receipt content/time = %+v", unit.Receipt)
	}
	if len(unit.Audit) != 1 || unit.Audit[0].Actor != actor.ID || unit.Audit[0].Operation != "execution.request.resolve" {
		t.Fatalf("resolve audit = %+v", unit.Audit)
	}
	requireEvent(t, unit.Events, event.TypeRequestResolved, scope, string(aggregate.ID), actor.ID, fixture.clock.now)
	requireAssignmentRoleRead(t, unit.Authority, tenancy.RoleReviewer)
	versionRead := requireAuthority(t, unit.Authority, "execution.request.resolve", authorize.CheckExecutionVersion)
	if versionRead.ExpectedVersion != aggregate.Version || versionRead.PrincipalID != actor.ID {
		t.Fatalf("execution version read = %+v; want version %d for %q", versionRead, aggregate.Version, actor.ID)
	}
	deadlineRead := requireAuthority(t, unit.Authority, "execution.request.resolve", authorize.CheckActionDeadline)
	if deadlineRead.TargetID != string(aggregate.Actions[0].ID) {
		t.Fatalf("action deadline read target = %q; want %q", deadlineRead.TargetID, aggregate.Actions[0].ID)
	}
	requirePostCommit(t, unit.PostCommit, event.PostPublishEvent)
	requirePostCommit(t, unit.PostCommit, event.PostRefreshCredential)

	stored, _ := fixture.store.Execution(aggregate.ID)
	if stored.State != kernel.ExecutionSucceeded {
		t.Fatalf("stored execution = %q; want succeeded", stored.State)
	}
	if stored.Actions[0].State != kernel.ActionSucceeded {
		t.Fatalf("stored action = %q; want succeeded", stored.Actions[0].State)
	}
	if stored.Actions[0].Request == nil || stored.Actions[0].Request.Answer == nil {
		t.Fatal("stored request has no recorded answer")
	}
	answer := stored.Actions[0].Request.Answer
	if answer.Decision != kernel.DecisionPass || answer.Actor != actor.ID || !answer.At.Equal(fixture.clock.now) {
		t.Fatalf("stored answer = %+v; want the authorized decision/actor/time", answer)
	}
	if answer.Outputs["approved"] != "yes" {
		t.Fatalf("stored answer outputs = %+v; want approved=yes", answer.Outputs)
	}
	if stored.ID != aggregate.ID || stored.WorkspaceID != aggregate.WorkspaceID {
		t.Fatalf("pass replaced the execution/workspace: %q/%q", stored.ID, stored.WorkspaceID)
	}
}

func TestResolveFailRetainsAnswerButDoesNotFeedDownstream(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	subject := literalSubject(map[string]string{"operation": "git.ref.update"})
	aggregate := runningRequestExecution(
		requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", subject, "approved"),
		containerAction("act-1", "deploy", 1, time.Hour, "result"),
	)
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	fixture.clock.now = testNow.Add(time.Minute)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	response := kernel.Outputs{"approved": "no"}

	result, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionFail, response))
	requireNoError(t, err, "Resolve fail")
	if result.Decision != request.DecisionFail {
		t.Fatalf("result decision = %q; want fail", result.Decision)
	}
	stored, _ := fixture.store.Execution(aggregate.ID)
	if stored.State != kernel.ExecutionFailed || stored.Actions[0].State != kernel.ActionFailed {
		t.Fatalf("failed decision states = %q/%q; want failed/failed", stored.State, stored.Actions[0].State)
	}
	if stored.Actions[1].State != kernel.ActionCancelled {
		t.Fatalf("pending successor state = %q; want cancelled", stored.Actions[1].State)
	}
	answer := stored.Actions[0].Request.Answer
	if answer == nil {
		t.Fatal("failed decision was not retained for inspection")
	}
	if answer.Decision != kernel.DecisionFail || answer.Outputs["approved"] != "no" {
		t.Fatalf("retained answer = %+v; want the rejected answer", answer)
	}
	if answer.DownstreamEligible() {
		t.Fatal("a failing decision was reported as downstream-eligible")
	}
	if len(stored.Actions[0].Outputs) != 0 {
		t.Fatalf("failed answer materialized downstream outputs: %+v", stored.Actions[0].Outputs)
	}
}

func TestResolveDeadlineBoundary(t *testing.T) {
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	cases := []struct {
		name    string
		offset  time.Duration
		expired bool
	}{
		{name: "strictly before deadline", offset: -time.Nanosecond},
		{name: "deadline equality expires", offset: 0, expired: true},
		{name: "after deadline expires", offset: time.Nanosecond, expired: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
			deadline := aggregate.Actions[0].Deadline
			fixture.clock.now = deadline.Add(testCase.offset)
			_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
			if testCase.expired {
				requireFaultClass(t, err, fault.DeadlineExpired)
				if fixture.resolutions.callCount() != 0 {
					t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
				}
				return
			}
			requireNoError(t, err, "Resolve before deadline")
			if fixture.resolutions.callCount() != 1 {
				t.Fatalf("resolve port calls = %d; want one", fixture.resolutions.callCount())
			}
		})
	}
}

func TestResolveWrongRespondentRole(t *testing.T) {
	t.Run("authority denies the assigned role even when the token lists it", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
		denyRole(fixture, tenancy.RoleReviewer)
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireFaultClass(t, err, fault.Forbidden)
		if fixture.resolutions.callCount() != 0 {
			t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
		}
	})

	t.Run("current role is read at the commit boundary, not from the token", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		// The token grants resolve through a different membership role; only the
		// current assignment read may authorize the reviewer answer.
		actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleViewer)
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireNoError(t, err, "Resolve with current role only")
		requireAssignmentRoleRead(t, fixture.resolutions.captured()[0].Authority, tenancy.RoleReviewer)
	})

	t.Run("a service cannot answer a human role assignment", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		actor := serviceActor("tenant-1", "project-1", "service-1", tenancy.RoleReviewer)
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireFaultClass(t, err, fault.Forbidden)
		if fixture.resolutions.callCount() != 0 {
			t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
		}
	})
}

func denyRole(fixture *testService, role tenancy.Role) {
	fixture.authority.read = func(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
		facts := allowedFacts(reads)
		for index, read := range reads {
			if read.Check != authorize.CheckMembership {
				continue
			}
			for _, candidate := range read.Required.HumanRoles {
				if candidate == role {
					facts[index].Allowed = false
				}
			}
		}
		return facts, nil
	}
}

func TestResolveServiceAssignmentRequiresExactEnabledService(t *testing.T) {
	t.Run("exact enabled service succeeds regardless of service roles", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "service/svc-1", "approved")
		// The service's grant role is irrelevant to the assignment; the exact
		// enabled service id is the assignment authority.
		actor := serviceActor("tenant-1", "project-1", "svc-1", tenancy.RoleViewer)
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireNoError(t, err, "Resolve service assignment")
		requireServiceAssignmentRead(t, fixture.resolutions.captured()[0].Authority, "svc-1")
	})

	t.Run("another service is refused", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "service/svc-1", "approved")
		actor := serviceActor("tenant-1", "project-1", "svc-2", tenancy.RoleReviewer)
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireFaultClass(t, err, fault.Forbidden)
		if fixture.resolutions.callCount() != 0 {
			t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
		}
	})

	t.Run("a human cannot answer a service assignment", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "service/svc-1", "approved")
		actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleAdmin)
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireFaultClass(t, err, fault.Forbidden)
		if fixture.resolutions.callCount() != 0 {
			t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
		}
	})

	t.Run("a disabled assigned service is refused", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "service/svc-1", "approved")
		actor := serviceActor("tenant-1", "project-1", "svc-1", tenancy.RoleViewer)
		fixture.authority.read = func(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
			facts := allowedFacts(reads)
			for index, read := range reads {
				if read.Check == authorize.CheckServiceEnabled && read.TargetID == "svc-1" {
					facts[index].Allowed = false
				}
			}
			return facts, nil
		}
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireFaultClass(t, err, fault.Forbidden)
		if fixture.resolutions.callCount() != 0 {
			t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
		}
	})
}

func TestResolveMissingPrincipal(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, authorize.Principal{}, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
	requireFaultClass(t, err, fault.Unauthenticated)
	if fixture.resolutions.callCount() != 0 {
		t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
	}
}

func TestResolveRefusesStaleVersion(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	command := resolveCommand(scope, aggregate, 0, humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer), request.DecisionPass, kernel.Outputs{"approved": "yes"})
	command.ExpectedActionVersion = aggregate.Actions[0].Version + 1
	_, err := fixture.service.Resolve(context.Background(), command)
	requireFaultClass(t, err, fault.VersionConflict)
	if fixture.resolutions.callCount() != 0 {
		t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
	}
}

func TestResolveRefusesWrongActionKind(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer), request.DecisionPass, kernel.Outputs{"candidate": "v"}))
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.resolutions.callCount() != 0 {
		t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
	}
}

func TestResolveValidatesDeclaredResponseSchema(t *testing.T) {
	t.Run("undeclared response key refused", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer), request.DecisionPass, kernel.Outputs{"other": "x"}))
		requireFaultClass(t, err, fault.InvalidInput)
		if fixture.resolutions.callCount() != 0 {
			t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
		}
	})

	t.Run("declared response key accepted", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer), request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireNoError(t, err, "Resolve declared response")
	})
}

func TestResolveDecisionAndAuthorityCannotComeFromResponseStrings(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	subject := literalSubject(map[string]string{"operation": "git.ref.update"})
	aggregate := runningRequestExecution(
		requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", subject, "decision", "actor", "authority", "subject"),
	)
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	fixture.clock.now = testNow.Add(time.Minute)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	response := kernel.Outputs{
		"decision":  "pass",
		"actor":     "attacker",
		"authority": "admin",
		"subject":   "forged",
	}
	_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionFail, response))
	requireNoError(t, err, "Resolve")
	unit := fixture.resolutions.captured()[0]
	if unit.Actor != actor.ID {
		t.Fatalf("unit actor = %q; want the verified %q, not the response string", unit.Actor, actor.ID)
	}
	if unit.Decision != request.DecisionFail {
		t.Fatalf("unit decision = %q; want the authorized fail, not the response string", unit.Decision)
	}
	if unit.Assignment.Kind != request.AssignmentRole || unit.Assignment.Role != tenancy.RoleReviewer {
		t.Fatalf("unit assignment = %+v; want the admitted role/reviewer", unit.Assignment)
	}
	stored, _ := fixture.store.Execution(aggregate.ID)
	answer := stored.Actions[0].Request.Answer
	if answer == nil || answer.Actor != actor.ID || answer.Decision != kernel.DecisionFail {
		t.Fatalf("stored answer = %+v; want verified actor and authorized decision", answer)
	}
	if answer.Outputs["actor"] != "attacker" {
		t.Fatalf("response strings were not retained as plain outputs: %+v", answer.Outputs)
	}
	fields := stored.Actions[0].Request.Subject.Fields()
	if fields["operation"].Literal != "git.ref.update" {
		t.Fatalf("response string overwrote the immutable subject: %+v", fields)
	}
}

func TestResolveRetryReturnsRecordedResultAfterCurrentAuthorization(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	first, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
	requireNoError(t, err, "first Resolve")
	authorityCalls := fixture.authority.callCount()

	retry := currentResolveCommand(t, fixture, scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})
	second, err := fixture.service.Resolve(context.Background(), retry)
	requireNoError(t, err, "retried Resolve")
	if !second.Replayed {
		t.Fatal("identical request retry did not report a replay")
	}
	if second.ExecutionVersion != first.ExecutionVersion || second.Decision != first.Decision {
		t.Fatalf("retry result = %+v; want the recorded %+v", second, first)
	}
	if fixture.authority.callCount() <= authorityCalls {
		t.Fatal("retry did not re-read current assignment authority")
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits = %d; want one (retry is not a new effect)", fixture.store.commitCount())
	}
}

func TestResolveRetryChangedContentRefused(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	if _, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	retry := currentResolveCommand(t, fixture, scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "no"})
	_, err := fixture.service.Resolve(context.Background(), retry)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits = %d; want one", fixture.store.commitCount())
	}
	stored, _ := fixture.store.Execution(aggregate.ID)
	if stored.Actions[0].Request.Answer.Outputs["approved"] != "yes" {
		t.Fatalf("changed retry rewrote the recorded answer: %+v", stored.Actions[0].Request.Answer)
	}
}

func TestResolveRetryAfterAuthorityRevokedRefused(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	if _, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	denyRole(fixture, tenancy.RoleReviewer)
	retry := currentResolveCommand(t, fixture, scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})
	_, err := fixture.service.Resolve(context.Background(), retry)
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits = %d; want one", fixture.store.commitCount())
	}
}

func TestResolveCancelRaceCancelFirstWins(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	blocked := make(chan struct{})
	release := make(chan struct{})
	fixture.resolutions.setBeforeApply(func() {
		close(blocked)
		<-release
	})
	result := make(chan error, 1)
	go func() {
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		result <- err
	}()
	<-blocked

	current, err := fixture.store.Load(context.Background(), scope, aggregate.ID)
	requireNoError(t, err, "load during cancel race")
	_, commit, err := flow.Reduce(current, flow.CancelFact{
		ExecutionID: aggregate.ID,
		Reason:      flow.CancelRequested,
		At:          testNow.Add(2 * time.Minute),
	})
	requireNoError(t, err, "cancel reduce")
	if commit == nil {
		t.Fatal("cancel produced no commit")
	}
	if _, err := fixture.store.Commit(context.Background(), *commit); err != nil {
		t.Fatalf("cancel commit: %v", err)
	}
	close(release)

	requireFaultClass(t, <-result, fault.VersionConflict)
	stored, _ := fixture.store.Execution(aggregate.ID)
	if stored.State != kernel.ExecutionCancelled {
		t.Fatalf("winner state = %q; want cancelled", stored.State)
	}
	if stored.Actions[0].Request != nil && stored.Actions[0].Request.Answer != nil {
		t.Fatalf("losing resolve recorded an answer after cancellation: %+v", stored.Actions[0].Request.Answer)
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits = %d; want only the cancellation", fixture.store.commitCount())
	}
}

func TestResolveFirstWinsAndCancelIsRefused(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	if _, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	current, _ := fixture.store.Load(context.Background(), scope, aggregate.ID)
	_, commit, err := flow.Reduce(current, flow.CancelFact{
		ExecutionID: aggregate.ID,
		Reason:      flow.CancelRequested,
		At:          testNow.Add(2 * time.Minute),
	})
	if err == nil && commit != nil {
		t.Fatal("cancellation replaced a settled successful resolution")
	}
	requireFaultClassIn(t, err, fault.ChangedIdempotencyContent, fault.InvalidInput)
	stored, _ := fixture.store.Execution(aggregate.ID)
	if stored.State != kernel.ExecutionSucceeded {
		t.Fatalf("state after losing cancel = %q; want succeeded", stored.State)
	}
}

func TestResolvePassKeepsExecutionAndWorkspaceForPlannedWait(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	subject := literalSubject(map[string]string{"operation": "git.ref.update"})
	aggregate := runningRequestExecution(
		requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", subject, "approved"),
		containerAction("act-1", "deploy", 1, time.Hour, "result"),
	)
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	if _, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	afterPass, _ := fixture.store.Execution(aggregate.ID)
	if afterPass.ID != aggregate.ID || afterPass.WorkspaceID != aggregate.WorkspaceID {
		t.Fatalf("pass changed execution/workspace: %q/%q", afterPass.ID, afterPass.WorkspaceID)
	}
	if afterPass.State != kernel.ExecutionRunning || afterPass.Actions[1].State != kernel.ActionPending {
		t.Fatalf("pass states = %q/%q; want running/pending", afterPass.State, afterPass.Actions[1].State)
	}
	_, commit, err := flow.Reduce(afterPass, flow.ActivateFact{
		ExecutionID: afterPass.ID,
		ActionID:    afterPass.Actions[1].ID,
		At:          testNow.Add(2 * time.Minute),
	})
	requireNoError(t, err, "activate next action")
	if commit == nil {
		t.Fatal("next activation produced no commit")
	}
	next := *commit
	if next.ExecutionID != aggregate.ID {
		t.Fatalf("planned wait activated a different execution: %q", next.ExecutionID)
	}
	if next.NextCurrentAction == nil || *next.NextCurrentAction != 1 {
		t.Fatalf("next current action = %v; want 1", next.NextCurrentAction)
	}
}

func TestResolveRefusesCrossScopeExecution(t *testing.T) {
	fixture := newTestService(t)
	subject := literalSubject(map[string]string{"operation": "op"})
	aggregate := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, "role/reviewer", subject))
	fixture.store.Seed(projectScope("tenant-1", "project-1"), aggregate)
	_, err := fixture.service.Resolve(context.Background(), resolveCommand(
		projectScope("tenant-2", "project-9"),
		aggregate,
		0,
		humanActor("tenant-2", "project-9", "principal-9", tenancy.RoleReviewer),
		request.DecisionPass,
		kernel.Outputs{},
	))
	requireFaultClass(t, err, fault.NotFound)
	if fixture.resolutions.callCount() != 0 {
		t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
	}
}

func TestResolveUnknownExecutionIsNotTreatedAsTerminal(t *testing.T) {
	fixture := newTestService(t)
	aggregate := baseExecution(requestAction("act-0", "review", 0, time.Hour, "role/reviewer", literalSubject(map[string]string{"operation": "op"})))
	_, err := fixture.service.Resolve(context.Background(), resolveCommand(
		projectScope("tenant-1", "project-1"),
		aggregate,
		0,
		humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer),
		request.DecisionPass,
		kernel.Outputs{},
	))
	requireFaultClass(t, err, fault.NotFound)
	if fixture.resolutions.callCount() != 0 {
		t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
	}
}

// keep definition used by the fixture type set honest.
var _ = definition.Subject{}
var _ = id.ExecutionID("")

func TestResolveRetryWithOriginalVersionsReplays(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	original := resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})
	first, err := fixture.service.Resolve(context.Background(), original)
	requireNoError(t, err, "first Resolve")
	authorityCalls := fixture.authority.callCount()

	// A lost response means the client resends the identical command with the
	// versions it originally observed; settlement has already advanced both.
	second, err := fixture.service.Resolve(context.Background(), original)
	requireNoError(t, err, "retried Resolve")
	if !second.Replayed {
		t.Fatal("identical request retry did not report a replay")
	}
	if second.ExecutionVersion != first.ExecutionVersion || second.ActionVersion != first.ActionVersion || second.Decision != first.Decision {
		t.Fatalf("retry result = %+v; want the recorded %+v", second, first)
	}
	if fixture.authority.callCount() <= authorityCalls {
		t.Fatal("retry did not re-read current assignment authority")
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits = %d; want one (retry is not a new effect)", fixture.store.commitCount())
	}
	units := fixture.resolutions.captured()
	if len(units) != 2 {
		t.Fatalf("resolve port calls = %d; want two", len(units))
	}
	if units[1].ExpectedExecutionVersion != first.ExecutionVersion || units[1].ExpectedActionVersion != first.ActionVersion {
		t.Fatalf("retry unit versions = %d/%d; want the observed settled %d/%d",
			units[1].ExpectedExecutionVersion, units[1].ExpectedActionVersion,
			first.ExecutionVersion, first.ActionVersion)
	}
	requireAssignmentRoleRead(t, units[1].Authority, tenancy.RoleReviewer)
	if _, stale := findAuthority(units[1].Authority, authorize.CheckExecutionVersion); stale {
		t.Fatalf("retry authority carries a historical execution version read: %v", units[1].Authority)
	}
}

func TestResolveRetryOriginalVersionsChangedContentRefused(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	original := resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})
	if _, err := fixture.service.Resolve(context.Background(), original); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	changed := original
	changed.Response = kernel.Outputs{"approved": "no"}
	_, err := fixture.service.Resolve(context.Background(), changed)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits = %d; want one", fixture.store.commitCount())
	}
}

// TestResolveAcceptsOmittedEnvelopeVersionAndDerivesObservedVersions pins the
// action-only cutover: an omitted envelope execution version is accepted, the
// in-commit unit is guarded by the versions read from observed state, and the
// result advances from that observed execution version. The action version
// stays a required explicit expectation.
func TestResolveAcceptsOmittedEnvelopeVersionAndDerivesObservedVersions(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	command := resolveCommand(scope, aggregate, 0,
		humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer),
		request.DecisionPass, kernel.Outputs{"approved": "yes"})
	command.ExpectedExecutionVersion = 0
	result, err := fixture.service.Resolve(context.Background(), command)
	requireNoError(t, err, "Resolve")
	if result.ExecutionVersion != aggregate.Version+1 || result.ActionVersion != aggregate.Actions[0].Version+1 {
		t.Fatalf("resolve result = %d/%d; want %d/%d",
			result.ExecutionVersion, result.ActionVersion,
			aggregate.Version+1, aggregate.Actions[0].Version+1)
	}
	unit := fixture.resolutions.captured()[0]
	if unit.ExpectedExecutionVersion != aggregate.Version {
		t.Fatalf("unit execution version = %d; want the observed %d", unit.ExpectedExecutionVersion, aggregate.Version)
	}
	if unit.ExpectedActionVersion != aggregate.Actions[0].Version {
		t.Fatalf("unit action version = %d; want the observed %d", unit.ExpectedActionVersion, aggregate.Actions[0].Version)
	}
	versionRead := requireAuthority(t, unit.Authority, "execution.request.resolve", authorize.CheckExecutionVersion)
	if versionRead.ExpectedVersion != aggregate.Version {
		t.Fatalf("authority execution version pin = %d; want the observed %d", versionRead.ExpectedVersion, aggregate.Version)
	}
}

func TestResolveRefusesStaleExecutionVersionForUnansweredAction(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	command := resolveCommand(scope, aggregate, 0, humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer), request.DecisionPass, kernel.Outputs{"approved": "yes"})
	command.ExpectedExecutionVersion = aggregate.Version + 1
	_, err := fixture.service.Resolve(context.Background(), command)
	requireFaultClass(t, err, fault.VersionConflict)
	if fixture.resolutions.callCount() != 0 {
		t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
	}
}

// TestResolveRefusesAtTheDeadline pins the service-level transaction-deadline
// guard: an answer at the deadline (or later) is deadline_expired and commits
// nothing, independent of the store's own guard.
func TestResolveRefusesAtTheDeadline(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	fixture.clock.now = aggregate.Actions[0].Deadline
	_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
	requireFaultClass(t, err, fault.DeadlineExpired)
	if strings.Contains(err.Error(), "flow:") {
		t.Fatalf("deadline refusal came from the Kernel reducer, not the service guard: %v", err)
	}
	if !strings.Contains(err.Error(), "is at or past its deadline") {
		t.Fatalf("deadline refusal = %v; want the service deadline guard", err)
	}
	if fixture.resolutions.callCount() != 0 {
		t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
	}
	if fixture.store.commitCount() != 0 {
		t.Fatalf("commits = %d; want zero", fixture.store.commitCount())
	}
}

// TestResolveRefusesNonRunningExecutionOrCurrentAction pins the in-commit
// liveness guards: only a running execution whose current action is this
// running request action may be settled. Each case is refused as invalid_input
// before any resolve unit is built.
func TestResolveRefusesNonRunningExecutionOrCurrentAction(t *testing.T) {
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	newAggregate := func(t *testing.T) (tenancy.Scope, kernel.Execution) {
		t.Helper()
		scope := projectScope("tenant-1", "project-1")
		subject := literalSubject(map[string]string{"operation": "git.ref.update"})
		return scope, runningRequestExecution(
			requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", subject, "approved"),
			requestAction("act-1", "second", 1, 2*time.Hour, "role/reviewer", subject, "approved"),
		)
	}
	cases := []struct {
		name   string
		mutate func(*kernel.Execution)
	}{
		{"execution is not running", func(execution *kernel.Execution) { execution.State = kernel.ExecutionSucceeded }},
		{"the current action is another action", func(execution *kernel.Execution) { execution.CurrentAction = 1 }},
		{"the action is not running", func(execution *kernel.Execution) { execution.Actions[0].State = kernel.ActionPending }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scope, aggregate := newAggregate(t)
			testCase.mutate(&aggregate)
			fixture := newTestService(t)
			fixture.store.Seed(scope, aggregate)
			_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
			requireFaultClass(t, err, fault.InvalidInput)
			if fixture.resolutions.callCount() != 0 {
				t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
			}
		})
	}
}

func TestResolveConfirmsAuthorityFactIdentityAndVersion(t *testing.T) {
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)

	t.Run("stale observed execution version is version_conflict", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		fixture.authority.read = func(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
			facts := allowedFacts(reads)
			for index, read := range reads {
				if read.Check == authorize.CheckExecutionVersion {
					facts[index].Version = read.ExpectedVersion + 1
				}
			}
			return facts, nil
		}
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireFaultClass(t, err, fault.VersionConflict)
		if fixture.resolutions.callCount() != 0 {
			t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
		}
	})

	t.Run("mismatched fact identity is internal_failure", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		fixture.authority.read = func(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
			facts := allowedFacts(reads)
			facts[0].PrincipalID = "other-principal"
			return facts, nil
		}
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireFaultClass(t, err, fault.InternalFailure)
		if fixture.resolutions.callCount() != 0 {
			t.Fatalf("resolve port calls = %d; want zero", fixture.resolutions.callCount())
		}
	})
}
