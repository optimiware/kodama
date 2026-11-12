package task_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func askCommand(actor authorize.Principal, expectedLifecycle uint64, question string, assignment request.Assignment) task.AskCommand {
	return task.AskCommand{
		Actor:                    actor,
		Scope:                    actor.Scope,
		TaskID:                   taskID,
		ExpectedLifecycleVersion: expectedLifecycle,
		Question:                 question,
		Assignment:               assignment,
		References:               []string{"git:abc123"},
		CallerKey:                "caller-ask",
		Content:                  receiptContent(`{"question":"` + question + `"}`),
	}
}

func replyCommand(actor authorize.Principal, expectedLifecycle uint64, questionID task.QuestionID, questionVersion uint64, answer string) task.ReplyCommand {
	return task.ReplyCommand{
		Actor:                    actor,
		Scope:                    actor.Scope,
		TaskID:                   taskID,
		ExpectedLifecycleVersion: expectedLifecycle,
		QuestionID:               questionID,
		ExpectedQuestionVersion:  questionVersion,
		Answer:                   answer,
		CallerKey:                "caller-reply",
		Content:                  receiptContent(`{"answer":"` + answer + `"}`),
	}
}

func cancelCommand(actor authorize.Principal, expectedLifecycle uint64) task.CancelCommand {
	return task.CancelCommand{
		Actor:                    actor,
		Scope:                    actor.Scope,
		TaskID:                   taskID,
		ExpectedLifecycleVersion: expectedLifecycle,
		CallerKey:                "caller-cancel",
		Content:                  receiptContent(`{"cancel":"task"}`),
	}
}

// TestAskCommitsWaitingHandoff proves ask is one indivisible command: question,
// waiting state, cancellation of the nonterminal execution at its version,
// credential revocation, event and publish, with no request action.
func TestAskCommitsWaitingHandoff(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	value := baseTask()
	env.reader.put(value)
	env.executions.put(openExecution(2))

	assignment := roleAssignment(t, tenancy.RoleReviewer)
	command := askCommand(actor, 4, "Which candidate should land?", assignment)
	result, err := env.service.Ask(t.Context(), command)
	requireNoError(t, err, "Ask")

	if calls := env.executions.calledIDs(); len(calls) != 1 || calls[0] != executionOne {
		t.Fatalf("execution reads = %v; want the current execution %q", calls, executionOne)
	}
	unit := env.ports.lastAsk(t)
	if unit.Owner != actor.Scope || unit.TaskID != taskID {
		t.Fatalf("ask identity = %+v/%q", unit.Owner, unit.TaskID)
	}
	if unit.ExpectedLifecycleVersion != 4 || unit.ExpectedExecutionVersion != 2 {
		t.Fatalf("expected versions = %d/%d; want lifecycle 4 and execution 2", unit.ExpectedLifecycleVersion, unit.ExpectedExecutionVersion)
	}
	if unit.QuestionID == "" {
		t.Fatal("ask has no question id")
	}
	if unit.Question != "Which candidate should land?" || unit.Assignment != assignment {
		t.Fatalf("question = %q/%+v", unit.Question, unit.Assignment)
	}
	if len(unit.References) != 1 || unit.References[0] != "git:abc123" {
		t.Fatalf("references = %+v", unit.References)
	}
	if unit.CredentialUpdate == nil || unit.CredentialUpdate.Present() {
		t.Fatalf("credential update = %+v; want a present revocation (absent credential)", unit.CredentialUpdate)
	}

	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.ask", command.CallerKey, command.Content, testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.ask", testNow)
	requireEvent(t, unit.Events, event.TypeTaskQuestion, actor.Scope, string(taskID), 5, testNow)
	requireNoEvent(t, unit.Events, event.TypeActionRequested)
	requireNoEvent(t, unit.Events, event.TypeRequestActivated)
	requireNoEvent(t, unit.Events, event.TypeExecutionQueued)
	requirePublish(t, unit.PostCommit)
	requireNoDispatch(t, unit.PostCommit)

	requireAuthority(t, unit.Authority, "task.ask", authorize.CheckMembership)
	taskVersion := requireAuthority(t, unit.Authority, "task.ask", authorize.CheckTaskVersion)
	if taskVersion.ExpectedVersion != 4 {
		t.Fatalf("task_version read = %d; want 4", taskVersion.ExpectedVersion)
	}
	executionVersion := requireAuthority(t, unit.Authority, "task.ask", authorize.CheckExecutionVersion)
	if executionVersion.TargetID != string(executionOne) || executionVersion.ExpectedVersion != 2 {
		t.Fatalf("execution_version read = %+v; want %q@2", executionVersion, executionOne)
	}
	if result.LifecycleVersion != 5 || result.QuestionVersion != 1 || result.Replayed {
		t.Fatalf("ask result = %+v; want lifecycle 5 question 1 fresh", result)
	}
	if env.ports.totalCalls() != 1 {
		t.Fatalf("commit port calls = %d; ask must be one unit", env.ports.totalCalls())
	}
}

