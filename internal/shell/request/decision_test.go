package request_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

func TestResolveCreatesApprovalDecisionOnlyForHumanRolePass(t *testing.T) {
	t.Run("role pass creates immutable approval decision and emits decision_id in request.resolved event", func(t *testing.T) {
		scope := projectScope("tenant-1", "project-1")
		subject := literalSubject(map[string]string{
			"operation":     "git.ref.update",
			"resource":      "repo-web",
			"ref":           "refs/heads/main",
			"expected_base": "base-1111",
			"candidate":     "cand-2222",
		})
		aggregate := runningRequestExecution(requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", subject, "approved"))
		snap := definition.NewSnapshot(definition.SnapshotInput{
			Pipeline: aggregate.Snapshot.Pipeline(),
			YAML:     []byte("actions: []\n"),
			TaskID:   id.TaskID("task-alpha"),
		})
		aggregate.Snapshot = snap

		fixture := newTestService(t)
		fixture.clock.now = testNow.Add(time.Minute)
		fixture.store.Seed(scope, aggregate)
		actor := humanActor("tenant-1", "project-1", "reviewer-1", tenancy.RoleReviewer)

		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireNoError(t, err, "Resolve(pass)")

		unit := fixture.resolutions.captured()[0]
		if unit.ApprovalDecision == nil {
			t.Fatal("expected ApprovalDecision on human role pass resolution")
		}
		dec := unit.ApprovalDecision
		if !strings.HasPrefix(string(dec.ID), "dec_") {
			t.Errorf("decision id = %q; want dec_ prefix", dec.ID)
		}
		if dec.Scope != scope {
			t.Errorf("decision scope = %+v; want %+v", dec.Scope, scope)
		}
		if dec.TaskID != id.TaskID("task-alpha") {
			t.Errorf("decision task_id = %q; want task-alpha", dec.TaskID)
		}
		if dec.ExecutionID != aggregate.ID || dec.ActionID != aggregate.Actions[0].ID {
			t.Errorf("decision execution/action = (%q, %q); want (%q, %q)", dec.ExecutionID, dec.ActionID, aggregate.ID, aggregate.Actions[0].ID)
		}
		if dec.Assignment.String() != "role/reviewer" {
			t.Errorf("decision assignment = %q; want role/reviewer", dec.Assignment.String())
		}
		if dec.Operation != "git.ref.update" || dec.ResourceID != "repo-web" {
			t.Errorf("decision op/resource = (%q, %q); want (git.ref.update, repo-web)", dec.Operation, dec.ResourceID)
		}
		if dec.Approver != actor.ID {
			t.Errorf("decision approver = %q; want %q", dec.Approver, actor.ID)
		}
		if !dec.ResolvedAt.Equal(fixture.clock.now) {
			t.Errorf("decision resolved_at = %v; want %v", dec.ResolvedAt, fixture.clock.now)
		}
		wantExpiry := fixture.clock.now.Add(request.DefaultDecisionTTL)
		if !dec.ExpiresAt.Equal(wantExpiry) {
			t.Errorf("decision expires_at = %v; want %v", dec.ExpiresAt, wantExpiry)
		}

		// Verify request.resolved event carries decision_id in JSON data.
		var resolvedEvent *event.Envelope
		for i := range unit.Events {
			if unit.Events[i].Type == event.TypeRequestResolved {
				resolvedEvent = &unit.Events[i]
				break
			}
		}
		if resolvedEvent == nil {
			t.Fatal("missing request.resolved event")
		}
		var payload struct {
			ActionID   string `json:"action_id"`
			Decision   string `json:"decision"`
			Actor      string `json:"actor"`
			DecisionID string `json:"decision_id"`
		}
		if err := json.Unmarshal(resolvedEvent.Data, &payload); err != nil {
			t.Fatalf("unmarshal request.resolved data: %v", err)
		}
		if payload.DecisionID != string(dec.ID) {
			t.Errorf("event decision_id = %q; want %q", payload.DecisionID, dec.ID)
		}
		if payload.Decision != "pass" || payload.ActionID != string(aggregate.Actions[0].ID) {
			t.Errorf("event payload = %+v", payload)
		}
	})

	for _, nonPass := range []request.Decision{request.DecisionRevise, request.DecisionFail} {
		t.Run("role "+string(nonPass)+" creates NO approval decision", func(t *testing.T) {
			fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "feedback")
			fixture.clock.now = testNow.Add(time.Minute)
			actor := humanActor("tenant-1", "project-1", "reviewer-1", tenancy.RoleReviewer)

			cmd := resolveCommand(scope, aggregate, 0, actor, nonPass, nil)
			cmd.Note = "needs changes"
			_, err := fixture.service.Resolve(context.Background(), cmd)
			requireNoError(t, err, "Resolve")

			unit := fixture.resolutions.captured()[0]
			if unit.ApprovalDecision != nil {
				t.Fatalf("non-pass decision %q unexpectedly created ApprovalDecision: %+v", nonPass, unit.ApprovalDecision)
			}
			for _, env := range unit.Events {
				if env.Type == event.TypeRequestResolved && strings.Contains(string(env.Data), "decision_id") {
					t.Errorf("non-pass request.resolved event unexpectedly carried decision_id: %s", env.Data)
				}
			}
		})
	}

	t.Run("service pass creates NO approval decision", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "service/merge-coordinator", "output")
		fixture.clock.now = testNow.Add(time.Minute)
		actor := serviceActor("tenant-1", "project-1", "merge-coordinator")

		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"output": "sha-new"}))
		requireNoError(t, err, "Resolve(service pass)")

		unit := fixture.resolutions.captured()[0]
		if unit.ApprovalDecision != nil {
			t.Fatalf("service resolution unexpectedly created ApprovalDecision: %+v", unit.ApprovalDecision)
		}
		for _, env := range unit.Events {
			if env.Type == event.TypeRequestResolved && strings.Contains(string(env.Data), "decision_id") {
				t.Errorf("service request.resolved event unexpectedly carried decision_id: %s", env.Data)
			}
		}
	})

	t.Run("custom DecisionTTL is respected and bounds are enforced", func(t *testing.T) {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		_, err := request.NewService(request.Config{
			Clock:       fixture.clock,
			IDs:         fixture.ids,
			Authorizer:  fixture.authorizer,
			Authority:   fixture.authority,
			State:       fixture.store,
			Activations: fixture.activations,
			Resolutions: fixture.resolutions,
			DecisionTTL: request.MaxDecisionTTL + time.Second,
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("oversized DecisionTTL err = %v; want invalid_input", err)
		}

		customSvc, err := request.NewService(request.Config{
			Clock:       fixture.clock,
			IDs:         fixture.ids,
			Authorizer:  fixture.authorizer,
			Authority:   fixture.authority,
			State:       fixture.store,
			Activations: fixture.activations,
			Resolutions: fixture.resolutions,
			DecisionTTL: 4 * time.Hour,
		})
		requireNoError(t, err, "NewService(4h)")

		fixture.clock.now = testNow.Add(time.Minute)
		actor := humanActor("tenant-1", "project-1", "reviewer-1", tenancy.RoleReviewer)
		_, err = customSvc.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireNoError(t, err, "Resolve(custom TTL)")

		unit := fixture.resolutions.captured()[0]
		if unit.ApprovalDecision == nil || !unit.ApprovalDecision.ExpiresAt.Equal(fixture.clock.now.Add(4*time.Hour)) {
			t.Fatalf("custom TTL expires_at = %+v; want %v", unit.ApprovalDecision, fixture.clock.now.Add(4*time.Hour))
		}
	})
}
