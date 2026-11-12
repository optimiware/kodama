// API note.
//
// This file pins the consumer-owned declarations the native-agent runtime adds
// on top of the provider-neutral model/tool types. It consumes the public
// checkpoint reference format and the S06/S09 handoff semantics without
// importing the session/skill packages; those packages remain behind
// consumer-owned ports here.
//
//	const (
//	    CheckpointVersion     = 1
//	    CheckpointRefPrefix   = "kodama-checkpoint/"
//	    CheckpointRefVersion  = "v1"
//	    MaxCheckpointRefBytes = 512
//	    MaxToolArgumentBytes  = 64 << 10
//	    MaxToolResultBytes    = 64 << 10
//	    MaxKnowledgeBytes     = 1 << 20
//	    MaxQuestionBytes      = 4096
//	    MaxModelCalls         = 32
//	    MaxSteps              = 64
//	    DefaultModelCalls     = 16
//	    DefaultSteps          = 32
//	)
//
//	type Limits struct { ModelCalls, Messages, Tokens, Steps int }
//	func DefaultLimits() Limits
//	func (Limits) Validate() error
//
//	type OutcomeStatus string
//	const (
//	    OutcomeSucceeded OutcomeStatus = "succeeded"
//	    OutcomeWaiting   OutcomeStatus = "waiting"
//	    OutcomeFailed    OutcomeStatus = "failed"
//	    OutcomeCancelled OutcomeStatus = "cancelled"
//	    OutcomeUncertain OutcomeStatus = "uncertain"
//	    OutcomeRefused   OutcomeStatus = "refused"
//	)
//	func (OutcomeStatus) Valid() bool
//
//	type RunInput struct {
//	    TaskID, ExecutionID string
//	    TaskVersion uint64
//	    Model string
//	    Messages []Message
//	    Knowledge []string
//	    Tools []string
//	    Limits Limits
//	    RestoreRef string
//	}
//	func (RunInput) Validate() error
//
//	type Outcome struct {
//	    Status OutcomeStatus
//	    Summary string
//	    Usage Usage
//	    CheckpointRef string
//	    Completed, Uncertain []string
//	    FinishReason string
//	    Class fault.Class
//	}
//
//	type Checkpoint struct {
//	    Version int
//	    TaskID, ExecutionID string
//	    Messages []Message
//	    Completed, InFlight, Uncertain []string
//	    Usage Usage
//	    CreatedAt time.Time
//	}
//	func (Checkpoint) Validate() error
//
//	type CheckpointPort interface {
//	    Save(context.Context, Checkpoint) (string, error)
//	    Restore(context.Context, string) (Checkpoint, error)
//	}
//	type KnowledgePort interface { Load(context.Context, string) ([]byte, error) }
//	type PublicClient interface { Invoke(context.Context, api.Invocation) (api.Result, error) }
//
//	type RuntimeConfig struct {
//	    Model ModelPort
//	    Tools ToolPort
//	    Checkpoints CheckpointPort
//	    Knowledge KnowledgePort
//	    Public PublicClient
//	    Clock Clock
//	    RetainRaw bool
//	}
//	func NewRuntime(RuntimeConfig) (*Runtime, error)
//	func (*Runtime) Run(context.Context, RunInput) (Outcome, error)
//	func ValidCheckpointRef(string) bool
//
// Reserved tool names ("task.reply", "request.resolve", "role.escalate") are
// refused even when allowlisted; "task.ask" is a runtime handoff and is never
// forwarded to ToolPort. The task.ask input is a JSON object carrying
// "checkpoint_ref", "question" and "assignee"; the invocation uses the input
// task version as expected_version and a non-empty idempotency key. A
// successful run requires a final "stop" response with non-empty content. Tool
// results larger than MaxToolResultBytes are truncated to that bound before
// entering the conversation.
//
// A durable completion is recorded even when cancellation follows a completed
// effect. When RetainRaw is false (the default) durable checkpoints omit raw
// tool output; the in-memory conversation may still use it.
//
// Bounded outcomes (length, content_filter, malformed response, limits,
// knowledge failure, cancellation, uncertainty) return a nil error and carry
// their reason in Outcome. Refusals and dependency/authorization failures
// return a non-nil fault error whose class is also copied into Outcome.Class.
// Checkpoints never carry credentials, approvals or decisions.
package agent_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/agent"
)