// TestAskOnWaitingRetryReachesPort proves a lost-response retry on a waiting
// task still reaches the receipt-first port, an identical retry replays and a
// different question is refused as a conflict.
func TestAskOnWaitingRetryReachesPort(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	value := waitingTask(t)
	value.LifecycleVersion = 5
	env.reader.put(value)

	var storedHash string
	applied := 0
	env.ports.setAsk(func(unit task.AskUnit) (task.AskResult, error) {
		if storedHash == "" {
			storedHash = unit.Receipt.ContentHash
			applied++
			return task.AskResult{}, fault.New(fault.DependencyUnavailable, "response lost after commit")
		}
		if storedHash != unit.Receipt.ContentHash {
			return task.AskResult{}, fault.New(fault.VersionConflict, "a different question while waiting")
		}
		return task.AskResult{LifecycleVersion: 6, QuestionVersion: 2, Replayed: true}, nil
	})

	assignment := roleAssignment(t, tenancy.RoleReviewer)
	if _, err := env.service.Ask(t.Context(), askCommand(actor, 5, "First question", assignment)); err == nil {
		t.Fatal("first ask reported success despite the lost response")
	} else {
		requireFaultClass(t, err, fault.DependencyUnavailable)
	}
	result, err := env.service.Ask(t.Context(), askCommand(actor, 5, "First question", assignment))
	requireNoError(t, err, "Ask retry")
	if !result.Replayed || result.LifecycleVersion != 6 {
		t.Fatalf("retry result = %+v; want the original replay", result)
	}
	_, err = env.service.Ask(t.Context(), askCommand(actor, 5, "Different question", assignment))
	requireFaultClass(t, err, fault.VersionConflict)
	if applied != 1 {
		t.Fatalf("applied asks = %d; want 1", applied)
	}
	if len(env.ports.asks) != 3 {
		t.Fatalf("ask port calls = %d; want every attempt to reach the port", len(env.ports.asks))
	}
}

// TestAskRefusesTerminalStates proves done and cancelled tasks are refused
// immediately as version conflicts before reading executions or reaching the port.
func TestAskRefusesTerminalStates(t *testing.T) {
	cases := map[string]task.Task{
		"done task":      doneTask(),
		"cancelled task": cancelledTask(),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
			env.reader.put(value)
			_, err := env.service.Ask(t.Context(), askCommand(actor, value.LifecycleVersion, "Question?", roleAssignment(t, tenancy.RoleReviewer)))
			requireFaultClass(t, err, fault.VersionConflict)
			if env.executions.callCount() != 0 {
				t.Fatalf("execution reader calls = %d; want none for terminal task", env.executions.callCount())
			}
			requireNoPortCalls(t, env.ports)
		})
	}
}

