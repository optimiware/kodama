package domaincontract

// Store-adapter seam vectors: the request resolve unit must carry the Kernel
// answer reduction, the admission snapshot must carry admitted environment
// defaults and distinguish an absent conversation from an admitted empty one,
// the pipeline change/read seams must carry the validated on.push projection,
// and the trigger units must state their common-receipt mode and the durable
// event-consumption marker they write. Every case below is a declaration and
// value-level contract check; no adapter, store or process is exercised.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// sampleAnswerCommit is the Kernel reduction flow.Reduce returns for one
// authorized request answer. The reducer output, not a Shell state machine, is
// what the resolve unit carries.
func sampleAnswerCommit(now time.Time) flow.Commit {
	answer := kernel.Answer{
		Decision: kernel.DecisionPass,
		Outputs:  kernel.Outputs{"approved": "true"},
		Actor:    id.PrincipalID("reviewer-1"),
		At:       now,
	}
	return flow.Commit{
		ExecutionID:              id.ExecutionID("exec-1"),
		ExpectedExecutionVersion: 5,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 id.ActionID("action-1"),
		ExpectedActionVersion:    3,
		NextActionState:          kernel.ActionSucceeded,
		Output:                   flow.OutputChange{Action: flow.OutputPromote, Values: kernel.Outputs{"approved": "true"}},
		Answer:                   &answer,
		At:                       now,
	}
}

func sampleResolveUnit(t *testing.T, now time.Time) request.ResolveUnit {
	t.Helper()
	return request.ResolveUnit{
		Owner:                    sampleScope(),
		ExecutionID:              id.ExecutionID("exec-1"),
		ActionID:                 id.ActionID("action-1"),
		ExpectedExecutionVersion: 5,
		ExpectedActionVersion:    3,
		Decision:                 request.DecisionPass,
		Response:                 kernel.Outputs{"approved": "true"},
		Actor:                    id.PrincipalID("reviewer-1"),
		Assignment:               mustAssignment(t, "role/reviewer"),
		At:                       now,
		IdempotencyKey:           "exec-1/action-1",
		Receipt:                  sampleReceipt(),
		Commit:                   sampleAnswerCommit(now),
	}
}

func invalidInputClass(t *testing.T, err error) {
	t.Helper()
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.InvalidInput {
		t.Fatalf("error = %v; want class %q", err, fault.InvalidInput)
	}
}

// TestResolveUnitCarriesKernelAnswerCommit pins that the resolve unit carries
// the Kernel reduction produced by flow.Reduce with an answer fact, keeps
// every existing field, and Validate refuses a unit that carries no coherent
// reduction.
func TestResolveUnitCarriesKernelAnswerCommit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	unit := sampleResolveUnit(t, now)
	typ := reflect.TypeOf(unit)
	for _, field := range []string{
		"Owner", "ExecutionID", "ActionID", "ExpectedExecutionVersion", "ExpectedActionVersion",
		"Decision", "Response", "Actor", "Assignment", "At", "IdempotencyKey",
		"Receipt", "Audit", "Events", "Authority", "PostCommit", "Commit",
	} {
		if _, ok := typ.FieldByName(field); !ok {
			t.Fatalf("request.ResolveUnit is missing field %q", field)
		}
	}
	commitField, _ := typ.FieldByName("Commit")
	if got := commitField.Type; got != reflect.TypeOf(flow.Commit{}) {
		t.Fatalf("request.ResolveUnit.Commit type = %v; want flow.Commit", got)
	}
	if err := unit.Validate(); err != nil {
		t.Fatalf("complete resolve unit was refused: %v", err)
	}
	if unit.Commit.Answer == nil || unit.Commit.Answer.Decision != kernel.DecisionPass {
		t.Fatalf("resolve unit lost the Kernel answer reduction: %+v", unit.Commit)
	}
	if unit.Decision != request.DecisionPass || unit.Response["approved"] != "true" {
		t.Fatalf("resolve unit lost its existing decision/response payload: %+v", unit)
	}

	missing := unit
	missing.Commit = flow.Commit{}
	invalidInputClass(t, missing.Validate())

	otherAction := unit
	otherAction.Commit.ActionID = id.ActionID("action-2")
	invalidInputClass(t, otherAction.Validate())

	otherExecution := unit
	otherExecution.Commit.ExecutionID = id.ExecutionID("exec-2")
	invalidInputClass(t, otherExecution.Validate())

	noAnswer := unit
	noAnswer.Commit.Answer = nil
	invalidInputClass(t, noAnswer.Validate())

	zeroTime := unit
	zeroTime.Commit.At = time.Time{}
	invalidInputClass(t, zeroTime.Validate())

	unknownState := unit
	unknownState.Commit.NextActionState = kernel.ActionState("unknown")
	invalidInputClass(t, unknownState.Validate())
}

