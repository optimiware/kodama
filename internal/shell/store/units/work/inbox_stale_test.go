package work

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/inbox"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

func (f *fixture) seedGitRefUpdateRequest(
	t *testing.T,
	scope tenancy.Scope,
	executionID id.ExecutionID,
	actionID id.ActionID,
	resource, ref, expectedBase, candidate string,
	at time.Time,
) {
	t.Helper()
	assignment := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator}
	resolved := definition.NewSubject(map[string]definition.SubjectValue{
		"operation":     {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
		"resource":      {Kind: definition.SubjectLiteral, Literal: resource},
		"ref":           {Kind: definition.SubjectLiteral, Literal: ref},
		"expected_base": {Kind: definition.SubjectLiteral, Literal: expectedBase},
		"candidate":     {Kind: definition.SubjectLiteral, Literal: candidate},
	})
	f.seedRequestProjection(t, scope, requestProjection{
		executionID: executionID,
		actionID:    actionID,
		name:        "review",
		assignment:  assignment,
		declared: definition.Request{
			Assignment: assignment.String(),
			Subject:    resolved,
			Outputs:    []id.OutputKey{"approved"},
		},
		resolved: resolved,
		at:       at,
	})
}

func (f *fixture) seedVerifiedGitRefUpdatedEvent(
	t *testing.T,
	scope tenancy.Scope,
	eventID string,
	resource, ref, expectedBase, candidate string,
	deleted bool,
) {
	t.Helper()
	payload := map[string]any{
		"operation":     "git.ref.update",
		"resource":      resource,
		"ref":           ref,
		"expected_base": expectedBase,
		"candidate":     candidate,
	}
	if deleted {
		payload["deleted"] = true
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal git.ref.updated: %v", err)
	}
	envelope, err := event.NewEnvelope(
		id.EventID(eventID),
		event.Source{Kind: event.SourceService, PrincipalID: "git-service"},
		scope,
		testNow,
		event.EnvelopeInput{
			Type:           event.TypeVerifiedReport,
			ReportType:     event.ReportType("git.ref.updated"),
			SubjectID:      resource,
			SubjectVersion: 1,
			CausationID:    "ver-" + eventID,
			Data:           data,
		},
	)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{Events: []event.Envelope{envelope}}); err != nil {
		t.Fatalf("commit git.ref.updated: %v", err)
	}
}

func TestInboxFlagsStaleGitRefUpdateRequests(t *testing.T) {
	const (
		shaBase1 = "1111111111111111111111111111111111111111"
		shaBase2 = "2222222222222222222222222222222222222222"
		shaCand1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaCand2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	f := newFixture(t)
	scope := testScope()

	// Seed two active git.ref.update requests targeting repo-1 refs/heads/main:
	// - action-stale expects shaBase1
	// - action-fresh expects shaBase2
	// And one unborn request targeting repo-1 refs/heads/release with expected_base "".
	f.seedGitRefUpdateRequest(t, scope, "exec-stale", "action-stale", "repo-1", "refs/heads/main", shaBase1, shaCand1, testNow.Add(time.Minute))
	f.seedGitRefUpdateRequest(t, scope, "exec-fresh", "action-fresh", "repo-1", "refs/heads/main", shaBase2, shaCand2, testNow.Add(2*time.Minute))
	f.seedGitRefUpdateRequest(t, scope, "exec-unborn", "action-unborn", "repo-1", "refs/heads/release", "", shaCand1, testNow.Add(3*time.Minute))

	// Before any verified git.ref.updated events, none are marked stale.
	before, err := f.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Limit: 10})
	if err != nil {
		t.Fatalf("Query before events: %v", err)
	}
	for _, item := range before.Items {
		if item.Stale || item.CurrentRefCommit != "" || item.StaleReason != "" {
			t.Fatalf("item %s unexpectedly stale before events: %+v", item.ActionID, item)
		}
	}

	// Commit verified git.ref.updated moving refs/heads/main from shaBase1 -> shaBase2,
	// and creating refs/heads/release at shaBase1.
	f.seedVerifiedGitRefUpdatedEvent(t, scope, "evt-main-move", "repo-1", "refs/heads/main", shaBase1, shaBase2, false)
	f.seedVerifiedGitRefUpdatedEvent(t, scope, "evt-release-create", "repo-1", "refs/heads/release", "", shaBase1, false)

	// An unverified custom.observation attempting to spoof main -> spoofed must not affect staleness.
	spoofData, _ := json.Marshal(map[string]any{
		"operation":     "git.ref.update",
		"resource":      "repo-1",
		"ref":           "refs/heads/main",
		"expected_base": shaBase2,
		"candidate":     "9999999999999999999999999999999999999999",
	})
	spoofEnv, err := event.NewEnvelope(
		id.EventID("evt-custom-spoof"),
		event.Source{Kind: event.SourceService, PrincipalID: "git-service"},
		scope,
		testNow,
		event.EnvelopeInput{Type: event.TypeCustomObservation, SubjectID: "repo-1", SubjectVersion: 1, Data: spoofData},
	)
	if err != nil {
		t.Fatalf("NewEnvelope spoof: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{Events: []event.Envelope{spoofEnv}}); err != nil {
		t.Fatalf("commit spoof: %v", err)
	}

	after, err := f.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Limit: 10})
	if err != nil {
		t.Fatalf("Query after events: %v", err)
	}
	byAction := make(map[id.ActionID]inbox.Item, len(after.Items))
	for _, item := range after.Items {
		byAction[item.ActionID] = item
	}

	staleItem := byAction["action-stale"]
	if !staleItem.Stale || staleItem.CurrentRefCommit != shaBase2 || !strings.Contains(staleItem.StaleReason, shaBase1) || !strings.Contains(staleItem.StaleReason, shaBase2) {
		t.Fatalf("action-stale = %+v; want Stale=true CurrentRefCommit=%s", staleItem, shaBase2)
	}

	freshItem := byAction["action-fresh"]
	if freshItem.Stale || freshItem.CurrentRefCommit != "" || freshItem.StaleReason != "" {
		t.Fatalf("action-fresh = %+v; want non-stale (custom observation must not spoof)", freshItem)
	}

	unbornItem := byAction["action-unborn"]
	if !unbornItem.Stale || unbornItem.CurrentRefCommit != shaBase1 || !strings.Contains(unbornItem.StaleReason, "<unborn>") {
		t.Fatalf("action-unborn = %+v; want Stale=true CurrentRefCommit=%s", unbornItem, shaBase1)
	}
}