// TestReplyQueuesOneFreshExecutionWithUpdatedConversation proves reply is one
// indivisible command: answer, reopen, exactly one fresh execution with the
// updated conversation snapshot, initiator start authority and one receipt.
func TestReplyQueuesOneFreshExecutionWithUpdatedConversation(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
	value := waitingTask(t)
	value.Messages = []task.Message{{
		Author: engineerPrincipal,
		At:     testNow.Add(-time.Hour),
		Body:   "Initial investigation",
	}}
	env.reader.put(value)
	env.executions.put(task.ExecutionView{
		ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: confirmedStop(),
	})

	command := replyCommand(actor, 4, questionID, 1, "Use candidate B")
	result, err := env.service.Reply(t.Context(), command)
	requireNoError(t, err, "Reply")

	if env.admission.callCount() != 1 {
		t.Fatalf("admission calls = %d; want exactly one", env.admission.callCount())
	}
	admitted := env.admission.requestsSnapshot()[0]
	if admitted.Definition.ID != pipelineID {
		t.Fatalf("admitted pipeline = %q; want %q", admitted.Definition.ID, pipelineID)
	}
	if admitted.Inputs["repository"] != "example/repo" {
		t.Fatalf("admitted inputs = %+v", admitted.Inputs)
	}
	if admitted.Task.TaskID != taskID {
		t.Fatalf("admitted task id = %q", admitted.Task.TaskID)
	}
	conversation := string(admitted.Task.Conversation)
	if !strings.Contains(conversation, "Use candidate B") {
		t.Fatalf("admitted conversation %q does not carry the answer", conversation)
	}
	if !strings.Contains(conversation, "Initial investigation") {
		t.Fatalf("admitted conversation %q does not retain prior messages", conversation)
	}

	if len(env.ports.replies) != 1 {
		t.Fatalf("reply port calls = %d; want exactly one", len(env.ports.replies))
	}
	unit := env.ports.lastReply(t)
	if unit.Owner != actor.Scope || unit.TaskID != taskID {
		t.Fatalf("reply identity = %+v/%q", unit.Owner, unit.TaskID)
	}
	if unit.ExpectedLifecycleVersion != 4 || unit.ExpectedQuestionVersion != 1 {
		t.Fatalf("expected versions = %d/%d; want lifecycle 4 question 1", unit.ExpectedLifecycleVersion, unit.ExpectedQuestionVersion)
	}
	if unit.QuestionID != questionID || unit.Answer != "Use candidate B" {
		t.Fatalf("reply = %q/%q", unit.QuestionID, unit.Answer)
	}
	if unit.PipelineID != pipelineID || unit.ExpectedPipelineVersion != 7 {
		t.Fatalf("reply pipeline = %q@%d; want %q@7", unit.PipelineID, unit.ExpectedPipelineVersion, pipelineID)
	}
	if unit.Snapshot.TaskID != taskID || !strings.Contains(string(unit.Snapshot.ConversationBytes()), "Use candidate B") {
		t.Fatalf("reply snapshot = task %q conversation %q", unit.Snapshot.TaskID, unit.Snapshot.ConversationBytes())
	}
	if len(unit.Actions) != 1 || unit.Actions[0].Name != "run" {
		t.Fatalf("reply actions = %+v", unit.Actions)
	}
	if unit.Initiator != engineerPrincipal || unit.Initiator == actor.ID {
		t.Fatalf("reply initiator = %q; want the task initiator %q", unit.Initiator, engineerPrincipal)
	}
	if unit.ExecutionPrincipal == "" || unit.NewExecutionID == "" {
		t.Fatalf("reply principals/execution = %q/%q", unit.ExecutionPrincipal, unit.NewExecutionID)
	}
	if !unit.At.Equal(testNow) {
		t.Fatalf("reply time = %v; want %v", unit.At, testNow)
	}
	if !env.principals.called(engineerPrincipal) || !env.principals.called(reviewerPrincipal) {
		t.Fatalf("principal reads = %v; want both the initiator and the respondent", env.principals.calledIDs())
	}

	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.reply", command.CallerKey, command.Content, testNow)
	if unit.Receipt.ExecutionID != result.ExecutionID {
		t.Fatalf("receipt execution = %q; want %q", unit.Receipt.ExecutionID, result.ExecutionID)
	}
	if unit.Receipt.SnapshotHash == nil || *unit.Receipt.SnapshotHash != unit.Snapshot.YAMLSHA256 {
		t.Fatalf("receipt snapshot hash = %v; want the admitted snapshot hash", unit.Receipt.SnapshotHash)
	}
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.reply", testNow)
	requireEvent(t, unit.Events, event.TypeTaskReply, actor.Scope, string(taskID), 5, testNow)
	requireEvent(t, unit.Events, event.TypeExecutionQueued, actor.Scope, string(result.ExecutionID), 1, testNow)
	requirePublish(t, unit.PostCommit)
	requireDispatch(t, unit.PostCommit)

	requireAuthority(t, unit.Authority, "task.reply", authorize.CheckMembership)
	taskVersion := requireAuthority(t, unit.Authority, "task.reply", authorize.CheckTaskVersion)
	if taskVersion.ExpectedVersion != 4 {
		t.Fatalf("task_version read = %d; want 4", taskVersion.ExpectedVersion)
	}
	executionVersion := requireAuthority(t, unit.Authority, "task.reply", authorize.CheckExecutionVersion)
	if executionVersion.TargetID != string(executionOne) || executionVersion.ExpectedVersion != 2 {
		t.Fatalf("execution_version read = %+v; want %q@2", executionVersion, executionOne)
	}
	pipelineVersion := requireAuthority(t, unit.Authority, "task.reply", authorize.CheckPipelineVersion)
	if pipelineVersion.TargetID != string(pipelineID) || pipelineVersion.ExpectedVersion != 7 {
		t.Fatalf("pipeline_version read = %+v; want %q@7", pipelineVersion, pipelineID)
	}
	startAuthority := requireStartAuthority(t, unit.Authority, engineerPrincipal)
	if startAuthority.PrincipalID != engineerPrincipal {
		t.Fatalf("initiator start authority = %+v; want %q", startAuthority, engineerPrincipal)
	}
	// A reply always creates a fresh execution, so the conditional operator
	// retry grant can never apply to the initiator. The carried read must not
	// list the operator role: an initiator demoted from engineer to operator-only
	// between the pre-admission check and the commit must be refused by the
	// in-transaction re-evaluation.
	for _, role := range startAuthority.Required.HumanRoles {
		if role == tenancy.RoleOperator {
			t.Fatalf("reply initiator start read requires the conditional operator role: %+v", startAuthority.Required)
		}
	}
	if !reflect.DeepEqual(startAuthority.Required.HumanRoles, []tenancy.Role{tenancy.RoleEngineer}) {
		t.Fatalf("reply initiator start read human roles = %v; want engineer only", startAuthority.Required.HumanRoles)
	}
	if !reflect.DeepEqual(startAuthority.Required.ServiceRoles, []tenancy.Role{tenancy.RoleEngineer}) {
		t.Fatalf("reply initiator start read service roles = %v; want the registry engineer grant unchanged", startAuthority.Required.ServiceRoles)
	}
	if result.ExecutionID != unit.NewExecutionID || result.LifecycleVersion != 5 || result.QuestionVersion != 2 || result.Replayed {
		t.Fatalf("reply result = %+v", result)
	}
	if env.ports.totalCalls() != 1 {
		t.Fatalf("commit port calls = %d; reply must be one unit", env.ports.totalCalls())
	}
}

