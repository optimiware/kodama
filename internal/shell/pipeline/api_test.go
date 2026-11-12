package pipeline_test

// Pipeline administration and admission API pinned by these tests.
//
// The names and signatures below are the exact API the behavioral tests in this
// directory exercise.
//
//	type ChangeKind string
//	const (
//	    ChangeApply  ChangeKind = "apply"
//	    ChangeUpdate ChangeKind = "update"
//	    ChangeDelete ChangeKind = "delete"
//	)
//
//	type ChangeUnit struct {
//	    Owner           tenancy.Scope
//	    Kind            ChangeKind
//	    PipelineID      PipelineID
//	    Name            string
//	    YAML            []byte
//	    ExpectedVersion uint64
//	    Receipt         receipt.Record
//	    Audit           []event.Audit
//	    Events          []event.Envelope
//	    Authority       []authorize.AuthorityRead
//	    PostCommit      []event.PostCommitEffect
//	}
//
//	type ChangeResult struct {
//	    PipelineID PipelineID
//	    Version    uint64
//	    Replayed   bool
//	}
//
//	type ChangePort interface {
//	    Change(context.Context, ChangeUnit) (ChangeResult, error)
//	}
//
//	type Definition struct {
//	    ID      PipelineID
//	    Name    string
//	    YAML    []byte
//	    Version uint64
//	    Push    *PushTrigger
//	}
//
//	type Reader interface {
//	    Get(context.Context, tenancy.Scope, PipelineID) (Definition, error)
//	    ByName(context.Context, tenancy.Scope, string) (Definition, error)
//	    List(context.Context, tenancy.Scope) ([]Definition, error)
//	}
//
//	func Decode(yaml []byte, limits Limits) (definition.Pipeline, error)
//
//	type AdmittedAction struct {
//	    Index     int
//	    Name      id.ActionName
//	    Kind      definition.ActionKind
//	    Timeout   time.Duration
//	    Container *definition.Container
//	    Request   *definition.Request
//	}
//	func (AdmittedAction) Validate() error
//
//	type TaskContext struct {
//	    TaskID       id.TaskID
//	    Conversation []byte
//	    References   []string
//	    Metadata     []byte
//	}
//
//	type AdmitRequest struct {
//	    Scope          tenancy.Scope
//	    Definition     Definition
//	    Inputs         map[id.InputKey]string
//	    Requirements   []string
//	    Task           TaskContext
//	    DefaultTimeout time.Duration
//	}
//
//	type AdmissionResult struct {
//	    PipelineID      PipelineID
//	    PipelineVersion uint64
//	    Snapshot        Snapshot
//	    Actions         []AdmittedAction
//	}
//
//	type AdmissionConfig struct {
//	    Resolver Resolver
//	    Reader   Reader
//	}
//
//	type AdmissionService struct{ /* ... */ }
//	func NewAdmissionService(AdmissionConfig) (*AdmissionService, error)
//	func (*AdmissionService) Admit(context.Context, AdmitRequest) (AdmissionResult, error)
//
//	type Clock interface { Now() time.Time }
//	type IDSource interface {
//	    NewEventID() (id.EventID, error)
//	    NewReceiptID() (id.ReceiptID, error)
//	}
//
//	type ApplyCommand struct {
//	    Actor     authorize.Principal
//	    Scope     tenancy.Scope
//	    Name      string
//	    YAML      []byte
//	    CallerKey string
//	    Content   receipt.Content
//	}
//
//	type UpdateCommand struct {
//	    Actor           authorize.Principal
//	    Scope           tenancy.Scope
//	    PipelineID      PipelineID
//	    Name            string
//	    YAML            []byte
//	    ExpectedVersion uint64
//	    CallerKey       string
//	    Content         receipt.Content
//	}
//
//	type DeleteCommand struct {
//	    Actor           authorize.Principal
//	    Scope           tenancy.Scope
//	    PipelineID      PipelineID
//	    ExpectedVersion uint64
//	    CallerKey       string
//	    Content         receipt.Content
//	}
//
//	type ReadCommand struct {
//	    Actor      authorize.Principal
//	    Scope      tenancy.Scope
//	    PipelineID PipelineID
//	}
//
//	type ListCommand struct {
//	    Actor authorize.Principal
//	    Scope tenancy.Scope
//	}
//
//	type ManagementConfig struct {
//	    Clock      Clock
//	    IDs        IDSource
//	    Authorizer authorize.Authorizer
//	    Authority  authorize.AuthorityReader
//	    Changes    ChangePort
//	    Reader     Reader
//	}
//
//	type Management struct{ /* ... */ }
//	func NewManagement(ManagementConfig) (*Management, error)
//	func (*Management) Apply(context.Context, ApplyCommand) (ChangeResult, error)
//	func (*Management) Update(context.Context, UpdateCommand) (ChangeResult, error)
//	func (*Management) Delete(context.Context, DeleteCommand) (ChangeResult, error)
//	func (*Management) Read(context.Context, ReadCommand) (Definition, error)
//	func (*Management) List(context.Context, ListCommand) ([]Definition, error)
//
// Pinned YAML authoring encoding (S03 §1–2/§4, KODAMA.md examples):
//
//   - A pipeline document is a mapping with keys inputs, env, actions and the
//     optional repository, service and on.push. Unknown keys are refused.
//   - inputs is a sequence. A scalar item is a required string input; a mapping
//     item is {key, kind?, required?} where kind is string|number|boolean
//     (default string) and required defaults to false. Declaration order is
//     preserved.
//   - env is a mapping of literal string values. Names use id.EnvName and the
//     reserved KODAMA_* names are refused. Action env overrides pipeline env.
//   - actions is a nonempty ordered sequence. Each action has name, exactly one
//     of image (container) or request (request), optional commands, env,
//     secrets, timeout and outputs; a request action also has subject.
//   - timeout is a Go duration string. An absent timeout is resolved from the
//     installation default at admission. Explicit and default durations must be
//     finite and inside [1m, 720h].
//   - subject is a mapping of field name to a literal string or to exactly
//     {from: <path>}, where <path> is inputs.<key> or
//     <earlier-action>.outputs.<key>.
//   - repository/service are resource/service ids. on is {push: [refs...]} with
//     exact refs only; on.push requires repository and service and restricts
//     declared inputs to repository, ref, commit and previous_commit.
//   - Duplicate keys, unsupported tags/anchors/aliases/merge keys, malformed
//     YAML, mixed literal/reference forms, unknown/self/forward references and
//     undeclared outputs are refused with fault.InvalidInput.
//
// The YAML parser bound set is the declared pipeline.DefaultLimits: 1 MiB of
// bytes, depth 64 (root mapping is depth 1) and zero alias expansion.
//
// Task metadata is one JSON object of string values at most 16 KiB
// (16<<10). Task conversation bytes are bounded at 64 KiB (64<<10), the event
// data bound. Snapshot.Metadata is a map[string]string.

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Compile-time pins for the added interfaces and method signatures.
var (
	_ pipeline.ChangePort = (*fakeChangePort)(nil)
	_ pipeline.Reader     = (*fakeReader)(nil)
	_ pipeline.Resolver   = (*fakeResolver)(nil)
	_ pipeline.Clock      = fixedClock{}
	_ pipeline.IDSource   = (*sequentialIDs)(nil)

	_ func([]byte, pipeline.Limits) (definition.Pipeline, error) = pipeline.Decode
	_ func(pipeline.AdmittedAction) error                        = pipeline.AdmittedAction.Validate

	_ func(pipeline.AdmissionConfig) (*pipeline.AdmissionService, error)                                         = pipeline.NewAdmissionService
	_ func(*pipeline.AdmissionService, context.Context, pipeline.AdmitRequest) (pipeline.AdmissionResult, error) = (*pipeline.AdmissionService).Admit

	_ func(pipeline.ManagementConfig) (*pipeline.Management, error)                                      = pipeline.NewManagement
	_ func(*pipeline.Management, context.Context, pipeline.ApplyCommand) (pipeline.ChangeResult, error)  = (*pipeline.Management).Apply
	_ func(*pipeline.Management, context.Context, pipeline.UpdateCommand) (pipeline.ChangeResult, error) = (*pipeline.Management).Update
	_ func(*pipeline.Management, context.Context, pipeline.DeleteCommand) (pipeline.ChangeResult, error) = (*pipeline.Management).Delete
	_ func(*pipeline.Management, context.Context, pipeline.ReadCommand) (pipeline.Definition, error)     = (*pipeline.Management).Read
	_ func(*pipeline.Management, context.Context, pipeline.ListCommand) ([]pipeline.Definition, error)   = (*pipeline.Management).List
)

