//go:build integration

package lifecycle

// Broker outage on the real factory composition.
//
// Coverage map (docs/specs/11-verification.md, AC 15; KODAMA.md "Broker
// independence"; docs/specs/08-operations-events.md §5):
//   - With NATS unavailable, execution observation/advancement, request
//     settlement, deadlines, task reply/queue and dispatch continue from
//     persisted state.
//   - The publication backlog grows visibly while the broker is down and drains
//     with the original event IDs when it returns.
//   - Every committed event of the fixture scope is eventually stored in the
//     events stream exactly once: the stored Nats-Msg-Id dedupe header is the
//     committed event identity, no stored message is duplicated and no stored
//     message lacks a committed event.
//   - The outage itself creates no new execution and no extra receipt.
//
// The four scenarios are progression, request wait, conversation reply and
// request deadline. Each owns a fresh harness (fresh PostgreSQL, NATS and
// Podman) so the stopped broker, the adjustable clock and the stream contents
// are isolated per scenario, and each compares the complete durable event table
// against the complete events stream of its own harness.
//
// NOT credited here, honestly:
//   - Exactly-once publication across a controller crash or a process restart:
//     the outbox is at-least-once, so this suite proves no duplicate *stored*
//     message within the stream's dedupe window and no lost event after a
//     broker restart, not crash recovery.
//   - Redelivery counts: JetStream may redeliver an event to a consumer after a
//     lost acknowledgement; that is at-least-once delivery, not duplicate
//     storage, and is not asserted.
//   - Push-trigger recovery, live log replay and credential invalidation
//     during the outage; those are covered by separate suites.
//   - A crash-before-effect/lost-response-after-effect replay for the outage
//     window: the committed-event outbox already owns that guarantee and this
//     suite does not re-open it.
//   - The reply-versus-in-progress-stop race: this scenario confirms the
//     predecessor's stop before the outage, so it proves dispatch from
//     persisted state without the broker, not the reply/stop interleaving.
//
// Reviewer question discovery: the waiting_task inbox item carries question_id,
// the question body, references, the task lifecycle version and a Version equal
// to the outstanding question version. The reply scenario therefore discovers
// the question through the assigned reviewer's own inbox, which the reviewer
// role may call, instead of an engineer-only task.read.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// outageWait is the bounded barrier for an outage-window observation.
	outageWait = 30 * time.Second
	// catchUpWait is the bounded barrier for the publication backlog to drain
	// after the broker returns.
	catchUpWait = 3 * time.Minute
)

// TestNATSOutage proves broker-independent lifecycle progression and durable
// publication catch-up on the real composition.
func TestNATSOutage(t *testing.T) {
	t.Run("progression completes and publication catches up", func(t *testing.T) {
		// A fresh harness keeps the stream comparison to this scenario's own
		// committed events.
		harness := factory.Start(t)
		testOutageProgression(t, harness)
	})
	t.Run("request wait is resolved while down and publication catches up", func(t *testing.T) {
		harness := factory.Start(t)
		testOutageRequest(t, harness)
	})
	t.Run("task reply queues a fresh execution while down and publication catches up", func(t *testing.T) {
		harness := factory.Start(t)
		testOutageReply(t, harness)
	})
	t.Run("request deadline expires while down and publication catches up", func(t *testing.T) {
		// A separate harness is required: this scenario advances the shared
		// authoritative clock by 61s.
		harness := factory.Start(t)
		testOutageDeadline(t, harness)
	})
}