// TestReplyDispatchWaitsForPredecessorStop proves reply and stop confirmation
// may arrive in either order; the queued execution is the durable dispatch
// record and dispatch waits for confirmed stop.
func TestReplyDispatchWaitsForPredecessorStop(t *testing.T) {
	cases := map[string]struct {
		stop         kernel.StopEvidence
		wantDispatch bool
	}{
		"reply before stop confirmation": {stop: pendingStop(), wantDispatch: false},
		"stop confirmed before reply":    {stop: confirmedStop(), wantDispatch: true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
			env.reader.put(waitingTask(t))
			env.executions.put(task.ExecutionView{
				ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: testCase.stop,
			})

			_, err := env.service.Reply(t.Context(), replyCommand(actor, 4, questionID, 1, "Answer"))
			requireNoError(t, err, "Reply")
			unit := env.ports.lastReply(t)
			requirePublish(t, unit.PostCommit)
			if testCase.wantDispatch {
				requireDispatch(t, unit.PostCommit)
			} else {
				requireNoDispatch(t, unit.PostCommit)
			}
		})
	}
}

// TestReplyRollsBackOnAdmissionFailure proves a failed admission commits no
// answer, no state change and no reply unit.
func TestReplyRollsBackOnAdmissionFailure(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
	env.reader.put(waitingTask(t))
	env.executions.put(task.ExecutionView{
		ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: confirmedStop(),
	})
	env.admission.admit = func(pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
		return pipeline.AdmissionResult{}, fault.New(fault.DependencyUnavailable, "admission unavailable")
	}

	_, err := env.service.Reply(t.Context(), replyCommand(actor, 4, questionID, 1, "Answer"))
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if env.admission.callCount() != 1 {
		t.Fatalf("admission calls = %d; want one failed attempt", env.admission.callCount())
	}
	requireNoPortCalls(t, env.ports)
}