// sampleFailResolveUnit is the first-settlement unit for a failing decision.
// flow.Reduce(AnswerFact) commits a failed action and a failed execution for
// kernel.DecisionFail, so the reduction must carry exactly that pair.
func sampleFailResolveUnit(t *testing.T, now time.Time) request.ResolveUnit {
	t.Helper()
	unit := sampleResolveUnit(t, now)
	unit.Decision = request.DecisionFail
	unit.Response = nil
	unit.Commit.Answer = &kernel.Answer{
		Decision: kernel.DecisionFail,
		Actor:    unit.Actor,
		At:       now,
	}
	unit.Commit.NextActionState = kernel.ActionFailed
	unit.Commit.NextExecutionState = kernel.ExecutionFailed
	unit.Commit.Output = flow.OutputChange{Action: flow.OutputDiscard}
	return unit
}

// TestResolveUnitValidateBindsCommitToUnit pins the first-settlement binding:
// the carried reduction must be the one flow.Reduce produces for this exact
// unit, so a decision, actor, expected-version or next-state mismatch is
// invalid_input instead of applying foreign Kernel state. The legal next-state
// pairs are derived from the reducer: pass commits a succeeded action with a
// running or succeeded execution; fail commits a failed action and execution.
func TestResolveUnitValidateBindsCommitToUnit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	pass := sampleResolveUnit(t, now)
	if err := pass.Validate(); err != nil {
		t.Fatalf("coherent passing resolve unit was refused: %v", err)
	}
	fail := sampleFailResolveUnit(t, now)
	if err := fail.Validate(); err != nil {
		t.Fatalf("coherent failing resolve unit was refused: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*request.ResolveUnit)
	}{
		{
			name: "answer decision mismatch",
			mutate: func(unit *request.ResolveUnit) {
				unit.Commit.Answer.Decision = kernel.DecisionFail
			},
		},
		{
			name: "answer actor mismatch",
			mutate: func(unit *request.ResolveUnit) {
				unit.Commit.Answer.Actor = id.PrincipalID("someone-else")
			},
		},
		{
			name: "execution version mismatch",
			mutate: func(unit *request.ResolveUnit) {
				unit.Commit.ExpectedExecutionVersion = unit.ExpectedExecutionVersion + 1
			},
		},
		{
			name: "action version mismatch",
			mutate: func(unit *request.ResolveUnit) {
				unit.Commit.ExpectedActionVersion = unit.ExpectedActionVersion + 1
			},
		},
		{
			name: "pass commits a failed action",
			mutate: func(unit *request.ResolveUnit) {
				unit.Commit.NextActionState = kernel.ActionFailed
			},
		},
		{
			name: "pass commits a failed execution",
			mutate: func(unit *request.ResolveUnit) {
				unit.Commit.NextExecutionState = kernel.ExecutionFailed
			},
		},
		{
			name: "undeclared resolve decision",
			mutate: func(unit *request.ResolveUnit) {
				unit.Decision = request.Decision("maybe")
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			unit := pass
			// Commit.Answer is a pointer, so clone it before mutating the
			// shared reduction in a negative case.
			answer := *unit.Commit.Answer
			unit.Commit.Answer = &answer
			testCase.mutate(&unit)
			invalidInputClass(t, unit.Validate())
		})
	}

	t.Run("pass commits a succeeded execution", func(t *testing.T) {
		unit := pass
		unit.Commit.NextExecutionState = kernel.ExecutionSucceeded
		if err := unit.Validate(); err != nil {
			t.Fatalf("passing unit with a succeeded execution was refused: %v", err)
		}
	})
	t.Run("fail commits a succeeded action", func(t *testing.T) {
		unit := fail
		unit.Commit.NextActionState = kernel.ActionSucceeded
		invalidInputClass(t, unit.Validate())
	})
	t.Run("fail commits a running execution", func(t *testing.T) {
		unit := fail
		unit.Commit.NextExecutionState = kernel.ExecutionRunning
		invalidInputClass(t, unit.Validate())
	})
}