func TestDefaultLimitsAndValidation(t *testing.T) {
	defaults := agent.DefaultLimits()
	if defaults.ModelCalls != agent.DefaultModelCalls || defaults.Steps != agent.DefaultSteps {
		t.Fatalf("DefaultLimits() = %+v; want ModelCalls=%d Steps=%d", defaults, agent.DefaultModelCalls, agent.DefaultSteps)
	}
	if defaults.Messages != agent.MaxMessages || defaults.Tokens != agent.MaxModelTokens {
		t.Fatalf("DefaultLimits() = %+v; want Messages=%d Tokens=%d", defaults, agent.MaxMessages, agent.MaxModelTokens)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatalf("DefaultLimits().Validate() = %v", err)
	}
	if err := (agent.Limits{}).Validate(); err != nil {
		t.Fatalf("zero Limits should mean defaults: %v", err)
	}
	valid := []agent.Limits{
		{ModelCalls: 1, Steps: 1, Messages: 1, Tokens: 1},
		{ModelCalls: agent.MaxModelCalls, Steps: agent.MaxSteps, Messages: agent.MaxMessages, Tokens: agent.MaxModelTokens},
	}
	for _, limits := range valid {
		if err := limits.Validate(); err != nil {
			t.Errorf("Limits%+v refused: %v", limits, err)
		}
	}
	invalid := []agent.Limits{
		{ModelCalls: -1},
		{Steps: -1},
		{Messages: -1},
		{Tokens: -1},
		{ModelCalls: agent.MaxModelCalls + 1},
		{Steps: agent.MaxSteps + 1},
		{Messages: agent.MaxMessages + 1},
		{Tokens: agent.MaxModelTokens + 1},
	}
	for _, limits := range invalid {
		if err := limits.Validate(); err == nil {
			t.Errorf("Limits%+v accepted", limits)
		}
	}
}

func TestOutcomeStatusVocabulary(t *testing.T) {
	valid := []agent.OutcomeStatus{
		agent.OutcomeSucceeded,
		agent.OutcomeWaiting,
		agent.OutcomeFailed,
		agent.OutcomeCancelled,
		agent.OutcomeUncertain,
		agent.OutcomeRefused,
	}
	seen := make(map[agent.OutcomeStatus]bool)
	for _, status := range valid {
		if !status.Valid() {
			t.Errorf("OutcomeStatus(%q).Valid() = false", status)
		}
		if status == "" || seen[status] {
			t.Errorf("OutcomeStatus(%q) is empty or duplicated", status)
		}
		seen[status] = true
	}
	for _, status := range []agent.OutcomeStatus{"", "done", "running", "SUCCEEDED", "error"} {
		if status.Valid() {
			t.Errorf("OutcomeStatus(%q).Valid() = true", status)
		}
	}
}