// TestReplyRollsBackOnRevokedInitiator proves the initiator's current start
// authority is re-read before admission; a revoked initiator commits nothing.
func TestReplyRollsBackOnRevokedInitiator(t *testing.T) {
	cases := map[string]func(*testEnv){
		"disabled initiator": func(env *testEnv) {
			env.principals.put(identity.Principal{
				ID: engineerPrincipal, Kind: identity.KindHuman, Scope: projectScope(),
				Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: false, Version: 2,
			})
		},
		"initiator lost start role": func(env *testEnv) {
			env.principals.put(identity.Principal{
				ID: engineerPrincipal, Kind: identity.KindHuman, Scope: projectScope(),
				Roles: []tenancy.Role{tenancy.RoleViewer}, Enabled: true, Version: 2,
			})
		},
		// The operator role holds task.start only conditionally. The reply
		// initiator check is a pre-admission read that cannot evaluate the
		// failed-task condition, so an operator-only initiator must not pass
		// it.
		"initiator holds only the conditional operator grant": func(env *testEnv) {
			env.principals.put(identity.Principal{
				ID: engineerPrincipal, Kind: identity.KindHuman, Scope: projectScope(),
				Roles: []tenancy.Role{tenancy.RoleOperator}, Enabled: true, Version: 2,
			})
		},
		"start authority read denied": func(env *testEnv) {
			env.authority.read = func(reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
				return allowFacts(reads, func(read authorize.AuthorityRead) bool {
					return read.Operation == "task.start"
				}), nil
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
			env.reader.put(waitingTask(t))
			env.executions.put(task.ExecutionView{
				ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: confirmedStop(),
			})
			mutate(env)

			_, err := env.service.Reply(t.Context(), replyCommand(actor, 4, questionID, 1, "Answer"))
			requireFaultClass(t, err, fault.Forbidden)
			if env.admission.callCount() != 0 {
				t.Fatalf("admission calls = %d; a revoked initiator must not admit", env.admission.callCount())
			}
			requireNoPortCalls(t, env.ports)
		})
	}
}