// testOutageProgression stops NATS while a two-action container pipeline is
// running, proves the pipeline still completes from persisted state, then
// proves the backlog drains with the original event identities.
func testOutageProgression(t *testing.T, harness *factory.Harness) {
	const seed = "outage-seed"
	artifact := "built:" + seed
	final := "consumed:" + artifact

	document := marshalPipeline(t, pipelineDoc{
		Inputs: []string{"seed"},
		Actions: []pipelineActionDoc{
			{
				Name:    "prepare",
				Image:   harness.Image.Reference,
				Timeout: containerTimeout,
				Outputs: []string{"artifact"},
				Commands: []string{
					// The sleep keeps the outage window inside the run instead
					// of racing an instant action.
					"sleep 2",
					fmt.Sprintf(`test "$(cat /kodama/inputs/seed)" = %q`, seed),
					`printf 'shared workspace\n' > /workspace/marker`,
					`ARTIFACT="built:$(cat /kodama/inputs/seed)"`,
					submitScript("artifact", "ARTIFACT"),
				},
			},
			{
				Name:    "consume",
				Image:   harness.Image.Reference,
				Timeout: containerTimeout,
				Outputs: []string{"final"},
				Commands: []string{
					"sleep 2",
					`test "$(cat /workspace/marker)" = "shared workspace"`,
					fmt.Sprintf(`test "$(cat /kodama/outputs/prepare/artifact)" = %q`, artifact),
					`FINAL="consumed:$(cat /kodama/outputs/prepare/artifact)"`,
					submitScript("final", "FINAL"),
				},
			},
		},
	})
	applied := applyPipeline(t, harness, "outage-progression", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": seed}, "start-outage-progression")

	// The outage must begin while the pipeline is still running.
	waitRequestRunning(t, harness, started.ExecutionID, "prepare")
	requireExecutionRunning(t, harness, started.ExecutionID)

	harness.NATS.Stop(t)
	requireBrokerDown(t, harness)
	// Nothing can be acknowledged while the broker is down, so the pending
	// count is monotonic from here: a later increase is event progress
	// committed inside the outage window.
	outageBaseline := pendingPublications(t, harness)

	// Broker-independent progression: the pipeline completes while down.
	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, finished)
	if consume := actionByName(t, finished, "consume"); consume.Outputs["final"] != final {
		t.Fatalf("consume output = %q; want %q", consume.Outputs["final"], final)
	}

	// Persisted progress is readable through the public edge while the broker
	// is down, and its publication intent is still pending.
	requireEvent(t, readRequestEvents(t, harness, "execution.succeeded"), "execution.succeeded", started.ExecutionID)
	requirePendingBacklog(t, harness, outageBaseline)
	requireBrokerDown(t, harness)
	requireExecutionCount(t, harness, applied.PipelineID, 1)

	// The broker returns; the backlog drains with the original identities.
	harness.NATS.Start(t)
	catchUp := waitPublicationCatchUp(t, harness, true)
	requireOutageDedupeWindow(t, catchUp)
	requireExecutionCount(t, harness, applied.PipelineID, 1)
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// testOutageRequest stops NATS while a request is waiting, resolves it with the
// assigned reviewer while the broker is down, proves the same execution
// continues and completes, then proves the request events catch up.
func testOutageRequest(t *testing.T, harness *factory.Harness) {
	applied := applyRequestPipeline(t, harness, "outage-request", requestWaitTimeout, nil, nil)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-outage-request")

	waiting := waitRequestRunning(t, harness, started.ExecutionID, "approve")
	approve := actionByName(t, waiting, "approve")

	harness.NATS.Stop(t)
	requireBrokerDown(t, harness)
	// Nothing can be acknowledged while the broker is down, so the pending
	// count is monotonic from here: a later increase is event progress
	// committed inside the outage window.
	outageBaseline := pendingPublications(t, harness)

	// The assigned reviewer settles the wait through the public edge while the
	// broker is down.
	outcome := requestResolveCurrent(t, harness, harness.ReviewerToken, started.ExecutionID, "approve",
		"pass", map[string]string{"approved": requestApproval}, "resolve-outage-request")
	if outcome.Result.ExecutionVersion <= outcome.ExpectedExecution {
		t.Fatalf("resolve did not advance the execution version: %+v", outcome.Result)
	}

	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, finished)
	if finished.ExecutionID != started.ExecutionID {
		t.Fatalf("request pass continued execution %q; want %q", finished.ExecutionID, started.ExecutionID)
	}
	if integrate := actionByName(t, finished, "integrate"); integrate.Outputs["integrated"] != requestIntegrated {
		t.Fatalf("integrate output = %q; want %q", integrate.Outputs["integrated"], requestIntegrated)
	}

	// Both request lifecycle events are durable while the broker is down.
	requireEvent(t, readRequestEvents(t, harness, "action.requested"), "action.requested",
		started.ExecutionID, approve.ActionID)
	requireEvent(t, readRequestEvents(t, harness, "request.resolved"), "request.resolved",
		started.ExecutionID, approve.ActionID)
	requirePendingBacklog(t, harness, outageBaseline)
	requireBrokerDown(t, harness)
	requireExecutionCount(t, harness, applied.PipelineID, 1)
	requireReceiptCount(t, harness, started.ExecutionID+"/"+approve.ActionID, 1)

	harness.NATS.Start(t)
	catchUp := waitPublicationCatchUp(t, harness, false)
	requireOutageDedupeWindow(t, catchUp)
	requireExecutionCount(t, harness, applied.PipelineID, 1)
	requireReceiptCount(t, harness, started.ExecutionID+"/"+approve.ActionID, 1)
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// testOutageReply asks a conversation question, stops NATS, replies as the
// assigned reviewer while down, and proves the queued fresh execution runs to
// success without the broker (after the predecessor's stop is confirmed) and
// that the reply events catch up exactly once.
func testOutageReply(t *testing.T, harness *factory.Harness) {
	const answerMarker = "OUTAGE-ANSWER-MARKER"

	// The same pipeline serves the initial execution and the reply's fresh
	// execution. Only the fresh execution's admitted conversation snapshot
	// contains the answer, so only it completes; the initial execution waits
	// until the ask cancels it.
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "restore",
			Image:   harness.Image.Reference,
			Timeout: containerTimeout,
			Commands: []string{
				`test -n "${KODAMA_TASK:-}"`,
				`test -e /kodama/task.json`,
				`if [ -s /kodama/task.json ] && grep -q '` + answerMarker + `' /kodama/task.json; then echo restored; exit 0; fi`,
				`echo awaiting-handoff`,
				`sleep 600`,
			},
		}},
	})
	applied := applyPipeline(t, harness, "outage-reply", document)

	created := harness.MustREST(harness.ProjectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "Outage handoff",
		PipelineID: applied.PipelineID,
	}, nil, "create-outage-reply")
	var started api.TaskCreateAndStartResult
	harness.Decode(created, &started)

	// The initial execution must be live so the ask has a nonterminal execution
	// whose stop the reply's dispatch has to wait for.
	waitRequestRunning(t, harness, started.ExecutionID, "restore")

	current := readTask(t, harness, harness.ProjectToken, started.TaskID)
	asked := outageAskTaskCurrent(t, harness, harness.ProjectToken, started.TaskID,
		"Which candidate should be restored?", "role/reviewer", "ask-outage-reply")
	if asked.QuestionVersion == 0 || asked.LifecycleVersion <= current.LifecycleVersion {
		t.Fatalf("ask result = %+v; want a committed question and an advanced lifecycle version past %d",
			asked, current.LifecycleVersion)
	}

	// The assigned reviewer discovers the outstanding question identity from
	// its own inbox projection; no task.read is granted to that role. This is a
	// store read, so it stays available while the broker is down.
	reviewerInbox := inboxWaitingTasks(t, harness, harness.ReviewerToken)
	inboxItem, found := findWaitingTask(reviewerInbox, started.TaskID)
	if !found {
		t.Fatalf("reviewer inbox has no waiting_task for %q: %+v", started.TaskID, reviewerInbox.Items)
	}
	if inboxItem.QuestionID != asked.QuestionID || inboxItem.Version != asked.QuestionVersion {
		t.Fatalf("reviewer waiting_task = %+v; want question %q version %d",
			inboxItem, asked.QuestionID, asked.QuestionVersion)
	}
	waiting := readTask(t, harness, harness.ProjectToken, started.TaskID)
	if waiting.State != "waiting" {
		t.Fatalf("task state after ask = %q; want waiting", waiting.State)
	}
	if waiting.Question == nil {
		t.Fatalf("waiting task has no outstanding question: %+v", waiting)
	}
	if waiting.Question.Assignment != "role/reviewer" {
		t.Fatalf("question assignment = %q; want role/reviewer", waiting.Question.Assignment)
	}
	if inboxItem.LifecycleVersion != waiting.LifecycleVersion {
		t.Fatalf("waiting_task lifecycle version = %d; want the task lifecycle %d",
			inboxItem.LifecycleVersion, waiting.LifecycleVersion)
	}

	// Ask cancels the nonterminal execution; its confirmed stop is the dispatch
	// barrier the reply has to respect.
	cancelled := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("prior execution state after ask = %q; want cancelled", cancelled.State)
	}

	harness.NATS.Stop(t)
	requireBrokerDown(t, harness)
	// Nothing can be acknowledged while the broker is down, so the pending
	// count is monotonic from here: a later increase is event progress
	// committed inside the outage window.
	outageBaseline := pendingPublications(t, harness)

	// The assigned reviewer replies while the broker is down. The reply
	// atomically queues the fresh execution from the persisted task state; the
	// predecessor's stop was confirmed before the outage, so dispatch proceeds
	// with no broker dependency. (The reply-versus-in-progress-stop race is
	// a separate scenario, not credited here.)
	replied := replyTaskCurrent(t, harness, harness.ReviewerToken, started.TaskID,
		"Restore candidate B. "+answerMarker, "reply-outage-reply")
	if replied.ExecutionID == "" || replied.ExecutionID == started.ExecutionID {
		t.Fatalf("reply execution id = %q; want a fresh execution distinct from %q",
			replied.ExecutionID, started.ExecutionID)
	}

	fresh := waitExecution(t, harness, harness.ProjectToken, replied.ExecutionID)
	requireSucceeded(t, fresh)
	if fresh.ExecutionID != replied.ExecutionID {
		t.Fatalf("fresh execution id = %q; want %q", fresh.ExecutionID, replied.ExecutionID)
	}

	// The handoff events and the fresh execution's outcome are durable while
	// the broker is down.
	requireEvent(t, readRequestEvents(t, harness, "task.created"), "task.created", started.TaskID)
	requireEvent(t, readRequestEvents(t, harness, "task.question"), "task.question", started.TaskID)
	requireEvent(t, readRequestEvents(t, harness, "task.reply"), "task.reply", started.TaskID)
	requireEvent(t, readRequestEvents(t, harness, "execution.succeeded"), "execution.succeeded", replied.ExecutionID)
	requirePendingBacklog(t, harness, outageBaseline)
	requireBrokerDown(t, harness)

	// The task conversation reflects the answer and exactly one fresh execution.
	afterReply := readTask(t, harness, harness.ProjectToken, started.TaskID)
	if afterReply.CurrentExecution != replied.ExecutionID {
		t.Fatalf("task current execution = %q; want %q", afterReply.CurrentExecution, replied.ExecutionID)
	}
	if len(afterReply.ExecutionHistory) != 2 {
		t.Fatalf("task execution history = %v; want exactly the initial and fresh execution", afterReply.ExecutionHistory)
	}
	if afterReply.Question != nil {
		t.Fatalf("answered question is still outstanding: %+v", afterReply.Question)
	}
	if !taskConversationContains(afterReply.Messages, answerMarker) {
		t.Fatalf("task conversation does not carry the reply answer: %+v", afterReply.Messages)
	}
	requireExecutionCount(t, harness, applied.PipelineID, 2)

	harness.NATS.Start(t)
	catchUp := waitPublicationCatchUp(t, harness, false)
	requireOutageDedupeWindow(t, catchUp)

	// The outage created no extra execution and no extra reply receipt.
	requireExecutionCount(t, harness, applied.PipelineID, 2)
	requireReceiptCount(t, harness, "reply-outage-reply", 1)
}