func TestCheckpointReferenceFormat(t *testing.T) {
	valid := []string{
		"kodama-checkpoint/v1/ctx-0001",
		"kodama-checkpoint/v1/a",
		"kodama-checkpoint/v1/context.2026-01-02_03",
	}
	for _, ref := range valid {
		if !agent.ValidCheckpointRef(ref) {
			t.Errorf("ValidCheckpointRef(%q) = false", ref)
		}
	}
	refusals := []string{
		"",
		"ctx-0001",
		"ctx/0001",
		"kodama-checkpoint/v2/ctx-0001",
		"kodama-checkpoint/v1/",
		"kodama-checkpoint/v1/../ctx",
		"kodama-checkpoint/v1/CTX-0001",
		"/tmp/ctx",
		"./ctx",
		"file:///tmp/ctx",
		"checkpoint/v1/ctx-0001",
		"kodama-checkpoint/v1/" + strings.Repeat("a", agent.MaxCheckpointRefBytes),
	}
	for _, ref := range refusals {
		if agent.ValidCheckpointRef(ref) {
			t.Errorf("ValidCheckpointRef(%q) = true; want refusal", ref)
		}
	}
	if agent.CheckpointRefPrefix != "kodama-checkpoint/" || agent.CheckpointRefVersion != "v1" {
		t.Fatalf("checkpoint prefix/version = %q/%q", agent.CheckpointRefPrefix, agent.CheckpointRefVersion)
	}
	if agent.CheckpointVersion != 1 {
		t.Fatalf("CheckpointVersion = %d; want 1", agent.CheckpointVersion)
	}
}

func TestCheckpointValidationAndShape(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	valid := agent.Checkpoint{
		Version:     agent.CheckpointVersion,
		TaskID:      "tsk-1",
		ExecutionID: "exec-1",
		Messages: []agent.Message{
			{Role: "user", Content: "please continue"},
			{Role: "assistant", Content: "continuing"},
		},
		Completed: []string{"effect-1"},
		Usage:     agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
		CreatedAt: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid checkpoint refused: %v", err)
	}
	invalid := []agent.Checkpoint{
		{Version: 2, TaskID: "tsk-1", ExecutionID: "exec-1", Messages: valid.Messages, CreatedAt: now},
		{Version: agent.CheckpointVersion, ExecutionID: "exec-1", Messages: valid.Messages, CreatedAt: now},
		{Version: agent.CheckpointVersion, TaskID: "tsk-1", Messages: valid.Messages, CreatedAt: now},
		{Version: agent.CheckpointVersion, TaskID: "tsk-1", ExecutionID: "exec-1", Messages: []agent.Message{{Role: "wizard", Content: "x"}}, CreatedAt: now},
		{Version: agent.CheckpointVersion, TaskID: "tsk-1", ExecutionID: "exec-1", Messages: valid.Messages},
		{Version: agent.CheckpointVersion, TaskID: "tsk-1", ExecutionID: "exec-1", Messages: valid.Messages, Completed: []string{""}, CreatedAt: now},
		{Version: agent.CheckpointVersion, TaskID: "tsk-1", ExecutionID: "exec-1", Messages: valid.Messages, Completed: []string{"same"}, Uncertain: []string{"same"}, CreatedAt: now},
		{Version: agent.CheckpointVersion, TaskID: "tsk-1", ExecutionID: "exec-1", Messages: valid.Messages, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 3}, CreatedAt: now},
	}
	for index, checkpoint := range invalid {
		if err := checkpoint.Validate(); err == nil {
			t.Errorf("invalid checkpoint %d accepted: %+v", index, checkpoint)
		}
	}

	// The durable checkpoint is context, not authority or secret material.
	checkpointType := reflect.TypeFor[agent.Checkpoint]()
	for field := range checkpointType.Fields() {
		name := strings.ToLower(field.Name)
		for _, forbidden := range []string{"credential", "token", "secret", "password", "approval", "decision", "authorization", "authority", "role"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("Checkpoint field %q carries %q", field.Name, forbidden)
			}
		}
	}
}

