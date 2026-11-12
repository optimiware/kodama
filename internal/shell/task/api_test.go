package task_test

// Task API pinned by these tests.
//
// types.go and ports.go declare the task conversation, its two optimistic
// versions and the start/ask/reply/cancel commit units. The creation, editing,
// progress and read seams are declared alongside them in package task.
//
// The names, signatures and rules below are the exact API these behavioral
// tests exercise:
//
//	type Clock interface { Now() time.Time }
//
//	type IDSource interface {
//	    NewTaskID() (id.TaskID, error)
//	    NewQuestionID() (QuestionID, error)
//	    NewExecutionID() (id.ExecutionID, error)
//	    NewPrincipalID() (id.PrincipalID, error)
//	    NewEventID() (id.EventID, error)
//	    NewReceiptID() (id.ReceiptID, error)
//	}
//
//	type Admission interface {
//	    Admit(context.Context, pipeline.AdmitRequest) (pipeline.AdmissionResult, error)
//	}
//
//	type PipelineReader interface {
//	    Get(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error)
//	}
//
//	type ExecutionView struct {
//	    ID           id.ExecutionID
//	    Version      uint64
//	    State        kernel.ExecutionState
//	    StopEvidence kernel.StopEvidence
//	}
//
//	type ExecutionReader interface {
//	    Get(context.Context, tenancy.Scope, id.ExecutionID) (ExecutionView, error)
//	}
//
//	type HistoryKind string
//	const (
//	    HistoryExecution HistoryKind = "execution"
//	    HistoryMessage   HistoryKind = "message"
//	    HistoryQuestion  HistoryKind = "question"
//	)
//
//	type HistoryQuery struct { Limit int; Cursor string }
//
//	type HistoryEntry struct {
//	    Kind        HistoryKind
//	    ExecutionID id.ExecutionID
//	    State       kernel.ExecutionState
//	    Message     *Message
//	    Question    *Question
//	    At          time.Time
//	}
//
//	type HistoryPage struct { Entries []HistoryEntry; NextCursor string }
//
//	type Reader interface {
//	    Get(context.Context, tenancy.Scope, id.TaskID) (Task, error)
//	    History(context.Context, tenancy.Scope, id.TaskID, HistoryQuery) (HistoryPage, error)
//	}
//
//	type CreateUnit struct {
//	    Owner            tenancy.Scope
//	    TaskID           id.TaskID
//	    Title            string
//	    Description      string
//	    Metadata         map[string]string
//	    SelectedPipeline pipeline.PipelineID
//	    Inputs           map[id.InputKey]string
//	    Initiator        id.PrincipalID
//	    At               time.Time
//	    Start            *StartUnit
//	    Receipt          receipt.Record
//	    Audit            []event.Audit
//	    Events           []event.Envelope
//	    Authority        []authorize.AuthorityRead
//	    PostCommit       []event.PostCommitEffect
//	}
//
//	type CreateResult struct {
//	    TaskID           id.TaskID
//	    ExecutionID      id.ExecutionID
//	    ContentVersion   uint64
//	    LifecycleVersion uint64
//	    Replayed         bool
//	}
//
//	type CreatePort interface {
//	    Create(context.Context, CreateUnit) (CreateResult, error)
//	}
//
//	type EditKind string
//	const (
//	    EditContent        EditKind = "content"
//	    EditComment        EditKind = "comment"
//	    EditSelectPipeline EditKind = "select_pipeline"
//	)
//
//	type EditUnit struct {
//	    Owner                    tenancy.Scope
//	    TaskID                   id.TaskID
//	    Kind                     EditKind
//	    ExpectedContentVersion   uint64
//	    ExpectedLifecycleVersion uint64
//	    Title                    string
//	    Description              string
//	    Metadata                 map[string]string
//	    Message                  *Message
//	    PipelineID               pipeline.PipelineID
//	    Inputs                   map[id.InputKey]string
//	    StartAuthority           id.PrincipalID
//	    ExpectedPipelineVersion  uint64
//	    Receipt                  receipt.Record
//	    Audit                    []event.Audit
//	    Events                   []event.Envelope
//	    Authority                []authorize.AuthorityRead
//	    PostCommit               []event.PostCommitEffect
//	}
//
//	type EditResult struct {
//	    ContentVersion   uint64
//	    LifecycleVersion uint64
//	    Replayed         bool
//	}
//
//	type EditPort interface {
//	    Edit(context.Context, EditUnit) (EditResult, error)
//	}
//
//	type ProgressOutcome string
//	const (
//	    ProgressSucceeded ProgressOutcome = "succeeded"
//	    ProgressFailed    ProgressOutcome = "failed"
//	)
//
//	type ProgressCommand struct {
//	    Scope       tenancy.Scope
//	    TaskID      id.TaskID
//	    ExecutionID id.ExecutionID
//	    Outcome     ProgressOutcome
//	}
//
//	type ProgressUnit struct {
//	    Owner                    tenancy.Scope
//	    TaskID                   id.TaskID
//	    ExecutionID              id.ExecutionID
//	    ExpectedLifecycleVersion uint64
//	    At                       time.Time
//	}
//
//	type ProgressResult struct {
//	    LifecycleVersion uint64
//	    Replayed         bool
//	}
//
//	type ProgressPort interface {
//	    Apply(context.Context, ProgressUnit) (ProgressResult, error)
//	}
//
//	type ProgressStatus struct {
//	    State    State
//	    Applied  bool
//	    Replayed bool
//	}
//
//	type CreateCommand struct {
//	    Actor       authorize.Principal
//	    Scope       tenancy.Scope
//	    Title       string
//	    Description string
//	    Metadata    []byte
//	    PipelineID  pipeline.PipelineID
//	    Inputs      map[id.InputKey]string
//	    Start       bool
//	    CallerKey   string
//	    Content     receipt.Content
//	}
//
//	type EditCommand struct {
//	    Actor                    authorize.Principal
//	    Scope                    tenancy.Scope
//	    TaskID                   id.TaskID
//	    Kind                     EditKind
//	    ExpectedContentVersion   uint64
//	    ExpectedLifecycleVersion uint64
//	    Title                    string
//	    Description              string
//	    Metadata                 []byte
//	    Body                     string
//	    References               []string
//	    PipelineID               pipeline.PipelineID
//	    Inputs                   map[id.InputKey]string
//	    CallerKey                string
//	    Content                  receipt.Content
//	}
//
//	type StartCommand struct {
//	    Actor                    authorize.Principal
//	    Scope                    tenancy.Scope
//	    TaskID                   id.TaskID
//	    ExpectedLifecycleVersion uint64
//	    CallerKey                string
//	    Content                  receipt.Content
//	}
//
//	type AskCommand struct {
//	    Actor                    authorize.Principal
//	    Scope                    tenancy.Scope
//	    TaskID                   id.TaskID
//	    ExpectedLifecycleVersion uint64
//	    Question                 string
//	    Assignment               request.Assignment
//	    References               []string
//	    CallerKey                string
//	    Content                  receipt.Content
//	}
//
//	type ReplyCommand struct {
//	    Actor                    authorize.Principal
//	    Scope                    tenancy.Scope
//	    TaskID                   id.TaskID
//	    ExpectedLifecycleVersion uint64
//	    QuestionID               QuestionID
//	    ExpectedQuestionVersion  uint64
//	    Answer                   string
//	    CallerKey                string
//	    Content                  receipt.Content
//	}
//
//	type CancelCommand struct {
//	    Actor                    authorize.Principal
//	    Scope                    tenancy.Scope
//	    TaskID                   id.TaskID
//	    ExpectedLifecycleVersion uint64
//	    CallerKey                string
//	    Content                  receipt.Content
//	}
//
//	type HistoryCommand struct {
//	    Actor  authorize.Principal
//	    Scope  tenancy.Scope
//	    TaskID id.TaskID
//	    Limit  int
//	    Cursor string
//	}
//
//	type ServiceConfig struct {
//	    Clock          Clock
//	    IDs            IDSource
//	    Authorizer     authorize.Authorizer
//	    Authority      authorize.AuthorityReader
//	    Principals     identity.PrincipalReader
//	    Pipelines      PipelineReader
//	    Reader         Reader
//	    Executions     ExecutionReader
//	    Admission      Admission
//	    Creates        CreatePort
//	    Edits          EditPort
//	    Starts         StartPort
//	    Asks           AskPort
//	    Replies        ReplyPort
//	    Cancels        CancelPort
//	    Progress       ProgressPort
//	    DefaultTimeout time.Duration
//	}
//
//	type Service struct{ /* ... */ }
//	func NewService(ServiceConfig) (*Service, error)
//	func (*Service) Create(context.Context, CreateCommand) (CreateResult, error)
//	func (*Service) Edit(context.Context, EditCommand) (EditResult, error)
//	func (*Service) Start(context.Context, StartCommand) (StartResult, error)
//	func (*Service) Ask(context.Context, AskCommand) (AskResult, error)
//	func (*Service) Reply(context.Context, ReplyCommand) (ReplyResult, error)
//	func (*Service) Cancel(context.Context, CancelCommand) (CancelResult, error)
//	func (*Service) Progress(context.Context, ProgressCommand) (ProgressStatus, error)
//	func (*Service) History(context.Context, HistoryCommand) (HistoryPage, error)
//
// Pinned rules:
//
//   - The Service authorizes, validates input, admits work and builds one
//     complete commit unit. Atomic state/version preconditions travel in the
//     unit and are re-checked by the port inside the commit. The Service
//     confirms current membership, principal enablement and the initiator's
//     current start authority before the port, but it does NOT pre-confirm
//     versioned reads (task_version, execution_version, pipeline_version):
//     those stay in Authority for the port, so a lost-response retry reaches
//     the port's receipt-first replay instead of failing early.
//   - Actor.Scope must equal the command Scope and the read row's scope. A
//     mismatch or another scope's task id is refused (forbidden/not_found)
//     before any admission or port call.
//   - task.create starts nothing: Start is nil, no admission, no
//     execution.queued event, no dispatch, and the result has no execution
//     id. task.create_and_start carries one *StartUnit in CreateUnit.Start and
//     commits creation plus admission in one CreatePort call; StartPort is
//     never called for it. The single operation record (Receipt/Audit/Events/
//     Authority/PostCommit) lives on CreateUnit; the nested StartUnit carries
//     only admission content (PipelineID, ExpectedLifecycleVersion 0,
//     ExpectedPipelineVersion, Snapshot, Actions, Initiator,
//     ExecutionPrincipal, NewExecutionID, At) and its own Receipt/Audit/
//     Events/Authority/PostCommit stay zero.
//   - Metadata is one JSON object of string values of at most 16 KiB
//     (16<<10), parsed into map[string]string and stored without interpreting
//     keys or values. Invalid JSON, a non-object, a non-string value, a
//     duplicate key and oversize input are invalid_input. Metadata never
//     changes state, selection, execution or admission behavior.
//   - Edit kinds: content (title/description/metadata; ExpectedContentVersion
//     set, ExpectedLifecycleVersion zero, Message and PipelineID absent, no
//     lifecycle event); comment (Message appended with the actor/time/body/
//     references; ExpectedContentVersion set, no lifecycle change);
//     select_pipeline (ExpectedLifecycleVersion set, PipelineID/Inputs/
//     StartAuthority recorded, ExpectedContentVersion zero, no content
//     fields). Content and comment edits can never start, answer, replace the
//     execution or change the pipeline selection. Every edit unit carries one
//     receipt and one audit record, but no Events and no PostCommit publish:
//     no edit event type is declared.
//   - Start requires an open task and a selected pipeline; a missing selection
//     is invalid_input. waiting/done/cancelled is version_conflict. A
//     nonterminal current execution is left
//     to the port's version/receipt checks so a lost-response retry can reach
//     replay. Dispatch is emitted only when the predecessor execution's stop
//     evidence is confirmed (or there is no predecessor); otherwise the
//     queued execution remains the durable dispatch record without a
//     dispatch effect.
//   - Ask appends the question, sets waiting, cancels the nonterminal current
//     execution at its expected version, writes a revoked (absent) credential
//     and never creates a request action. A different ask while waiting is
//     refused by the port; an identical retry replays.
//   - Reply requires the named outstanding question and expected question
//     version for a new key; a waiting task is checked here, while an open
//     task is treated as a possible lost-response retry and still reaches the
//     port. cancelled/done tasks are refused. Assignment is enforced: role/
//     <role> only by a human currently holding that role, service/<id> only by
//     that exact service principal. The initiator's current start authority
//     (the task.start grant re-read through Principals) is confirmed before
//     admission and carried in Authority. A revoked initiator or an admission
//     failure commits nothing. The receipt records the new execution id and
//     the snapshot hash; the conversation snapshot carries the answer.
//   - Cancel cancels the nonterminal current execution at its expected
//     version and writes a revoked credential; state/version checks stay in
//     the port.
//   - Progress is current-only. A succeeded observation completes the task
//     only when the task is open and the observation names CurrentExecution;
//     otherwise Applied is false and no port call is made. A failed
//     observation leaves the task open with no write. Older/duplicate success
//     can never overwrite waiting/cancelled/newer work. No stored
//     progress/start-intent/request entity exists.
//   - History is an authorized task.history read through Reader.History; the
//     stable cursor and limit pass through unchanged.
//   - Every unit carries exactly one receipt for its operation and caller
//     key, one audit record naming the operation, and the events asserted by
//     the tests. PostCommit publishes when the unit has an event and
//     dispatches only for a newly queued execution with confirmed predecessor
//     stop.
//   - Time, ids and randomness are injected. Tests use deterministic fakes and
//     explicit barriers; production IDSource uses secure randomness.

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Compile-time pins for the added interfaces and method signatures.
var (
	_ task.Clock           = fixedClock{}
	_ task.IDSource        = (*sequentialIDs)(nil)
	_ task.Admission       = (*fakeAdmission)(nil)
	_ task.PipelineReader  = (*fakePipelineReader)(nil)
	_ task.ExecutionReader = (*fakeExecutionReader)(nil)
	_ task.Reader          = (*fakeReader)(nil)
	_ task.CreatePort      = (*fakePorts)(nil)
	_ task.EditPort        = (*fakePorts)(nil)
	_ task.StartPort       = (*fakePorts)(nil)
	_ task.AskPort         = (*fakePorts)(nil)
	_ task.ReplyPort       = (*fakePorts)(nil)
	_ task.CancelPort      = (*fakePorts)(nil)
	_ task.ProgressPort    = (*fakePorts)(nil)

	_ func(task.ServiceConfig) (*task.Service, error)                                         = task.NewService
	_ func(*task.Service, context.Context, task.CreateCommand) (task.CreateResult, error)     = (*task.Service).Create
	_ func(*task.Service, context.Context, task.EditCommand) (task.EditResult, error)         = (*task.Service).Edit
	_ func(*task.Service, context.Context, task.StartCommand) (task.StartResult, error)       = (*task.Service).Start
	_ func(*task.Service, context.Context, task.AskCommand) (task.AskResult, error)           = (*task.Service).Ask
	_ func(*task.Service, context.Context, task.ReplyCommand) (task.ReplyResult, error)       = (*task.Service).Reply
	_ func(*task.Service, context.Context, task.CancelCommand) (task.CancelResult, error)     = (*task.Service).Cancel
	_ func(*task.Service, context.Context, task.ProgressCommand) (task.ProgressStatus, error) = (*task.Service).Progress
	_ func(*task.Service, context.Context, task.HistoryCommand) (task.HistoryPage, error)     = (*task.Service).History
)

