package task_test

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func editCommand(actor authorize.Principal, kind task.EditKind) task.EditCommand {
	return task.EditCommand{
		Actor:                    actor,
		Scope:                    actor.Scope,
		TaskID:                   taskID,
		Kind:                     kind,
		ExpectedContentVersion:   3,
		ExpectedLifecycleVersion: 4,
		Title:                    "Updated title",
		Description:              "Updated markdown",
		Metadata:                 []byte(`{"stage":"qa","priority":"low"}`),
		Body:                     "A comment",
		References:               []string{"git:def456"},
		PipelineID:               pipelineID,
		Inputs:                   map[id.InputKey]string{"repository": "example/repo"},
		CallerKey:                "caller-edit",
		Content:                  receiptContent(`{"edit":"content"}`),
	}
}

// TestEditContentUpdatesContentOnly proves a content edit carries the content
// version and no lifecycle version, message, pipeline selection, lifecycle
// event or dispatch effect.
func TestEditContentUpdatesContentOnly(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(baseTask())

	command := editCommand(actor, task.EditContent)
	result, err := env.service.Edit(t.Context(), command)
	requireNoError(t, err, "Edit content")

	if result.ContentVersion != 4 {
		t.Fatalf("content version = %d; want 4", result.ContentVersion)
	}
	if env.pipelines.callCount() != 0 {
		t.Fatalf("pipeline reader called for a content edit: %v", env.pipelines.calledIDs())
	}

	unit := env.ports.lastEdit(t)
	if unit.Kind != task.EditContent {
		t.Fatalf("edit kind = %q; want %q", unit.Kind, task.EditContent)
	}
	if unit.Owner != actor.Scope || unit.TaskID != taskID {
		t.Fatalf("edit identity = %+v/%q", unit.Owner, unit.TaskID)
	}
	if unit.ExpectedContentVersion != 3 || unit.ExpectedLifecycleVersion != 0 {
		t.Fatalf("expected versions = %d/%d; want content 3 and lifecycle 0", unit.ExpectedContentVersion, unit.ExpectedLifecycleVersion)
	}
	if unit.Title != "Updated title" || unit.Description != "Updated markdown" {
		t.Fatalf("edited content = %q/%q", unit.Title, unit.Description)
	}
	if unit.Metadata["stage"] != "qa" || unit.Metadata["priority"] != "low" {
		t.Fatalf("edited metadata = %+v", unit.Metadata)
	}
	if unit.Message != nil {
		t.Fatalf("content edit carries a message: %+v", unit.Message)
	}
	if unit.PipelineID != "" || unit.Inputs != nil {
		t.Fatalf("content edit carries a pipeline selection: %q/%+v", unit.PipelineID, unit.Inputs)
	}
	if unit.StartAuthority != "" {
		t.Fatalf("content edit carries a start authority %q", unit.StartAuthority)
	}
	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.edit", command.CallerKey, command.Content, testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.edit", testNow)
	requireNoEvents(t, unit.Events)
	requireNoPostCommit(t, unit.PostCommit)

	requireAuthority(t, unit.Authority, "task.edit", authorize.CheckMembership)
	taskVersion := requireAuthority(t, unit.Authority, "task.edit", authorize.CheckTaskVersion)
	if taskVersion.ExpectedVersion != 0 {
		t.Fatalf("content edit task_version read = %d; want no lifecycle expectation", taskVersion.ExpectedVersion)
	}
	requireNoAuthority(t, unit.Authority, authorize.CheckPipelineVersion)
}

// TestEditCommentAppendsMessageOnly proves a comment advances the content
// version, records the actor/time/body/references and never touches lifecycle
// state, execution or pipeline selection.
func TestEditCommentAppendsMessageOnly(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(baseTask())

	command := editCommand(actor, task.EditComment)
	result, err := env.service.Edit(t.Context(), command)
	requireNoError(t, err, "Edit comment")

	if result.ContentVersion != 4 {
		t.Fatalf("content version = %d; want 4", result.ContentVersion)
	}
	unit := env.ports.lastEdit(t)
	if unit.Kind != task.EditComment {
		t.Fatalf("edit kind = %q; want %q", unit.Kind, task.EditComment)
	}
	if unit.ExpectedContentVersion != 3 || unit.ExpectedLifecycleVersion != 0 {
		t.Fatalf("expected versions = %d/%d; want content 3 and lifecycle 0", unit.ExpectedContentVersion, unit.ExpectedLifecycleVersion)
	}
	if unit.Message == nil {
		t.Fatal("comment edit carries no message")
	}
	if unit.Message.Author != actor.ID || unit.Message.Body != "A comment" {
		t.Fatalf("message = %+v", unit.Message)
	}
	if !unit.Message.At.Equal(testNow) {
		t.Fatalf("message time = %v; want %v", unit.Message.At, testNow)
	}
	if len(unit.Message.References) != 1 || unit.Message.References[0] != "git:def456" {
		t.Fatalf("message references = %+v", unit.Message.References)
	}
	if unit.Title != "" || unit.Description != "" || unit.Metadata != nil {
		t.Fatalf("comment edit carries content fields: %q/%q/%+v", unit.Title, unit.Description, unit.Metadata)
	}
	if unit.PipelineID != "" || unit.Inputs != nil {
		t.Fatalf("comment edit carries a pipeline selection: %q/%+v", unit.PipelineID, unit.Inputs)
	}
	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.comment", command.CallerKey, command.Content, testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.comment", testNow)
	requireNoEvents(t, unit.Events)
	requireNoPostCommit(t, unit.PostCommit)
	requireAuthority(t, unit.Authority, "task.comment", authorize.CheckMembership)
}