// replayResolveUnit is the equal-content retry of an already-answered action:
// flow.Reduce returns (execution, nil, nil) for it, so the unit legitimately
// carries a zero Commit and the adapter must find the committed receipt before
// requiring the reduction.
func replayResolveUnit(t *testing.T, now time.Time) request.ResolveUnit {
	t.Helper()
	unit := sampleResolveUnit(t, now)
	unit.Commit = flow.Commit{}
	unit.Receipt.Key.PrincipalID = unit.Actor
	unit.Receipt.Key.CallerKey = unit.IdempotencyKey
	return unit
}

// TestResolveUnitReplayValidationAllowsZeroCommit pins the replay path split:
// ValidateReplay accepts the zero-Commit equal retry with the receipt-first
// identity, while first-settlement Validate refuses the same forged omission.
// A miss with a zero Commit is refused because there is no settled result to
// return and no reduction to apply.
func TestResolveUnitReplayValidationAllowsZeroCommit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	replay := replayResolveUnit(t, now)
	if err := replay.ValidateReplay(); err != nil {
		t.Fatalf("zero-commit replay unit was refused: %v", err)
	}
	invalidInputClass(t, replay.Validate())

	withCommit := replay
	withCommit.Commit = sampleAnswerCommit(now)
	if err := withCommit.ValidateReplay(); err != nil {
		t.Fatalf("replay unit carrying a first settlement was refused: %v", err)
	}
	brokenCommit := withCommit
	brokenCommit.Commit.Answer = nil
	invalidInputClass(t, brokenCommit.ValidateReplay())

	cases := []struct {
		name   string
		mutate func(*request.ResolveUnit)
	}{
		{
			name: "missing execution identity",
			mutate: func(unit *request.ResolveUnit) {
				unit.ExecutionID = ""
			},
		},
		{
			name: "missing action identity",
			mutate: func(unit *request.ResolveUnit) {
				unit.ActionID = ""
			},
		},
		{
			name: "missing idempotency key",
			mutate: func(unit *request.ResolveUnit) {
				unit.IdempotencyKey = ""
			},
		},
		{
			name: "missing expected versions",
			mutate: func(unit *request.ResolveUnit) {
				unit.ExpectedActionVersion = 0
			},
		},
		{
			name: "missing receipt key",
			mutate: func(unit *request.ResolveUnit) {
				unit.Receipt.Key = receipt.Key{}
			},
		},
		{
			name: "receipt key outside the unit scope",
			mutate: func(unit *request.ResolveUnit) {
				unit.Receipt.Key.TenantID = id.TenantID("tenant-2")
			},
		},
		{
			name: "receipt key names another principal",
			mutate: func(unit *request.ResolveUnit) {
				unit.Receipt.Key.PrincipalID = id.PrincipalID("human-9")
			},
		},
		{
			name: "receipt caller key mismatch",
			mutate: func(unit *request.ResolveUnit) {
				unit.Receipt.Key.CallerKey = "other-key"
			},
		},
		{
			name: "missing owner scope",
			mutate: func(unit *request.ResolveUnit) {
				unit.Owner = tenancy.Scope{}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			unit := replay
			testCase.mutate(&unit)
			invalidInputClass(t, unit.ValidateReplay())
		})
	}
}

