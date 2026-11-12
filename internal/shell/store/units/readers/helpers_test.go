package readers_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

const (
	testTenant     = "tenant-1"
	testProject    = "project-1"
	otherProject   = "project-2"
	testPrincipal  = "principal-1"
	testPipeline   = "pipeline-1"
	testTask       = "task-1"
	testExecution  = "execution-1"
	integrationImg = "registry.example.com/run@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	contentHashA   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	contentHashB   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: otherProject}
}

// newStore returns one independent memory store with the fixed transaction
// clock.
func newStore() *memory.Store {
	return memory.NewWithClock(func() time.Time { return testNow })
}

// seedPipeline writes one pipelines row through the neutral seam so the real
// work adapter can read and guard it.
func seedPipeline(t *testing.T, backend *memory.Store, scope tenancy.Scope, identity pipeline.PipelineID, name string, yaml []byte, version uint64) {
	t.Helper()
	digest := sha256.Sum256(yaml)
	_, err := backend.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "pipelines",
			ID:     string(identity),
			Insert: true,
			Values: map[string]any{
				"name":        name,
				"yaml":        yaml,
				"yaml_sha256": digest[:],
				"version":     int64(version),
				"created_at":  testNow,
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed pipeline: %v", err)
	}
}

// seedMembership writes one membership row through the neutral seam so the real
// identity adapter resolves the principal and its authority facts.
func seedMembership(t *testing.T, backend *memory.Store, scope tenancy.Scope, rowID string, principal id.PrincipalID, version uint64, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("encode roles: %v", err)
	}
	_, err = backend.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "memberships",
			ID:     rowID,
			Insert: true,
			Values: map[string]any{
				"principal_id": string(principal),
				"roles":        encoded,
				"version":      int64(version),
				"created_at":   testNow,
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// receiptCounter allocates deterministic unique receipt identities.
type receiptCounter struct {
	mu sync.Mutex
	n  int
}

func (counter *receiptCounter) next(scope tenancy.Scope, principal id.PrincipalID, operation, caller string) receipt.Record {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	counter.n++
	return receipt.Record{
		ID: id.ReceiptID(fmt.Sprintf("receipt-%d", counter.n)),
		Key: receipt.Key{
			TenantID:    scope.TenantID,
			ProjectID:   scope.ProjectID,
			PrincipalID: principal,
			Operation:   operation,
			CallerKey:   caller,
		},
		ContentHash: contentHashA,
		At:          testNow,
	}
}

// startedTask writes one open task with a nested queued execution through the
// real work adapter. It returns the task and execution identities.
func startedTask(t *testing.T, adapters *work.Task, pipelines *work.Pipelines, receipts *receiptCounter, scope tenancy.Scope) (id.TaskID, id.ExecutionID) {
	t.Helper()
	definitionValue, err := pipelines.Get(context.Background(), scope, testPipeline)
	if err != nil {
		t.Fatalf("read seeded pipeline: %v", err)
	}
	taskID := id.TaskID(testTask + "-" + string(scope.ProjectID))
	executionID := id.ExecutionID(testExecution + "-" + string(scope.ProjectID))
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   definitionValue.YAML,
		TaskID: taskID,
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	container := definition.Container{Image: integrationImg, Commands: []string{"echo", "ok"}}
	actions := []pipeline.AdmittedAction{{
		Index:     0,
		Name:      id.ActionName("run"),
		Kind:      definition.ActionKindContainer,
		Timeout:   time.Minute,
		Container: &container,
	}}
	record := receipts.next(scope, testPrincipal, "task.create_and_start", "create-"+string(scope.ProjectID))
	created, err := adapters.Create(context.Background(), task.CreateUnit{
		Owner:            scope,
		TaskID:           taskID,
		Title:            "Fix the build",
		SelectedPipeline: testPipeline,
		Initiator:        testPrincipal,
		At:               testNow,
		Receipt:          record,
		Start: &task.StartUnit{
			Owner:                   scope,
			TaskID:                  taskID,
			PipelineID:              testPipeline,
			ExpectedPipelineVersion: 1,
			Snapshot:                snapshot,
			Actions:                 actions,
			Initiator:               testPrincipal,
			ExecutionPrincipal:      id.PrincipalID("execution-principal-1"),
			NewExecutionID:          executionID,
			At:                      testNow,
		},
	})
	if err != nil {
		t.Fatalf("create-and-start task: %v", err)
	}
	return created.TaskID, created.ExecutionID
}

// isClass reports whether err carries the given fault class.
func isClass(err error, class fault.Class) bool {
	if err == nil {
		return false
	}
	actual, ok := fault.ClassOf(err)
	return ok && actual == class
}

// jsonMarshal encodes one value as a stored jsonb column.
func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}