// TestEditSelectPipelineRecordsExplicitSelection proves selection is a
// separate versioned lifecycle operation: it records the selecting principal as
// the start authority and the exact pipeline version, without editing content.
func TestEditSelectPipelineRecordsExplicitSelection(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(baseTask())

	command := editCommand(actor, task.EditSelectPipeline)
	command.ExpectedContentVersion = 0
	command.Title = ""
	command.Description = ""
	command.Metadata = nil
	command.Body = ""
	command.References = nil

	result, err := env.service.Edit(t.Context(), command)
	requireNoError(t, err, "Edit select_pipeline")

	if result.LifecycleVersion != 5 {
		t.Fatalf("lifecycle version = %d; want 5", result.LifecycleVersion)
	}
	if result.ContentVersion != 0 {
		t.Fatalf("selection result content version = %d; want no content change", result.ContentVersion)
	}
	if calls := env.pipelines.calledIDs(); len(calls) != 1 || calls[0] != pipelineID {
		t.Fatalf("pipeline reads = %v; want exactly %q", calls, pipelineID)
	}
	unit := env.ports.lastEdit(t)
	if unit.Kind != task.EditSelectPipeline {
		t.Fatalf("edit kind = %q; want %q", unit.Kind, task.EditSelectPipeline)
	}
	if unit.ExpectedLifecycleVersion != 4 || unit.ExpectedContentVersion != 0 {
		t.Fatalf("expected versions = %d/%d; want lifecycle 4 and content 0", unit.ExpectedLifecycleVersion, unit.ExpectedContentVersion)
	}
	if unit.ExpectedPipelineVersion != 3 {
		t.Fatalf("expected pipeline version = %d; want the current pipeline version 3", unit.ExpectedPipelineVersion)
	}
	if unit.PipelineID != pipelineID || unit.Inputs["repository"] != "example/repo" {
		t.Fatalf("selection = %q/%+v", unit.PipelineID, unit.Inputs)
	}
	if unit.StartAuthority != actor.ID {
		t.Fatalf("start authority = %q; want the selecting principal %q", unit.StartAuthority, actor.ID)
	}
	if unit.Title != "" || unit.Description != "" || unit.Metadata != nil || unit.Message != nil {
		t.Fatalf("selection carries content fields: %+v", unit)
	}
	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.select_pipeline", command.CallerKey, command.Content, testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.select_pipeline", testNow)
	requireNoEvents(t, unit.Events)
	requireNoPostCommit(t, unit.PostCommit)

	requireAuthority(t, unit.Authority, "task.select_pipeline", authorize.CheckMembership)
	taskVersion := requireAuthority(t, unit.Authority, "task.select_pipeline", authorize.CheckTaskVersion)
	if taskVersion.ExpectedVersion != 4 {
		t.Fatalf("task_version read = %d; want 4", taskVersion.ExpectedVersion)
	}
	pipelineVersion := requireAuthority(t, unit.Authority, "task.select_pipeline", authorize.CheckPipelineVersion)
	if pipelineVersion.TargetID != string(pipelineID) || pipelineVersion.ExpectedVersion != 3 {
		t.Fatalf("pipeline_version read = %+v; want %q@3", pipelineVersion, pipelineID)
	}
}

// TestEditPropagatesStaleContentVersion proves the service never silently
// accepts a stale content edit: the expected content version travels in the
// unit and the port's version conflict is returned unchanged.
func TestEditPropagatesStaleContentVersion(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(baseTask())
	current := baseTask()
	env.ports.setEdit(func(unit task.EditUnit) (task.EditResult, error) {
		if unit.ExpectedContentVersion != current.ContentVersion {
			return task.EditResult{}, fault.New(fault.VersionConflict, "stale content version")
		}
		return task.EditResult{ContentVersion: current.ContentVersion + 1}, nil
	})

	command := editCommand(actor, task.EditContent)
	command.ExpectedContentVersion = 2
	_, err := env.service.Edit(t.Context(), command)
	requireFaultClass(t, err, fault.VersionConflict)
	if len(env.ports.edits) != 1 {
		t.Fatalf("edit port calls = %d; want the version check at the port", len(env.ports.edits))
	}
	if unit := env.ports.lastEdit(t); unit.ExpectedContentVersion != 2 {
		t.Fatalf("unit expected content version = %d; want 2", unit.ExpectedContentVersion)
	}
}

// TestEditRefusesInvalidMetadataBeforePorts proves edit metadata follows the
// same opaque bounded JSON rules as creation and is validated before any commit.
func TestEditRefusesInvalidMetadataBeforePorts(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(baseTask())

	cases := map[string][]byte{
		"invalid json":  []byte(`{"stage":`),
		"non object":    []byte(`["stage"]`),
		"non string":    []byte(`{"stage":1}`),
		"duplicate key": []byte(`{"stage":"a","stage":"b"}`),
		"oversize":      []byte(`{"k":"` + strings.Repeat("a", 16<<10) + `"}`),
	}
	for name, metadata := range cases {
		t.Run(name, func(t *testing.T) {
			command := editCommand(actor, task.EditContent)
			command.Metadata = metadata
			_, err := env.service.Edit(t.Context(), command)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
	requireNoPortCalls(t, env.ports)
}