// TestSnapshotCarriesAdmittedEnvironmentDefaults pins that the admitted
// environment defaults are copied into the immutable snapshot and later caller
// edits cannot mutate them.
func TestSnapshotCarriesAdmittedEnvironmentDefaults(t *testing.T) {
	env := []definition.EnvVar{
		{Name: id.EnvName("LOG_LEVEL"), Value: "info"},
		{Name: id.EnvName("CI"), Value: "true"},
	}
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML: []byte("actions: []\n"),
		Env:  env,
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if !reflect.DeepEqual(snapshot.Env, env) {
		t.Fatalf("snapshot env = %+v; want %+v", snapshot.Env, env)
	}
	env[0].Value = "debug"
	if snapshot.Env[0].Value != "info" {
		t.Fatalf("snapshot env aliases the admission input: %+v", snapshot.Env)
	}

	withoutEnv, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions: []\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if withoutEnv.Env != nil {
		t.Fatalf("snapshot env = %+v; want nil when no defaults were admitted", withoutEnv.Env)
	}
}

// TestSnapshotDistinguishesAbsentAndEmptyConversation pins the
// nil-versus-empty rule: a task-free execution has an absent conversation
// while an admitted empty conversation is present, both in memory and in the
// serialized snapshot.
func TestSnapshotDistinguishesAbsentAndEmptyConversation(t *testing.T) {
	taskFree, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions: []\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if taskFree.ConversationBytes() != nil {
		t.Fatalf("task-free conversation = %q; want nil", taskFree.ConversationBytes())
	}
	if taskFree.HasConversation() {
		t.Fatal("task-free snapshot reports an admitted conversation")
	}

	empty, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         []byte("actions: []\n"),
		Conversation: []byte{},
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if empty.ConversationBytes() == nil {
		t.Fatal("admitted empty conversation collapsed to absent")
	}
	if len(empty.ConversationBytes()) != 0 {
		t.Fatalf("admitted empty conversation = %q; want empty", empty.ConversationBytes())
	}
	if !empty.HasConversation() {
		t.Fatal("admitted empty conversation is not reported as present")
	}

	present, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         []byte("actions: []\n"),
		Conversation: []byte("please review"),
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if !bytes.Equal(present.ConversationBytes(), []byte("please review")) {
		t.Fatalf("conversation = %q; want %q", present.ConversationBytes(), "please review")
	}

	taskFreeJSON, err := json.Marshal(taskFree)
	if err != nil {
		t.Fatalf("marshal task-free snapshot: %v", err)
	}
	emptyJSON, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty-conversation snapshot: %v", err)
	}
	if bytes.Equal(taskFreeJSON, emptyJSON) {
		t.Fatalf("absent and admitted empty conversation serialize identically: %s", taskFreeJSON)
	}
	var absentDocument, emptyDocument struct {
		Conversation *string `json:"conversation"`
	}
	if err := json.Unmarshal(taskFreeJSON, &absentDocument); err != nil {
		t.Fatalf("decode task-free snapshot: %v", err)
	}
	if absentDocument.Conversation != nil {
		t.Fatalf("task-free conversation serialized as %q; want null/absent", *absentDocument.Conversation)
	}
	if err := json.Unmarshal(emptyJSON, &emptyDocument); err != nil {
		t.Fatalf("decode empty-conversation snapshot: %v", err)
	}
	if emptyDocument.Conversation == nil || *emptyDocument.Conversation != "" {
		t.Fatalf("admitted empty conversation serialized as %v; want an empty string", emptyDocument.Conversation)
	}
}