// TestReplyEnforcesAssignment proves role assignments require a current human
// role holder and service assignments require the exact service principal.
func TestReplyEnforcesAssignment(t *testing.T) {
	t.Run("role assignment", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.put(waitingTask(t))
		env.executions.put(task.ExecutionView{
			ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: confirmedStop(),
		})
		viewer := humanActor(viewerPrincipal, tenancy.RoleViewer)
		_, err := env.service.Reply(t.Context(), replyCommand(viewer, 4, questionID, 1, "Answer"))
		requireFaultClass(t, err, fault.Forbidden)
		if env.admission.callCount() != 0 {
			t.Fatalf("admission calls = %d for an unassigned viewer", env.admission.callCount())
		}
		requireNoPortCalls(t, env.ports)
	})

	t.Run("stale command role", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.put(waitingTask(t))
		env.executions.put(task.ExecutionView{
			ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: confirmedStop(),
		})
		// The command claims reviewer but the current principal has only viewer.
		env.principals.put(identity.Principal{
			ID: reviewerPrincipal, Kind: identity.KindHuman, Scope: projectScope(),
			Roles: []tenancy.Role{tenancy.RoleViewer}, Enabled: true, Version: 2,
		})
		actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
		_, err := env.service.Reply(t.Context(), replyCommand(actor, 4, questionID, 1, "Answer"))
		requireFaultClass(t, err, fault.Forbidden)
		requireNoPortCalls(t, env.ports)
	})

	t.Run("service assignment", func(t *testing.T) {
		env := newTestEnv(t)
		value := waitingTask(t)
		value.Question.Assignment = serviceAssignment(t, string(serviceAPrincipal))
		env.reader.put(value)
		env.executions.put(task.ExecutionView{
			ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: confirmedStop(),
		})

		other := serviceActor(serviceBPrincipal, tenancy.RoleReviewer)
		_, err := env.service.Reply(t.Context(), replyCommand(other, 4, questionID, 1, "Answer"))
		requireFaultClass(t, err, fault.Forbidden)
		requireNoPortCalls(t, env.ports)

		human := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
		_, err = env.service.Reply(t.Context(), replyCommand(human, 4, questionID, 1, "Answer"))
		requireFaultClass(t, err, fault.Forbidden)
		requireNoPortCalls(t, env.ports)

		assigned := serviceActor(serviceAPrincipal, tenancy.RoleReviewer)
		_, err = env.service.Reply(t.Context(), replyCommand(assigned, 4, questionID, 1, "Answer"))
		requireNoError(t, err, "assigned service reply")
		if len(env.ports.replies) != 1 {
			t.Fatalf("reply port calls = %d; want the assigned service's one reply", len(env.ports.replies))
		}
	})
}

// TestReplyRefusesStaleQuestionAndTerminalStates proves the outstanding
// question/version and terminal task state are checked before admission.
func TestReplyRefusesStaleQuestionAndTerminalStates(t *testing.T) {
	cases := map[string]struct {
		task    func(*testing.T) task.Task
		command func(authorize.Principal) task.ReplyCommand
		want    []fault.Class
	}{
		"wrong question": {
			task: waitingTask,
			command: func(actor authorize.Principal) task.ReplyCommand {
				return replyCommand(actor, 4, task.QuestionID("question-other"), 1, "Answer")
			},
			want: []fault.Class{fault.VersionConflict, fault.NotFound},
		},
		"stale question version": {
			task: waitingTask,
			command: func(actor authorize.Principal) task.ReplyCommand {
				return replyCommand(actor, 4, questionID, 0, "Answer")
			},
			want: []fault.Class{fault.VersionConflict},
		},
		"done task": {
			task: func(*testing.T) task.Task { return doneTask() },
			command: func(actor authorize.Principal) task.ReplyCommand {
				return replyCommand(actor, 4, questionID, 1, "Answer")
			},
			want: []fault.Class{fault.VersionConflict},
		},
		"cancelled task": {
			task: func(*testing.T) task.Task { return cancelledTask() },
			command: func(actor authorize.Principal) task.ReplyCommand {
				return replyCommand(actor, 4, questionID, 1, "Answer")
			},
			want: []fault.Class{fault.VersionConflict},
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
			env.reader.put(testCase.task(t))
			_, err := env.service.Reply(t.Context(), testCase.command(actor))
			requireFaultClassOneOf(t, err, testCase.want...)
			if env.admission.callCount() != 0 {
				t.Fatalf("admission calls = %d", env.admission.callCount())
			}
			requireNoPortCalls(t, env.ports)
		})
	}
}