func TestRunInputValidation(t *testing.T) {
	valid := agent.RunInput{
		TaskID:      "tsk-1",
		ExecutionID: "exec-1",
		TaskVersion: 3,
		Model:       "fixture-model",
		Messages:    []agent.Message{{Role: "user", Content: "do it"}},
		Tools:       []string{"repo.read"},
		Knowledge:   []string{"context/skills/review.md"},
		Limits:      agent.DefaultLimits(),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid RunInput refused: %v", err)
	}
	if err := (agent.RunInput{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m", Messages: valid.Messages, RestoreRef: "kodama-checkpoint/v1/ctx-1"}).Validate(); err != nil {
		t.Fatalf("restoring RunInput refused: %v", err)
	}
	invalid := []agent.RunInput{
		{ExecutionID: "exec-1", Model: "m", Messages: valid.Messages},
		{TaskID: "tsk-1", Model: "m", Messages: valid.Messages},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Messages: valid.Messages},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m"},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m", Messages: []agent.Message{{Role: "wizard", Content: "x"}}},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m", Messages: valid.Messages, Tools: []string{"Repo.Read"}},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m", Messages: valid.Messages, Knowledge: []string{"/etc/passwd"}},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m", Messages: valid.Messages, Knowledge: []string{"../ctx"}},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m", Messages: valid.Messages, RestoreRef: "ctx-1"},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m", Messages: valid.Messages, Limits: agent.Limits{ModelCalls: -1}},
		{TaskID: "tsk-1", ExecutionID: "exec-1", Model: "m", Messages: []agent.Message{{Role: "user", Content: "one"}, {Role: "user", Content: "two"}}, Limits: agent.Limits{Messages: 1}},
	}
	for index, input := range invalid {
		if err := input.Validate(); err == nil {
			t.Errorf("invalid RunInput %d accepted: %+v", index, input)
		}
	}
}

func TestNewRuntimeRequiresBoundaries(t *testing.T) {
	complete := agent.RuntimeConfig{
		Model:       &fakeModel{},
		Tools:       &fakeTool{},
		Checkpoints: newFakeCheckpoints(),
		Public:      &fakePublic{},
		Clock:       fakeClock{at: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
	}
	if _, err := agent.NewRuntime(complete); err != nil {
		t.Fatalf("NewRuntime refused a complete config: %v", err)
	}
	cases := map[string]agent.RuntimeConfig{
		"model":       {Tools: complete.Tools, Checkpoints: complete.Checkpoints, Public: complete.Public, Clock: complete.Clock},
		"tools":       {Model: complete.Model, Checkpoints: complete.Checkpoints, Public: complete.Public, Clock: complete.Clock},
		"checkpoints": {Model: complete.Model, Tools: complete.Tools, Public: complete.Public, Clock: complete.Clock},
		"public":      {Model: complete.Model, Tools: complete.Tools, Checkpoints: complete.Checkpoints, Clock: complete.Clock},
		"clock":       {Model: complete.Model, Tools: complete.Tools, Checkpoints: complete.Checkpoints, Public: complete.Public},
	}
	for name, config := range cases {
		if _, err := agent.NewRuntime(config); err == nil {
			t.Errorf("NewRuntime accepted a config without %s", name)
		}
	}
}

// TestRuntimeHasNoAutonomousAuthority proves the runtime surface cannot resolve
// requests, answer tasks or escalate roles, and that the checkpoint carries no
// approval/decision state a fresh execution could inherit.
func TestRuntimeHasNoAutonomousAuthority(t *testing.T) {
	runtimeType := reflect.TypeFor[*agent.Runtime]()
	foundRun := false
	for method := range runtimeType.Methods() {
		name := method.Name
		if name == "Run" {
			foundRun = true
		}
		lower := strings.ToLower(name)
		for _, forbidden := range []string{"resolve", "reply", "ask", "approve", "escalate", "grant", "authorize"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("Runtime exposes autonomous-authority method %q", name)
			}
		}
	}
	if !foundRun {
		t.Fatal("Runtime does not expose Run")
	}

	// A Checkpoint marshals to durable context only. No approval, decision or
	// authority vocabulary may appear in its JSON shape.
	payload, err := json.Marshal(agent.Checkpoint{
		Version:     agent.CheckpointVersion,
		TaskID:      "tsk-1",
		ExecutionID: "exec-1",
		Messages:    []agent.Message{{Role: "user", Content: "continue"}},
		CreatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	text := strings.ToLower(string(payload))
	for _, forbidden := range []string{"approval", "decision", "authorization", "authority", "credential", "token", "secret"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("checkpoint JSON contains %q: %s", forbidden, text)
		}
	}
}