// TestPushProjectionDeclaredOnPipelineChangeAndReadSeams pins the push seam:
// the pipeline change unit carries the validated on.push projection and the
// read definition exposes the same projection. The single declared trigger
// maps to exactly one projection entry with its repository, service and exact
// refs.
func TestPushProjectionDeclaredOnPipelineChangeAndReadSeams(t *testing.T) {
	triggerValue := pipeline.PushTrigger{
		Repository: "resource-1",
		Service:    "service-1",
		Refs:       []string{"refs/heads/main"},
	}
	projection := []pipeline.PushTrigger{triggerValue}

	unitType := reflect.TypeOf(pipeline.ChangeUnit{})
	field, ok := unitType.FieldByName("PushTriggers")
	if !ok {
		t.Fatal("pipeline.ChangeUnit is missing the PushTriggers projection")
	}
	if field.Type != reflect.TypeOf(projection) {
		t.Fatalf("pipeline.ChangeUnit.PushTriggers type = %v; want %v", field.Type, reflect.TypeOf(projection))
	}
	definitionType := reflect.TypeOf(pipeline.Definition{})
	field, ok = definitionType.FieldByName("PushTriggers")
	if !ok {
		t.Fatal("pipeline.Definition is missing the PushTriggers projection")
	}
	if field.Type != reflect.TypeOf(projection) {
		t.Fatalf("pipeline.Definition.PushTriggers type = %v; want %v", field.Type, reflect.TypeOf(projection))
	}

	unit := pipeline.ChangeUnit{PushTriggers: projection}
	definition := pipeline.Definition{PushTriggers: projection}
	if len(unit.PushTriggers) != 1 || unit.PushTriggers[0].Repository != "resource-1" ||
		unit.PushTriggers[0].Service != "service-1" || len(unit.PushTriggers[0].Refs) != 1 {
		t.Fatalf("change unit lost the push projection: %+v", unit.PushTriggers)
	}
	if len(definition.PushTriggers) != 1 || definition.PushTriggers[0].Refs[0] != "refs/heads/main" {
		t.Fatalf("definition lost the push projection: %+v", definition.PushTriggers)
	}
}

// TestTriggerReceiptModes pins the receipt modes on the domain side: the
// selection inserts the common receipt and the start (or refusal) completes it
// under the same key, so the store unit can carry an explicit receipt mode.
func TestTriggerReceiptModes(t *testing.T) {
	if got := (trigger.SelectUnit{}).ReceiptMode(); got != trigger.ReceiptInsert {
		t.Fatalf("trigger.SelectUnit receipt mode = %q; want %q", got, trigger.ReceiptInsert)
	}
	if got := (trigger.StartUnit{}).ReceiptMode(); got != trigger.ReceiptComplete {
		t.Fatalf("trigger.StartUnit receipt mode = %q; want %q", got, trigger.ReceiptComplete)
	}
	if trigger.ReceiptInsert == trigger.ReceiptComplete {
		t.Fatal("insert and complete receipt modes are the same value")
	}
	if !trigger.ReceiptInsert.Valid() || !trigger.ReceiptComplete.Valid() {
		t.Fatal("a declared receipt mode is not valid")
	}
	if trigger.ReceiptMode("upsert").Valid() {
		t.Fatal("an unknown receipt mode was accepted")
	}
}

// TestTriggerCompletionWritesEventConsumptionMarker pins that the completion
// unit carries the settled event identity and the port writes the durable
// event_consumptions marker under one stable consumer name.
func TestTriggerCompletionWritesEventConsumptionMarker(t *testing.T) {
	if trigger.CompletionConsumer != "trigger" {
		t.Fatalf("trigger completion consumer = %q; want %q", trigger.CompletionConsumer, "trigger")
	}
	unit := trigger.CompletionUnit{
		Owner:    sampleScope(),
		Sequence: 7,
		EventID:  id.EventID("event-1"),
	}
	if unit.Sequence != 7 || unit.EventID != "event-1" || unit.Owner.TenantID != "tenant-1" {
		t.Fatalf("completion unit lost its settled event identity: %+v", unit)
	}
}