// TestReplyOpenTaskLostResponseReachesPort proves an open task is treated as a
// possible lost-response retry and still reaches the receipt-first port.
func TestReplyOpenTaskLostResponseReachesPort(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
	value := baseTask()
	env.reader.put(value)
	original := task.ReplyResult{ExecutionID: "execution-original", LifecycleVersion: 5, QuestionVersion: 2, Replayed: true}
	env.ports.setReply(func(unit task.ReplyUnit) (task.ReplyResult, error) {
		return original, nil
	})

	result, err := env.service.Reply(t.Context(), replyCommand(actor, 4, questionID, 1, "Answer"))
	requireNoError(t, err, "Reply retry on an open task")
	if result != original {
		t.Fatalf("reply result = %+v; want the original replay %+v", result, original)
	}
	if len(env.ports.replies) != 1 {
		t.Fatalf("reply port calls = %d; want the retry to reach the port", len(env.ports.replies))
	}
}

// TestCancelCommitsCancelledTask proves cancel is one unit: cancelled state,
// execution cancellation at its expected version, credential revocation,
// event and publish.
func TestCancelCommitsCancelledTask(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(operatorPrincipal, tenancy.RoleOperator)
	value := baseTask()
	env.reader.put(value)
	env.executions.put(openExecution(2))

	command := cancelCommand(actor, 4)
	result, err := env.service.Cancel(t.Context(), command)
	requireNoError(t, err, "Cancel")

	if calls := env.executions.calledIDs(); len(calls) != 1 || calls[0] != executionOne {
		t.Fatalf("execution reads = %v; want %q", calls, executionOne)
	}
	unit := env.ports.lastCancel(t)
	if unit.Owner != actor.Scope || unit.TaskID != taskID {
		t.Fatalf("cancel identity = %+v/%q", unit.Owner, unit.TaskID)
	}
	if unit.ExpectedLifecycleVersion != 4 || unit.ExpectedExecutionVersion != 2 {
		t.Fatalf("expected versions = %d/%d; want lifecycle 4 execution 2", unit.ExpectedLifecycleVersion, unit.ExpectedExecutionVersion)
	}
	if unit.CredentialUpdate == nil || unit.CredentialUpdate.Present() {
		t.Fatalf("credential update = %+v; want an absent credential", unit.CredentialUpdate)
	}
	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.cancel", command.CallerKey, command.Content, testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.cancel", testNow)
	requireEvent(t, unit.Events, event.TypeTaskCancelled, actor.Scope, string(taskID), 5, testNow)
	requirePublish(t, unit.PostCommit)
	requireNoDispatch(t, unit.PostCommit)

	requireAuthority(t, unit.Authority, "task.cancel", authorize.CheckMembership)
	taskVersion := requireAuthority(t, unit.Authority, "task.cancel", authorize.CheckTaskVersion)
	if taskVersion.ExpectedVersion != 4 {
		t.Fatalf("task_version read = %d; want 4", taskVersion.ExpectedVersion)
	}
	executionVersion := requireAuthority(t, unit.Authority, "task.cancel", authorize.CheckExecutionVersion)
	if executionVersion.TargetID != string(executionOne) || executionVersion.ExpectedVersion != 2 {
		t.Fatalf("execution_version read = %+v; want %q@2", executionVersion, executionOne)
	}
	if result.LifecycleVersion != 5 || result.Replayed {
		t.Fatalf("cancel result = %+v; want lifecycle 5 fresh", result)
	}
	if env.admission.callCount() != 0 {
		t.Fatalf("admission calls = %d during cancel", env.admission.callCount())
	}
}

