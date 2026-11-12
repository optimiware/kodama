package services_test

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

// The single deterministic clock instant every fixture derives from. It carries
// no monotonic reading, so a JSON round trip through the file store preserves
// exact time equality.
var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

var (
	testTenant  = id.TenantID("tenant-a")
	testProject = id.ProjectID("project-a")
	testService = id.PrincipalID("email-svc")
)

// callLog is the shared, ordered record of boundary interactions. Store and
// effect fakes append to the same log so tests can prove "intent before effect"
// and "observe before retry" ordering without inspecting private fields.
type callLog struct {
	mu      sync.Mutex
	entries []string
}

func (log *callLog) add(entry string) {
	if log == nil {
		return
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	log.entries = append(log.entries, entry)
}

func (log *callLog) snapshot() []string {
	if log == nil {
		return nil
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.entries...)
}

func (log *callLog) indexOf(entry string) int {
	for index, candidate := range log.snapshot() {
		if candidate == entry {
			return index
		}
	}
	return -1
}

func validMergeKey() services.MergeKey {
	return services.MergeKey{Repository: "shop/web", Ref: "refs/heads/main"}
}

func validClaim(t *testing.T) services.Claim {
	t.Helper()
	return services.Claim{
		Key:         validMergeKey(),
		Owner:       "merge-worker-1",
		OperationID: "op-1",
		Generation:  1,
		ExpiresAt:   testNow.Add(time.Minute),
	}
}

func validMergeRequest() services.MergeRequest {
	return services.MergeRequest{
		Key:        validMergeKey(),
		Assignment: services.MergeAssignment{ExecutionID: "exec-1", ActionID: "action-1"},
		Base:       "base-1111111",
		Candidate:  "candidate-2222222",
	}
}

func validAssignment() services.Assignment {
	return services.Assignment{
		TenantID:      testTenant,
		ProjectID:     testProject,
		ExecutionID:   "exec-1",
		ActionID:      "action-1",
		ServiceID:     testService,
		Request:       "service/email-svc",
		Version:       1,
		SourceEventID: "event-1",
		At:            testNow,
		Payload:       json.RawMessage(`{"summary":"ready"}`),
	}
}

// servedRequest is one request action as the served public DTOs expose it. The
// served inbox carries only the identities its kind owns, so the test fixture
// deliberately has no tenant, project, payload or source-event field: those
// never travel over the public wire.
type servedRequest struct {
	executionID      string
	actionID         string
	assignment       string
	executionVersion uint64
	actionVersion    uint64
	at               time.Time
	state            string
	decision         string
	actor            string
	answeredAt       *time.Time
}

// validServedRequest is one running service-assigned request for the adapter's
// own service identity.
func validServedRequest() servedRequest {
	return servedRequest{
		executionID:      "exec-1",
		actionID:         "action-1",
		assignment:       "service/" + string(testService),
		executionVersion: 1,
		actionVersion:    1,
		at:               testNow,
		state:            "running",
	}
}

// inboxItem projects the request as the served task.inbox item.
func (request servedRequest) inboxItem() api.InboxItem {
	return api.InboxItem{
		Kind:        "request",
		ExecutionID: request.executionID,
		ActionID:    request.actionID,
		Assignment:  request.assignment,
		Subject:     &api.Subject{Fields: []api.SubjectField{{Name: "seed", Kind: "string", Value: "seed-1"}}},
		Version:     request.actionVersion,
		At:          request.at,
	}
}

// inspectResult projects the request as the served execution.inspect result.
// The execution and action versions are separate observations.
func (request servedRequest) inspectResult() api.ExecutionInspectResult {
	state := request.state
	if state == "" {
		state = "running"
	}
	return api.ExecutionInspectResult{
		ExecutionID:   request.executionID,
		PipelineID:    "pipeline-1",
		State:         "running",
		Version:       request.executionVersion,
		CurrentAction: 0,
		Actions: []api.ExecutionAction{{
			ActionID:       request.actionID,
			Index:          0,
			Name:           "answer",
			Kind:           "request",
			State:          state,
			Version:        request.actionVersion,
			Decision:       request.decision,
			Actor:          request.actor,
			AnsweredAt:     request.answeredAt,
			TimeoutSeconds: int64((time.Hour) / time.Second),
		}},
		Cleanup: api.ExecutionCleanup{State: "none"},
	}
}

func validAdapterConfig(handler api.Handler, events services.EventSource, clock services.Clock) services.AdapterConfig {
	return services.AdapterConfig{
		Kind:      services.KindEmail,
		ServiceID: testService,
		TenantID:  testTenant,
		ProjectID: testProject,
		Handler:   handler,
		Events:    events,
		Clock:     clock,
	}
}

func validCoordinatorConfig(store services.CoordinationStore, effects services.MergeEffects, clock services.Clock) services.CoordinatorConfig {
	return services.CoordinatorConfig{
		TenantID:       testTenant,
		Owner:          "merge-worker-1",
		Lease:          time.Minute,
		Store:          store,
		Effects:        effects,
		Clock:          clock,
		NewOperationID: operationIDs("op"),
	}
}

// operationIDs returns a deterministic identity generator producing
// "<prefix>-1", "<prefix>-2", ... .
func operationIDs(prefix string) func() (id.OperationID, error) {
	var mu sync.Mutex
	next := 0
	return func() (id.OperationID, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return id.OperationID(prefix + "-" + strconv.Itoa(next)), nil
	}
}
