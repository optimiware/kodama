package app

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
)

// setDown switches availability.
func (broker *inProcessBroker) setDown(down bool) {
	broker.mu.Lock()
	broker.down = down
	broker.mu.Unlock()
}

// hasPublished reports whether one identity was published.
func (broker *inProcessBroker) hasPublished(identity id.EventID) bool {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	_, ok := broker.published[identity]
	return ok
}

func TestBrokerOutageDoesNotBlockProgressionAndBacklogDrains(t *testing.T) {
	environment := newTestEnvironment(t)
	broker, err := newInProcessBroker(environment.clock)
	if err != nil {
		t.Fatalf("newInProcessBroker: %v", err)
	}
	environment.config.Adapters.Publisher = broker
	environment.config.Adapters.Invalidator = broker
	broker.setDown(true)

	app := environment.startApp(t)
	handler := app.Handler()
	token := "dev-project-token"
	grantProjectEngineer(t, handler, "dev-admin-token")

	recorder, result, publicError := restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/pipelines/build", token,
		api.PipelineApplyRequest{Name: "build", YAML: []byte(smokePipeline)}, nil, "apply-broker-down")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("pipeline.apply status=%d error=%+v", recorder.Code, publicError)
	}
	applied := decodeBody[api.PipelineApplyResult](t, result)
	recorder, result, publicError = restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", token,
		api.TaskCreateAndStartRequest{Title: "broker down", PipelineID: applied.PipelineID}, nil, "start-broker-down")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	started := decodeBody[api.TaskCreateAndStartResult](t, result)

	// Execution progression is broker-independent.
	eventually(t, 5*time.Second, func() bool {
		_, inspected, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, token, nil, nil, "")
		var view api.ExecutionInspectResult
		if err := json.Unmarshal(inspected.Body, &view); err != nil {
			return false
		}
		return view.State == "succeeded"
	})

	// The publication backlog is retained while the broker is unavailable.
	recorder, result, publicError = restCall(t, handler, http.MethodGet,
		"/api/v1/projects/project-1/events", token, nil, nil, "")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("event.read status=%d error=%+v", recorder.Code, publicError)
	}
	events := decodeBody[api.EventReadResult](t, result)
	if len(events.Events) == 0 {
		t.Fatal("no committed events to publish")
	}
	for _, envelope := range events.Events {
		if broker.hasPublished(idOfEvent(envelope.ID)) {
			t.Fatalf("event %q was published while the broker was down", envelope.ID)
		}
	}

	// Restoring the broker drains the backlog with the original identities.
	broker.setDown(false)
	eventually(t, 5*time.Second, func() bool {
		for _, envelope := range events.Events {
			if !broker.hasPublished(idOfEvent(envelope.ID)) {
				return false
			}
		}
		return true
	})
}

func idOfEvent(value string) id.EventID { return id.EventID(value) }