// TestCancelStateChecksStayAtThePort proves a done or already-cancelled task
// still reaches the receipt-first port: the port returns the conflict or the
// original replay, so a lost-response retry converges.
func TestCancelStateChecksStayAtThePort(t *testing.T) {
	t.Run("done task conflict", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(operatorPrincipal, tenancy.RoleOperator)
		env.reader.put(doneTask())
		env.ports.setCancel(func(unit task.CancelUnit) (task.CancelResult, error) {
			return task.CancelResult{}, fault.New(fault.VersionConflict, "task is done")
		})
		_, err := env.service.Cancel(t.Context(), cancelCommand(actor, 4))
		requireFaultClass(t, err, fault.VersionConflict)
		if len(env.ports.cancels) != 1 {
			t.Fatalf("cancel port calls = %d; want the port to check state", len(env.ports.cancels))
		}
	})
	t.Run("already cancelled replay", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(operatorPrincipal, tenancy.RoleOperator)
		env.reader.put(cancelledTask())
		original := task.CancelResult{LifecycleVersion: 5, Replayed: true}
		env.ports.setCancel(func(unit task.CancelUnit) (task.CancelResult, error) {
			return original, nil
		})
		result, err := env.service.Cancel(t.Context(), cancelCommand(actor, 4))
		requireNoError(t, err, "Cancel replay")
		if result != original {
			t.Fatalf("cancel result = %+v; want %+v", result, original)
		}
	})
}

// TestAskAndCancelConflictOnOneTaskVersion proves ask and cancel carry the same
// expected lifecycle version, so the second command loses as a version conflict
// rather than producing a partial handoff.
func TestAskAndCancelConflictOnOneTaskVersion(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	value := baseTask()
	env.reader.put(value)
	env.executions.put(openExecution(2))

	applied := uint64(4)
	env.ports.setAsk(func(unit task.AskUnit) (task.AskResult, error) {
		if unit.ExpectedLifecycleVersion != applied {
			return task.AskResult{}, fault.New(fault.VersionConflict, "stale ask")
		}
		applied++
		return task.AskResult{LifecycleVersion: applied, QuestionVersion: 1}, nil
	})
	env.ports.setCancel(func(unit task.CancelUnit) (task.CancelResult, error) {
		if unit.ExpectedLifecycleVersion != applied {
			return task.CancelResult{}, fault.New(fault.VersionConflict, "stale cancel")
		}
		applied++
		return task.CancelResult{LifecycleVersion: applied}, nil
	})

	assignment := roleAssignment(t, tenancy.RoleReviewer)
	_, err := env.service.Ask(t.Context(), askCommand(actor, 4, "Question", assignment))
	requireNoError(t, err, "Ask")
	_, err = env.service.Cancel(t.Context(), cancelCommand(actor, 4))
	requireFaultClass(t, err, fault.VersionConflict)
	if applied != 5 {
		t.Fatalf("applied version = %d; want only the ask to commit", applied)
	}
}

// requireStartAuthority returns the task.start membership read for one
// initiator. Unlike requireAuthority it does not require the read's Required set
// to equal the registry grant: a reply's initiator read deliberately narrows the
// human roles because the conditional operator retry can never apply to a fresh
// reply execution.
func requireStartAuthority(t *testing.T, reads []authorize.AuthorityRead, initiator id.PrincipalID) authorize.AuthorityRead {
	t.Helper()
	for _, read := range reads {
		if read.Operation == "task.start" && read.Check == authorize.CheckMembership && read.PrincipalID == initiator {
			return read
		}
	}
	t.Fatalf("authority reads %+v do not contain the task.start membership read for %q", reads, initiator)
	return authorize.AuthorityRead{}
}