// Pinned struct shapes. Keyed composite literals make a missing or renamed
// field a compile-time failure here instead of a silent behavioral gap.
var (
	_ = task.CreateUnit{
		Owner:            tenancy.Scope{},
		TaskID:           id.TaskID(""),
		Title:            "",
		Description:      "",
		Metadata:         map[string]string{},
		SelectedPipeline: pipeline.PipelineID(""),
		Inputs:           map[id.InputKey]string{},
		Initiator:        id.PrincipalID(""),
		At:               time.Time{},
		Start:            (*task.StartUnit)(nil),
		Receipt:          receipt.Record{},
		Audit:            []event.Audit{},
		Events:           []event.Envelope{},
		Authority:        []authorize.AuthorityRead{},
		PostCommit:       []event.PostCommitEffect{},
	}
	_ = task.CreateResult{
		TaskID:           id.TaskID(""),
		ExecutionID:      id.ExecutionID(""),
		ContentVersion:   0,
		LifecycleVersion: 0,
		Replayed:         false,
	}
	_ = task.EditUnit{
		Owner:                    tenancy.Scope{},
		TaskID:                   id.TaskID(""),
		Kind:                     task.EditContent,
		ExpectedContentVersion:   0,
		ExpectedLifecycleVersion: 0,
		Title:                    "",
		Description:              "",
		Metadata:                 map[string]string{},
		Message:                  (*task.Message)(nil),
		PipelineID:               pipeline.PipelineID(""),
		Inputs:                   map[id.InputKey]string{},
		StartAuthority:           id.PrincipalID(""),
		ExpectedPipelineVersion:  0,
		Receipt:                  receipt.Record{},
		Audit:                    []event.Audit{},
		Events:                   []event.Envelope{},
		Authority:                []authorize.AuthorityRead{},
		PostCommit:               []event.PostCommitEffect{},
	}
	_ = task.EditResult{ContentVersion: 0, LifecycleVersion: 0, Replayed: false}
	_ = task.ProgressUnit{
		Owner:                    tenancy.Scope{},
		TaskID:                   id.TaskID(""),
		ExecutionID:              id.ExecutionID(""),
		ExpectedLifecycleVersion: 0,
		At:                       time.Time{},
	}
	_ = task.ProgressResult{LifecycleVersion: 0, Replayed: false}
	_ = task.ProgressStatus{State: task.StateOpen, Applied: false, Replayed: false}
	_ = task.HistoryEntry{
		Kind:        task.HistoryExecution,
		ExecutionID: id.ExecutionID(""),
		State:       kernel.ExecutionQueued,
		Message:     (*task.Message)(nil),
		Question:    (*task.Question)(nil),
		At:          time.Time{},
	}
	_ = task.HistoryPage{Entries: []task.HistoryEntry{}, NextCursor: ""}
	_ = task.HistoryQuery{Limit: 0, Cursor: ""}
	_ = task.ExecutionView{
		ID:           id.ExecutionID(""),
		Version:      0,
		State:        kernel.ExecutionQueued,
		StopEvidence: kernel.PendingStop(""),
	}
	_ = task.CreateCommand{
		Actor:       authorize.Principal{},
		Scope:       tenancy.Scope{},
		Title:       "",
		Description: "",
		Metadata:    nil,
		PipelineID:  pipeline.PipelineID(""),
		Inputs:      map[id.InputKey]string{},
		Start:       false,
		CallerKey:   "",
		Content:     receipt.Content{},
	}
	_ = task.EditCommand{
		Actor:                    authorize.Principal{},
		Scope:                    tenancy.Scope{},
		TaskID:                   id.TaskID(""),
		Kind:                     task.EditSelectPipeline,
		ExpectedContentVersion:   0,
		ExpectedLifecycleVersion: 0,
		Title:                    "",
		Description:              "",
		Metadata:                 nil,
		Body:                     "",
		References:               []string{},
		PipelineID:               pipeline.PipelineID(""),
		Inputs:                   map[id.InputKey]string{},
		CallerKey:                "",
		Content:                  receipt.Content{},
	}
	_ = task.StartCommand{
		Actor:                    authorize.Principal{},
		Scope:                    tenancy.Scope{},
		TaskID:                   id.TaskID(""),
		ExpectedLifecycleVersion: 0,
		CallerKey:                "",
		Content:                  receipt.Content{},
	}
	_ = task.AskCommand{
		Actor:                    authorize.Principal{},
		Scope:                    tenancy.Scope{},
		TaskID:                   id.TaskID(""),
		ExpectedLifecycleVersion: 0,
		Question:                 "",
		Assignment:               request.Assignment{},
		References:               []string{},
		CallerKey:                "",
		Content:                  receipt.Content{},
	}
	_ = task.ReplyCommand{
		Actor:                    authorize.Principal{},
		Scope:                    tenancy.Scope{},
		TaskID:                   id.TaskID(""),
		ExpectedLifecycleVersion: 0,
		QuestionID:               task.QuestionID(""),
		ExpectedQuestionVersion:  0,
		Answer:                   "",
		CallerKey:                "",
		Content:                  receipt.Content{},
	}
	_ = task.CancelCommand{
		Actor:                    authorize.Principal{},
		Scope:                    tenancy.Scope{},
		TaskID:                   id.TaskID(""),
		ExpectedLifecycleVersion: 0,
		CallerKey:                "",
		Content:                  receipt.Content{},
	}
	_ = task.HistoryCommand{
		Actor:  authorize.Principal{},
		Scope:  tenancy.Scope{},
		TaskID: id.TaskID(""),
		Limit:  0,
		Cursor: "",
	}
	_ = task.ServiceConfig{
		Clock:          fixedClock{},
		IDs:            (*sequentialIDs)(nil),
		Authorizer:     (*fakeAuthorizer)(nil),
		Authority:      (*fakeAuthority)(nil),
		Principals:     (*fakePrincipals)(nil),
		Pipelines:      (*fakePipelineReader)(nil),
		Reader:         (*fakeReader)(nil),
		Executions:     (*fakeExecutionReader)(nil),
		Admission:      (*fakeAdmission)(nil),
		Creates:        (*fakePorts)(nil),
		Edits:          (*fakePorts)(nil),
		Starts:         (*fakePorts)(nil),
		Asks:           (*fakePorts)(nil),
		Replies:        (*fakePorts)(nil),
		Cancels:        (*fakePorts)(nil),
		Progress:       (*fakePorts)(nil),
		DefaultTimeout: time.Duration(0),
	}
)

// Pinned edit-kind and progress-outcome vocabulary.
var (
	_ = []task.EditKind{task.EditContent, task.EditComment, task.EditSelectPipeline}
	_ = []task.ProgressOutcome{task.ProgressSucceeded, task.ProgressFailed}
	_ = []task.HistoryKind{task.HistoryExecution, task.HistoryMessage, task.HistoryQuestion}
	_ = identity.Principal{}
	_ = definition.ActionKindContainer
)
