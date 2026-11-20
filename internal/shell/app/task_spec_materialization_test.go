package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/id"
	kernelengine "github.com/optimiware/kodema/internal/kernel/engine"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

// TestTaskSpecMaterializationEndToEnd proves that task title, description, and
// metadata are snapshotted at admission and materialized under /kodema/task/
// alongside /kodema/task.json, stay pinned across later task edits, reflect
// edits on a subsequent reply's fresh execution, and stay completely absent
// for task-free executions.
func TestTaskSpecMaterializationEndToEnd(t *testing.T) {
	t.Run("create_and_start materializes task spec and pins snapshot across edits", func(t *testing.T) {
		fixture := newActivationFixture(t, nil, "first")
		fixture.runtime.pauseExits()
		application := fixture.environment.startApp(t)
		handler := application.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")

		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "first"},
			activationAction{name: "second"},
		), "task-spec-pipeline")

		recorder, result, publicError := restCall(t, handler, http.MethodPost,
			"/api/v1/projects/project-1/tasks/create-and-start", "dev-project-token",
			api.TaskCreateAndStartRequest{
				Title:       "Fix login race",
				Description: "Reproduce under -race -> stabilize token refresh.",
				Metadata:    map[string]string{"z": "last", "a": "first"},
				PipelineID:  applied.PipelineID,
			}, nil, "task-spec-create-and-start")
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
		}
		started := decodeBody[api.TaskCreateAndStartResult](t, result)

		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		assertMaterializedTaskSpec(t, prepares[0], started.TaskID,
			"Fix login race",
			"Reproduce under -race -> stabilize token refresh.",
			`{"a":"first","z":"last"}`,
			"[]",
		)

		// Mutate the task after admission; the already-admitted execution's
		// next action must still observe the admitted snapshot.
		expectedContent := uint64(1)
		editRecorder, _, editErr := restCall(t, handler, http.MethodPatch,
			"/api/v1/tasks/"+started.TaskID, "dev-project-token",
			api.TaskEditRequest{
				Title:       "Mutated title after admission",
				Description: "Mutated description after admission",
				Metadata:    map[string]string{"mutated": "yes"},
			}, &expectedContent, "task-spec-edit-after-admission")
		if editRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.edit status=%d error=%+v", editRecorder.Code, editErr)
		}

		fixture.runtime.releaseExits()
		prepares = waitForActivationPrepares(t, fixture.runtime, 2)
		if prepares[1].ActionName != "second" {
			t.Fatalf("second prepare action = %q; want second", prepares[1].ActionName)
		}
		assertMaterializedTaskSpec(t, prepares[1], started.TaskID,
			"Fix login race",
			"Reproduce under -race -> stabilize token refresh.",
			`{"a":"first","z":"last"}`,
			"[]",
		)
	})

	t.Run("start with empty metadata/description and reply picks up edited task spec", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		application := fixture.environment.startApp(t)
		handler := application.Handler()
		grantRecorder, _, grantErr := restCall(t, handler, http.MethodPut,
			"/api/v1/projects/project-1/memberships/admin-1", "dev-admin-token",
			api.MembershipGrantRequest{Roles: []string{"admin", "engineer", "reviewer"}}, nil, "membership-bootstrap-reviewer")
		if grantRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("membership.grant status=%d error=%+v", grantRecorder.Code, grantErr)
		}

		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "work"},
		), "task-spec-handoff-pipeline")

		createRecorder, createResult, createErr := restCall(t, handler, http.MethodPost,
			"/api/v1/projects/project-1/tasks", "dev-project-token",
			api.TaskCreateRequest{
				Title:      "Minimal task",
				PipelineID: applied.PipelineID,
			}, nil, "task-spec-create")
		if createRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.create status=%d error=%+v", createRecorder.Code, createErr)
		}
		created := decodeBody[api.TaskCreateResult](t, createResult)

		expectedLifecycle := uint64(1)
		startRecorder, startResult, startErr := restCall(t, handler, http.MethodPost,
			"/api/v1/tasks/"+created.TaskID+"/start", "dev-project-token",
			api.TaskStartRequest{}, &expectedLifecycle, "task-spec-start")
		if startRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.start status=%d error=%+v", startRecorder.Code, startErr)
		}
		started := decodeBody[api.TaskStartResult](t, startResult)

		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		assertMaterializedTaskSpec(t, prepares[0], created.TaskID, "Minimal task", "", "{}", "[]")

		// Ask a question (cancelling the first execution), then edit the task,
		// then reply so the fresh execution snapshots the edited title,
		// description, metadata, and the updated conversation.
		askVersion := started.LifecycleVersion
		askRecorder, askResult, askErr := restCall(t, handler, http.MethodPost,
			"/api/v1/tasks/"+created.TaskID+"/ask", "dev-project-token",
			api.TaskAskRequest{
				Question:   "Which branch should we target?",
				Assignment: "role/reviewer",
			}, &askVersion, "task-spec-ask")
		if askRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.ask status=%d error=%+v", askRecorder.Code, askErr)
		}
		asked := decodeBody[api.TaskAskResult](t, askResult)

		// Confirm the cancelled predecessor execution is stopped so reply dispatches.
		waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "cancelled")

		editVersion := uint64(1)
		editRecorder, _, editErr := restCall(t, handler, http.MethodPatch,
			"/api/v1/tasks/"+created.TaskID, "dev-project-token",
			api.TaskEditRequest{
				Title:       "Targeted task",
				Description: "Target release-1 branch.",
				Metadata:    map[string]string{"branch": "release-1"},
			}, &editVersion, "task-spec-edit-before-reply")
		if editRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.edit status=%d error=%+v", editRecorder.Code, editErr)
		}

		replyVersion := asked.LifecycleVersion
		replyRecorder, _, replyErr := restCall(t, handler, http.MethodPost,
			"/api/v1/tasks/"+created.TaskID+"/reply", "dev-project-token",
			api.TaskReplyRequest{
				QuestionID:              asked.QuestionID,
				ExpectedQuestionVersion: 1,
				Answer:                  "Use release-1.",
			}, &replyVersion, "task-spec-reply")
		if replyRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.reply status=%d error=%+v", replyRecorder.Code, replyErr)
		}

		prepares = waitForActivationPrepares(t, fixture.runtime, 2)
		assertMaterializedTaskSpec(t, prepares[1], created.TaskID,
			"Targeted task",
			"Target release-1 branch.",
			`{"branch":"release-1"}`,
			"",
		)
		var messages []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(prepares[1].Task, &messages); err != nil {
			t.Fatalf("reply /kodema/task.json is not valid JSON array: %v (%s)", err, prepares[1].Task)
		}
		if len(messages) != 2 || messages[0].Body != "Which branch should we target?" || messages[1].Body != "Use release-1." {
			t.Fatalf("reply /kodema/task.json messages = %+v; want question + answer", messages)
		}
	})

	t.Run("task-free pipeline.start omits task.json and /kodema/task/", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		application := fixture.environment.startApp(t)
		handler := application.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")

		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "run"},
		), "task-free-pipeline")

		version := applied.Version
		startRecorder, _, startErr := restCall(t, handler, http.MethodPost,
			"/api/v1/projects/project-1/pipelines/"+applied.PipelineID+"/start", "dev-project-token",
			api.PipelineStartRequest{}, &version, "task-free-start")
		if startRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("pipeline.start status=%d error=%+v", startRecorder.Code, startErr)
		}

		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		prepare := prepares[0]
		if prepare.TaskID != "" || prepare.Task != nil || len(prepare.TaskFiles) != 0 {
			t.Fatalf("task-free prepare carried task context: id=%q task=%v taskFiles=%v",
				prepare.TaskID, prepare.Task, prepare.TaskFiles)
		}
		materialization, err := workspace.BuildMaterialization(context.Background(), prepare, workspace.MaterializationConfig{
			Scope: tenancy.Scope{TenantID: "dev-tenant", ProjectID: "project-1"},
		})
		if err != nil {
			t.Fatalf("BuildMaterialization task-free: %v", err)
		}
		for _, file := range materialization.Files {
			if file.Path == workspace.TaskPath || strings.HasPrefix(file.Path, workspace.TaskRoot+"/") {
				t.Fatalf("task-free materialization unexpectedly contains %q", file.Path)
			}
		}
	})
}