// testOutageDeadline stops NATS while a request waits, advances the authoritative
// clock past its persisted deadline, and proves the engine expires it from
// persisted state without the broker, then catches the events up.
func testOutageDeadline(t *testing.T, harness *factory.Harness) {
	applied := applyRequestPipeline(t, harness, "outage-deadline", "1m", nil, nil)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-outage-deadline")

	waiting := waitRequestRunning(t, harness, started.ExecutionID, "approve")
	approve := actionByName(t, waiting, "approve")

	harness.NATS.Stop(t)
	requireBrokerDown(t, harness)
	// Nothing can be acknowledged while the broker is down, so the pending
	// count is monotonic from here: a later increase is event progress
	// committed inside the outage window.
	outageBaseline := pendingPublications(t, harness)

	harness.Clock.Advance(61 * time.Second)

	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if finished.State != "failed" {
		t.Fatalf("expired execution state = %q; want failed (failure=%+v)", finished.State, finished.Failure)
	}
	if finished.Failure == nil || finished.Failure.Kind != "timeout" {
		t.Fatalf("expired execution failure = %+v; want kind timeout", finished.Failure)
	}
	expired := actionByName(t, finished, "approve")
	if expired.Decision != "" || expired.Actor != "" || expired.AnsweredAt != nil {
		t.Fatalf("expired request retained an answer: %+v", expired)
	}

	requireEvent(t, readRequestEvents(t, harness, "execution.failed"), "execution.failed", started.ExecutionID)
	requirePendingBacklog(t, harness, outageBaseline)
	requireBrokerDown(t, harness)
	requireExecutionCount(t, harness, applied.PipelineID, 1)
	requireReceiptCount(t, harness, started.ExecutionID+"/"+approve.ActionID, 0)

	harness.NATS.Start(t)
	catchUp := waitPublicationCatchUp(t, harness, false)
	requireOutageDedupeWindow(t, catchUp)
	requireExecutionCount(t, harness, applied.PipelineID, 1)
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// catchUpResult is the settled publication comparison of one harness.
type catchUpResult struct {
	events int
	window time.Duration
}

// waitPublicationCatchUp waits until the durable outbox is drained and every
// committed project event is stored in the events stream exactly once with its
// own identity as the JetStream dedupe header. requireOrder additionally checks
// that events sharing a subject keep their durable sequence order; it is only
// used where one writer produces the subject's events, because the public
// contract explicitly tolerates consumer-side reordering.
func waitPublicationCatchUp(t *testing.T, harness *factory.Harness, requireOrder bool) catchUpResult {
	t.Helper()
	deadline := time.Now().Add(catchUpWait)
	var last string
	for {
		pending, err := harness.PendingPublications(t.Context())
		if err != nil {
			last = fmt.Sprintf("read pending publications: %v", err)
		} else if pending != 0 {
			last = fmt.Sprintf("%d publication intents are still pending", pending)
		} else {
			durable := readAllProjectEvents(t, harness)
			stream, window, streamErr := harness.EventStream(t.Context())
			if streamErr != nil {
				last = fmt.Sprintf("read events stream: %v", streamErr)
			} else if mismatch := comparePublication(durable, stream, requireOrder); mismatch != "" {
				last = mismatch
			} else {
				return catchUpResult{events: len(durable), window: window}
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("publication did not catch up within %s: %s\ncomposition log:\n%s",
				catchUpWait, last, factory.SanitizeDiagnostic(harness.Logs()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireOutageDedupeWindow proves the observed outage is inside the stream's
// message-ID dedupe window, which is what makes "no duplicate stored message"
// meaningful for a republish after a lost acknowledgement.
func requireOutageDedupeWindow(t *testing.T, result catchUpResult) {
	t.Helper()
	if result.events == 0 {
		t.Fatalf("publication catch-up compared zero committed events")
	}
	if result.window <= 0 {
		t.Fatalf("events stream has no message-ID dedupe window")
	}
	if result.window < time.Minute {
		t.Fatalf("events stream dedupe window = %s; want at least a minute to cover a retry burst", result.window)
	}
}

// comparePublication compares the committed project events read through
// event.read with the messages stored in the events stream. It returns an empty
// string when every committed event is stored exactly once, no stored message
// is duplicated or unaccounted for, and each stored message carries the
// committed event identity as its dedupe header.
func comparePublication(events []api.Event, stream []factory.StreamEvent, requireOrder bool) string {
	expectedSubject := fmt.Sprintf("kodama.v1.%s.%s.events", factory.TenantID, factory.ProjectID)

	durableOrder := make([]string, 0, len(events))
	subjectOf := make(map[string]string, len(events))
	durableIDs := make(map[string]struct{}, len(events))
	durableBySubject := make(map[string][]string)
	for _, event := range events {
		if _, duplicate := durableIDs[event.ID]; duplicate {
			return fmt.Sprintf("event.read returned duplicate committed event id %q", event.ID)
		}
		durableIDs[event.ID] = struct{}{}
		durableOrder = append(durableOrder, event.ID)
		subjectOf[event.ID] = event.SubjectID
		durableBySubject[event.SubjectID] = append(durableBySubject[event.SubjectID], event.ID)
	}

	storedCount := make(map[string]int, len(stream))
	storedBySubject := make(map[string][]string)
	ignored := 0
	for _, message := range stream {
		if message.Subject != expectedSubject {
			// The events stream also captures tenant-scope subjects, which a
			// project-scoped read cannot see; they are outside this comparison.
			ignored++
			continue
		}
		if message.MessageID == "" {
			return fmt.Sprintf("stored stream message %d has no Nats-Msg-Id header", message.Sequence)
		}
		if message.EventID == "" {
			return fmt.Sprintf("stored stream message %d has no event identity in its payload", message.Sequence)
		}
		if message.MessageID != message.EventID {
			return fmt.Sprintf("stored stream message %d dedupe identity %q does not match payload event id %q",
				message.Sequence, message.MessageID, message.EventID)
		}
		if _, committed := durableIDs[message.EventID]; !committed {
			return fmt.Sprintf("stored stream message %d carries event %q that is not a committed event of this scope",
				message.Sequence, message.EventID)
		}
		storedCount[message.EventID]++
		if storedCount[message.EventID] > 1 {
			return fmt.Sprintf("committed event %q is stored %d times in the events stream",
				message.EventID, storedCount[message.EventID])
		}
		subject := subjectOf[message.EventID]
		storedBySubject[subject] = append(storedBySubject[subject], message.EventID)
	}

	missing := make([]string, 0)
	for _, eventID := range durableOrder {
		if storedCount[eventID] == 0 {
			missing = append(missing, eventID)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return fmt.Sprintf("%d committed events were never stored: %v (stream ignored %d other-scope messages)",
			len(missing), missing, ignored)
	}

	if requireOrder {
		for subject, durable := range durableBySubject {
			stored := storedBySubject[subject]
			if len(stored) != len(durable) {
				return fmt.Sprintf("subject %q stored %d events; want %d", subject, len(stored), len(durable))
			}
			for index := range durable {
				if stored[index] != durable[index] {
					return fmt.Sprintf("subject %q stored order %v; want durable order %v", subject, stored, durable)
				}
			}
		}
	}
	return ""
}

// readAllProjectEvents pages every committed project event through the public
// event read.
func readAllProjectEvents(t *testing.T, harness *factory.Harness) []api.Event {
	t.Helper()
	events := make([]api.Event, 0, 64)
	cursor := ""
	for range 200 {
		result := harness.MustREST(harness.ProjectToken, "event.read", api.EventReadRequest{
			ProjectID: factory.ProjectID,
			Page:      &api.Page{Limit: api.MaxPageLimit, Cursor: cursor},
		}, nil, "")
		var read api.EventReadResult
		harness.Decode(result, &read)
		events = append(events, read.Events...)
		if !read.HasMore {
			return events
		}
		if read.Next == "" || read.Next == cursor {
			t.Fatalf("event.read returned has_more without an advancing cursor: %q", read.Next)
		}
		cursor = read.Next
	}
	t.Fatalf("event.read did not terminate within 200 pages")
	return nil
}

// requireBrokerDown proves the outage window is real: the broker's client port
// refuses connections.
func requireBrokerDown(t *testing.T, harness *factory.Harness) {
	t.Helper()
	harness.Eventually(t, outageWait, func() bool {
		return harness.BrokerUnreachable(t.Context())
	})
}

// pendingPublications reads the durable pending publication count.
func pendingPublications(t *testing.T, harness *factory.Harness) int64 {
	t.Helper()
	count, err := harness.PendingPublications(t.Context())
	if err != nil {
		t.Fatalf("read pending publications: %v", err)
	}
	return count
}

// requirePendingBacklog proves the durable outbox grew past the backlog that
// existed when the broker stopped: at least one event committed while the
// broker was unavailable is still unacknowledged, so the outage window really
// contains persisted progress that has to catch up.
func requirePendingBacklog(t *testing.T, harness *factory.Harness, baseline int64) {
	t.Helper()
	var last int64
	var lastErr error
	harness.Eventually(t, outageWait, func() bool {
		last, lastErr = harness.PendingPublications(t.Context())
		return lastErr == nil && last > baseline
	})
	if lastErr != nil {
		t.Fatalf("read pending publications: %v", lastErr)
	}
	if last <= baseline {
		t.Fatalf("pending publication intent = %d; want more than the %d present when the broker stopped",
			last, baseline)
	}
}

// requireExecutionRunning proves one execution is still nonterminal.
func requireExecutionRunning(t *testing.T, harness *factory.Harness, executionID string) {
	t.Helper()
	execution := inspectExecution(t, harness, harness.ProjectToken, executionID)
	if execution.State != "running" {
		t.Fatalf("execution %s state = %q; want running when the outage begins", executionID, execution.State)
	}
}

// requireExecutionCount proves the pipeline has exactly the expected number of
// executions, so the outage created no duplicate execution.
func requireExecutionCount(t *testing.T, harness *factory.Harness, pipelineID string, want int64) {
	t.Helper()
	count, err := harness.ExecutionCount(t.Context(), pipelineID)
	if err != nil {
		t.Fatalf("count executions of pipeline %s: %v", pipelineID, err)
	}
	if count != want {
		t.Fatalf("pipeline %s execution count = %d; want %d", pipelineID, count, want)
	}
}

// requireReceiptCount proves one idempotency key has exactly the expected number
// of receipts, so a retry across the outage committed no second receipt.
func requireReceiptCount(t *testing.T, harness *factory.Harness, callerKey string, want int64) {
	t.Helper()
	count, err := harness.ReceiptCount(t.Context(), callerKey)
	if err != nil {
		t.Fatalf("count receipts of %q: %v", callerKey, err)
	}
	if count != want {
		t.Fatalf("receipt count of %q = %d; want %d", callerKey, count, want)
	}
}

// outageAskTaskCurrent asks a conversation question from the latest task read.
// The running execution advances its own version while it activates and runs,
// and task.ask internally guards that execution version even though the public
// request carries only the task lifecycle version, so an ask that races live
// execution progress is refused with version_conflict. The client can only
// retry from a fresh read; a refused attempt commits nothing, so each retry uses
// its own idempotency key.
//
// It is deliberately separate from the handoff suite's askTaskCurrent: that
// helper retries a fixed number of times with one key, while an outage test
// must keep retrying across the whole outageWait budget and must fail when the
// task left "open" under it.
func outageAskTaskCurrent(t *testing.T, harness *factory.Harness, token, taskID, question, assignment, key string) api.TaskAskResult {
	t.Helper()
	deadline := time.Now().Add(outageWait)
	for attempt := 0; ; attempt++ {
		current := readTask(t, harness, token, taskID)
		if current.State != "open" {
			t.Fatalf("task %s state = %q; want open before ask", taskID, current.State)
		}
		result, publicError := harness.REST(token, "task.ask", api.TaskAskRequest{
			TaskID:     taskID,
			Question:   question,
			Assignment: assignment,
		}, &current.LifecycleVersion, fmt.Sprintf("%s-%d", key, attempt))
		if publicError.Code == api.CodeVersionConflict {
			if !time.Now().Before(deadline) {
				t.Fatalf("task.ask did not converge on a current version within %s: %s (%s)",
					outageWait, publicError.Code, publicError.Message)
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("task.ask refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var asked api.TaskAskResult
		harness.Decode(result, &asked)
		return asked
	}
}

// replyTaskCurrent replies to the outstanding question from the assigned
// reviewer's own inbox projection. The inbox carries the question_id, the
// question version and the task lifecycle version, so the reviewer needs no
// task.read. A stop confirmation can advance the predecessor execution version
// while the reply is composed, so a guard conflict is retried from a fresh
// inbox read; a refused attempt commits nothing, so the same idempotency key is
// safe to repeat.
func replyTaskCurrent(t *testing.T, harness *factory.Harness, replyToken, taskID, answer, key string) api.TaskReplyResult {
	t.Helper()
	deadline := time.Now().Add(outageWait)
	for {
		inbox := inboxWaitingTasks(t, harness, replyToken)
		item, found := findWaitingTask(inbox, taskID)
		if !found {
			t.Fatalf("reviewer inbox has no waiting_task for %s: %+v", taskID, inbox.Items)
		}
		lifecycle := item.LifecycleVersion
		result, publicError := harness.REST(replyToken, "task.reply", api.TaskReplyRequest{
			TaskID:                  taskID,
			QuestionID:              item.QuestionID,
			ExpectedQuestionVersion: item.Version,
			Answer:                  answer,
		}, &lifecycle, key)
		if publicError.Code == api.CodeVersionConflict {
			if !time.Now().Before(deadline) {
				t.Fatalf("task.reply did not converge on a current version within %s: %s (%s)",
					outageWait, publicError.Code, publicError.Message)
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("task.reply refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var replied api.TaskReplyResult
		harness.Decode(result, &replied)
		return replied
	}
}

// taskConversationContains reports whether one conversation message carries the
// exact text.
func taskConversationContains(messages []api.TaskMessage, text string) bool {
	for _, message := range messages {
		if strings.Contains(message.Body, text) {
			return true
		}
	}
	return false
}