// Pinned struct shapes. Composite literals make a missing or renamed field a
// compile-time failure in this file rather than a silent behavioral gap.
var (
	_ = pipeline.ChangeUnit{
		Owner:           tenancy.Scope{},
		Kind:            pipeline.ChangeApply,
		PipelineID:      pipeline.PipelineID(""),
		Name:            "",
		YAML:            nil,
		ExpectedVersion: 0,
		Receipt:         receipt.Record{},
		Audit:           []event.Audit{},
		Events:          []event.Envelope{},
		Authority:       []authorize.AuthorityRead{},
		PostCommit:      []event.PostCommitEffect{},
	}
	_ = pipeline.ChangeResult{PipelineID: pipeline.PipelineID(""), Version: 0, Replayed: false}
	_ = pipeline.Definition{ID: pipeline.PipelineID(""), Name: "", YAML: nil, Version: 0, Push: (*pipeline.PushTrigger)(nil)}
	_ = pipeline.AdmittedAction{
		Index:     0,
		Name:      id.ActionName(""),
		Kind:      definition.ActionKind(""),
		Timeout:   time.Duration(0),
		Container: (*definition.Container)(nil),
		Request:   (*definition.Request)(nil),
	}
	_ = pipeline.TaskContext{TaskID: id.TaskID(""), Conversation: nil, References: nil, Metadata: nil}
	_ = pipeline.AdmitRequest{
		Scope:          tenancy.Scope{},
		Definition:     pipeline.Definition{},
		Inputs:         map[id.InputKey]string{},
		Requirements:   []string{},
		Task:           pipeline.TaskContext{},
		DefaultTimeout: time.Duration(0),
	}
	_ = pipeline.AdmissionResult{
		PipelineID:      pipeline.PipelineID(""),
		PipelineVersion: 0,
		Snapshot:        pipeline.Snapshot{},
		Actions:         []pipeline.AdmittedAction{},
	}
	_ = pipeline.AdmissionConfig{Resolver: (*fakeResolver)(nil), Reader: (*fakeReader)(nil)}
	_ = pipeline.ManagementConfig{
		Clock:      fixedClock{},
		IDs:        (*sequentialIDs)(nil),
		Authorizer: (*fakeAuthorizer)(nil),
		Authority:  (*fakeAuthorityReader)(nil),
		Changes:    (*fakeChangePort)(nil),
		Reader:     (*fakeReader)(nil),
	}
	_ = pipeline.ApplyCommand{
		Actor:     authorize.Principal{},
		Scope:     tenancy.Scope{},
		Name:      "",
		YAML:      nil,
		CallerKey: "",
		Content:   receipt.Content{},
	}
	_ = pipeline.UpdateCommand{
		Actor:           authorize.Principal{},
		Scope:           tenancy.Scope{},
		PipelineID:      pipeline.PipelineID(""),
		Name:            "",
		YAML:            nil,
		ExpectedVersion: 0,
		CallerKey:       "",
		Content:         receipt.Content{},
	}
	_ = pipeline.DeleteCommand{
		Actor:           authorize.Principal{},
		Scope:           tenancy.Scope{},
		PipelineID:      pipeline.PipelineID(""),
		ExpectedVersion: 0,
		CallerKey:       "",
		Content:         receipt.Content{},
	}
	_ = pipeline.ReadCommand{Actor: authorize.Principal{}, Scope: tenancy.Scope{}, PipelineID: pipeline.PipelineID("")}
	_ = pipeline.ListCommand{Actor: authorize.Principal{}, Scope: tenancy.Scope{}}
)