func assertMaterializedTaskSpec(
	t *testing.T,
	request kernelengine.PrepareRequest,
	wantTaskID, wantTitle, wantDescription, wantMetadataJSON, wantConversation string,
) {
	t.Helper()
	if request.TaskID != id.TaskID(wantTaskID) {
		t.Fatalf("prepare task id = %q; want %q", request.TaskID, wantTaskID)
	}
	materialization, err := workspace.BuildMaterialization(context.Background(), request, workspace.MaterializationConfig{
		Scope: tenancy.Scope{TenantID: "dev-tenant", ProjectID: "project-1"},
	})
	if err != nil {
		t.Fatalf("BuildMaterialization: %v", err)
	}
	files := make(map[string]workspace.File, len(materialization.Files))
	for _, file := range materialization.Files {
		files[file.Path] = file
	}
	checkFile := func(path, want string) {
		t.Helper()
		file, ok := files[path]
		if !ok {
			t.Fatalf("materialization missing %q", path)
		}
		if file.Mode != 0o444 {
			t.Fatalf("%s mode = %#o; want 0444", path, file.Mode)
		}
		if string(file.Data) != want {
			t.Fatalf("%s = %q; want %q", path, file.Data, want)
		}
	}
	if wantConversation != "" {
		checkFile(workspace.TaskPath, wantConversation)
	} else if _, ok := files[workspace.TaskPath]; !ok {
		t.Fatalf("materialization missing %q", workspace.TaskPath)
	}
	checkFile(workspace.TaskTitlePath, wantTitle)
	checkFile(workspace.TaskDescriptionPath, wantDescription)
	checkFile(workspace.TaskMetadataPath, wantMetadataJSON)

	specFile, ok := files[workspace.TaskSpecPath]
	if !ok {
		t.Fatalf("materialization missing %q", workspace.TaskSpecPath)
	}
	if specFile.Mode != 0o444 {
		t.Fatalf("%s mode = %#o; want 0444", workspace.TaskSpecPath, specFile.Mode)
	}
	var spec struct {
		ID          string            `json:"id"`
		Title       string            `json:"title"`
		Description string            `json:"description"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(specFile.Data, &spec); err != nil {
		t.Fatalf("unmarshal %s: %v (%s)", workspace.TaskSpecPath, err, specFile.Data)
	}
	if spec.ID != wantTaskID || spec.Title != wantTitle || spec.Description != wantDescription {
		t.Fatalf("spec.json = %+v; want id=%q title=%q description=%q", spec, wantTaskID, wantTitle, wantDescription)
	}
	var wantMeta map[string]string
	if err := json.Unmarshal([]byte(wantMetadataJSON), &wantMeta); err != nil {
		t.Fatalf("unmarshal want metadata: %v", err)
	}
	if len(spec.Metadata) != len(wantMeta) {
		t.Fatalf("spec.json metadata = %+v; want %+v", spec.Metadata, wantMeta)
	}
	for k, v := range wantMeta {
		if spec.Metadata[k] != v {
			t.Fatalf("spec.json metadata[%q] = %q; want %q", k, spec.Metadata[k], v)
		}
	}
}
