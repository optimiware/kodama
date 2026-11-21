package pipeline_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/pipeline"
)

func TestAdmissionCompletesTaskAndParentID(t *testing.T) {
	resolver := &fakeResolver{}
	admission := newAdmission(t, resolver, nil)
	scope := projectScope("tenant-a", "project-a")

	t.Run("default completes_task is true when omitted", func(t *testing.T) {
		yamlDoc := []byte(`actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [echo ok]
`)
		res, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
			Scope:          scope,
			Definition:     pipeline.Definition{Name: "default", YAML: yamlDoc},
			DefaultTimeout: 5 * time.Minute,
		})
		requireNoError(t, err, "admit default")
		if !res.Snapshot.CompletesTaskOnSuccess() {
			t.Fatal("omitted completes_task must default to true")
		}
	})

	t.Run("yaml completes_task false is snapshotted", func(t *testing.T) {
		yamlDoc := []byte(`completes_task: false
actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [echo ok]
`)
		res, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
			Scope:          scope,
			Definition:     pipeline.Definition{Name: "stage", YAML: yamlDoc},
			Task:           pipeline.TaskContext{TaskID: "task-child", ParentID: "task-parent", Title: "Child", Conversation: []byte("[]")},
			DefaultTimeout: 5 * time.Minute,
		})
		requireNoError(t, err, "admit completes_task=false")
		if res.Snapshot.CompletesTaskOnSuccess() {
			t.Fatal("expected CompletesTaskOnSuccess() == false from YAML")
		}
		if res.Snapshot.ParentID != id.TaskID("task-parent") {
			t.Fatalf("snapshot ParentID = %q; want task-parent", res.Snapshot.ParentID)
		}
	})

	t.Run("request override takes precedence over yaml", func(t *testing.T) {
		yamlFalse := []byte(`completes_task: false
actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [echo ok]
`)
		overrideTrue := true
		res, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
			Scope:          scope,
			Definition:     pipeline.Definition{Name: "stage", YAML: yamlFalse},
			CompletesTask:  &overrideTrue,
			DefaultTimeout: 5 * time.Minute,
		})
		requireNoError(t, err, "admit override true")
		if !res.Snapshot.CompletesTaskOnSuccess() {
			t.Fatal("expected explicit true override to win over YAML false")
		}

		yamlDefault := []byte(`actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [echo ok]
`)
		overrideFalse := false
		res2, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
			Scope:          scope,
			Definition:     pipeline.Definition{Name: "stage", YAML: yamlDefault},
			CompletesTask:  &overrideFalse,
			DefaultTimeout: 5 * time.Minute,
		})
		requireNoError(t, err, "admit override false")
		if res2.Snapshot.CompletesTaskOnSuccess() {
			t.Fatal("expected explicit false override to win over default true")
		}
	})

	t.Run("invalid non-boolean completes_task is refused", func(t *testing.T) {
		yamlBad := []byte(`completes_task: "nope"
actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [echo ok]
`)
		_, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
			Scope:          scope,
			Definition:     pipeline.Definition{Name: "bad", YAML: yamlBad},
			DefaultTimeout: 5 * time.Minute,
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("bad completes_task err = %v; want invalid_input", err)
		}
	})
}
