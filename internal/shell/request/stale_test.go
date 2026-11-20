package request_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

type fakeRefHeadReader struct {
	heads map[string]request.RefHead
	err   error
	calls int
}

func (reader *fakeRefHeadReader) LatestRefHead(_ context.Context, _ tenancy.Scope, resourceID, ref string) (request.RefHead, error) {
	reader.calls++
	if reader.err != nil {
		return request.RefHead{}, reader.err
	}
	return reader.heads[resourceID+"|"+ref], nil
}

func newTestServiceWithRefHeads(t *testing.T, heads *fakeRefHeadReader) *testService {
	t.Helper()
	store := newFakeStore()
	clock := &fixedClock{now: testNow.Add(time.Minute)}
	ids := &sequentialIDs{}
	authorizer := &fakeAuthorizer{}
	authority := &fakeAuthorityReader{}
	activations := &fakeActivationPort{}
	resolutions := newFakeResolvePort(store)
	service, err := request.NewService(request.Config{
		Clock:       clock,
		IDs:         ids,
		Authorizer:  authorizer,
		Authority:   authority,
		State:       store,
		Activations: activations,
		Resolutions: resolutions,
		RefHeads:    heads,
	})
	if err != nil {
		t.Fatalf("request.NewService: %v", err)
	}
	return &testService{
		service:     service,
		clock:       clock,
		ids:         ids,
		authorizer:  authorizer,
		authority:   authority,
		store:       store,
		activations: activations,
		resolutions: resolutions,
	}
}

func gitRefUpdateSubject(resource, ref, expectedBase, candidate string) definition.Subject {
	return literalSubject(map[string]string{
		"operation":     "git.ref.update",
		"resource":      resource,
		"ref":           ref,
		"expected_base": expectedBase,
		"candidate":     candidate,
	})
}

func seedRunningGitRequest(t *testing.T, ts *testService, scope tenancy.Scope, resource, ref, expectedBase, candidate string) kernel.Execution {
	t.Helper()
	action := requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", gitRefUpdateSubject(resource, ref, expectedBase, candidate))
	exec := runningRequestExecution(action)
	ts.store.Seed(scope, exec)
	return exec
}

func TestEvaluateGitRefStaleness(t *testing.T) {
	const (
		shaA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaB    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		zeroSHA = "0000000000000000000000000000000000000000"
	)

	cases := []struct {
		name        string
		target      request.GitRefUpdateSubject
		head        request.RefHead
		wantStale   bool
		wantCurrent string
		wantSubstr  string
	}{
		{
			name:      "no verified event yet",
			target:    request.GitRefUpdateSubject{Resource: "repo-1", Ref: "refs/heads/main", ExpectedBase: shaA},
			head:      request.RefHead{Found: false},
			wantStale: false,
		},
		{
			name:      "exact match",
			target:    request.GitRefUpdateSubject{Resource: "repo-1", Ref: "refs/heads/main", ExpectedBase: shaA},
			head:      request.RefHead{Found: true, Commit: shaA},
			wantStale: false,
		},
		{
			name:        "moved to newer commit",
			target:      request.GitRefUpdateSubject{Resource: "repo-1", Ref: "refs/heads/main", ExpectedBase: shaA},
			head:        request.RefHead{Found: true, Commit: shaB},
			wantStale:   true,
			wantCurrent: shaB,
			wantSubstr:  "moved from expected_base " + shaA + " to " + shaB,
		},
		{
			name:        "unborn empty expected_base but ref was created",
			target:      request.GitRefUpdateSubject{Resource: "repo-1", Ref: "refs/heads/main", ExpectedBase: ""},
			head:        request.RefHead{Found: true, Commit: shaB},
			wantStale:   true,
			wantCurrent: shaB,
			wantSubstr:  "<unborn> to " + shaB,
		},
		{
			name:        "unborn zero-oid expected_base but ref was created",
			target:      request.GitRefUpdateSubject{Resource: "repo-1", Ref: "refs/heads/main", ExpectedBase: zeroSHA},
			head:        request.RefHead{Found: true, Commit: shaB},
			wantStale:   true,
			wantCurrent: shaB,
			wantSubstr:  "<unborn> to " + shaB,
		},
		{
			name:      "deleted ref matches unborn expected_base",
			target:    request.GitRefUpdateSubject{Resource: "repo-1", Ref: "refs/heads/main", ExpectedBase: ""},
			head:      request.RefHead{Found: true, Deleted: true},
			wantStale: false,
		},
		{
			name:       "deleted ref is stale for non-unborn expected_base",
			target:     request.GitRefUpdateSubject{Resource: "repo-1", Ref: "refs/heads/main", ExpectedBase: shaA},
			head:       request.RefHead{Found: true, Deleted: true},
			wantStale:  true,
			wantSubstr: "was deleted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stale, current, reason := request.EvaluateGitRefStaleness(tc.target, tc.head)
			if stale != tc.wantStale {
				t.Fatalf("stale = %v; want %v (reason=%q)", stale, tc.wantStale, reason)
			}
			if current != tc.wantCurrent {
				t.Fatalf("current = %q; want %q", current, tc.wantCurrent)
			}
			if tc.wantSubstr != "" && !strings.Contains(reason, tc.wantSubstr) {
				t.Fatalf("reason = %q; want substring %q", reason, tc.wantSubstr)
			}
		})
	}
}

func TestResolveStaleApprovalGuard(t *testing.T) {
	const (
		shaBase = "1111111111111111111111111111111111111111"
		shaHead = "2222222222222222222222222222222222222222"
		shaCand = "3333333333333333333333333333333333333333"
	)
	scope := projectScope("tenant-1", "project-1")
	reviewer := humanActor("tenant-1", "project-1", "reviewer-1", tenancy.RoleReviewer)

	t.Run("non-stale git.ref.update passes normally", func(t *testing.T) {
		heads := &fakeRefHeadReader{heads: map[string]request.RefHead{
			"repo-1|refs/heads/main": {Found: true, Commit: shaBase},
		}}
		ts := newTestServiceWithRefHeads(t, heads)
		exec := seedRunningGitRequest(t, ts, scope, "repo-1", "refs/heads/main", shaBase, shaCand)

		result, err := ts.service.Resolve(context.Background(), resolveCommand(scope, exec, 0, reviewer, request.DecisionPass, nil))
		if err != nil {
			t.Fatalf("non-stale Resolve pass failed: %v", err)
		}
		if result.Decision != request.DecisionPass {
			t.Fatalf("decision = %q; want pass", result.Decision)
		}
		if heads.calls != 1 {
			t.Fatalf("RefHeads calls = %d; want 1", heads.calls)
		}
	})

	t.Run("stale git.ref.update refuses pass by default with version_conflict", func(t *testing.T) {
		heads := &fakeRefHeadReader{heads: map[string]request.RefHead{
			"repo-1|refs/heads/main": {Found: true, Commit: shaHead},
		}}
		ts := newTestServiceWithRefHeads(t, heads)
		exec := seedRunningGitRequest(t, ts, scope, "repo-1", "refs/heads/main", shaBase, shaCand)

		_, err := ts.service.Resolve(context.Background(), resolveCommand(scope, exec, 0, reviewer, request.DecisionPass, nil))
		requireFaultClass(t, err, fault.VersionConflict)
		for _, want := range []string{"refs/heads/main", "repo-1", shaBase, shaHead, "allow_stale"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stale refusal %q missing %q", err.Error(), want)
			}
		}
		if ts.resolutions.callCount() != 0 {
			t.Fatalf("stale refusal committed %d resolve units; want 0", ts.resolutions.callCount())
		}
	})

	t.Run("stale git.ref.update allows pass when AllowStale is true", func(t *testing.T) {
		heads := &fakeRefHeadReader{heads: map[string]request.RefHead{
			"repo-1|refs/heads/main": {Found: true, Commit: shaHead},
		}}
		ts := newTestServiceWithRefHeads(t, heads)
		exec := seedRunningGitRequest(t, ts, scope, "repo-1", "refs/heads/main", shaBase, shaCand)

		cmd := resolveCommand(scope, exec, 0, reviewer, request.DecisionPass, nil)
		cmd.AllowStale = true
		result, err := ts.service.Resolve(context.Background(), cmd)
		if err != nil {
			t.Fatalf("Resolve with AllowStale=true failed: %v", err)
		}
		if result.Decision != request.DecisionPass {
			t.Fatalf("decision = %q; want pass", result.Decision)
		}
		if heads.calls != 0 {
			t.Fatalf("RefHeads calls with AllowStale=true = %d; want 0", heads.calls)
		}
	})

	t.Run("stale git.ref.update always allows decision fail without AllowStale", func(t *testing.T) {
		heads := &fakeRefHeadReader{heads: map[string]request.RefHead{
			"repo-1|refs/heads/main": {Found: true, Commit: shaHead},
		}}
		ts := newTestServiceWithRefHeads(t, heads)
		exec := seedRunningGitRequest(t, ts, scope, "repo-1", "refs/heads/main", shaBase, shaCand)

		cmd := resolveCommand(scope, exec, 0, reviewer, request.DecisionFail, nil)
		cmd.Note = "rebase required"
		result, err := ts.service.Resolve(context.Background(), cmd)
		if err != nil {
			t.Fatalf("Resolve fail on stale review failed: %v", err)
		}
		if result.Decision != request.DecisionFail {
			t.Fatalf("decision = %q; want fail", result.Decision)
		}
		if heads.calls != 0 {
			t.Fatalf("RefHeads calls on fail = %d; want 0", heads.calls)
		}
	})

	t.Run("idempotent replay of settled pass succeeds even after ref later moves", func(t *testing.T) {
		heads := &fakeRefHeadReader{heads: map[string]request.RefHead{
			"repo-1|refs/heads/main": {Found: true, Commit: shaBase},
		}}
		ts := newTestServiceWithRefHeads(t, heads)
		exec := seedRunningGitRequest(t, ts, scope, "repo-1", "refs/heads/main", shaBase, shaCand)

		cmd := resolveCommand(scope, exec, 0, reviewer, request.DecisionPass, nil)
		first, err := ts.service.Resolve(context.Background(), cmd)
		if err != nil {
			t.Fatalf("first Resolve failed: %v", err)
		}

		// Simulate the ref advancing after approval (e.g. integration succeeded).
		heads.heads["repo-1|refs/heads/main"] = request.RefHead{Found: true, Commit: shaCand}

		replay, err := ts.service.Resolve(context.Background(), cmd)
		if err != nil {
			t.Fatalf("idempotent replay after ref move failed: %v", err)
		}
		if !replay.Replayed || replay.ActionVersion != first.ActionVersion {
			t.Fatalf("replay = %+v; want replayed version %d", replay, first.ActionVersion)
		}
	})

	t.Run("non-git request is unaffected and skips RefHeads", func(t *testing.T) {
		heads := &fakeRefHeadReader{err: fault.New(fault.InternalFailure, "should not be called")}
		ts := newTestServiceWithRefHeads(t, heads)
		action := requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", literalSubject(map[string]string{
			"operation": "deploy.rollout",
			"target":    "prod",
		}))
		exec := runningRequestExecution(action)
		ts.store.Seed(scope, exec)

		if _, err := ts.service.Resolve(context.Background(), resolveCommand(scope, exec, 0, reviewer, request.DecisionPass, nil)); err != nil {
			t.Fatalf("non-git Resolve failed: %v", err)
		}
		if heads.calls != 0 {
			t.Fatalf("RefHeads calls for non-git request = %d; want 0", heads.calls)
		}
	})

	t.Run("ref head reader failure fails closed", func(t *testing.T) {
		heads := &fakeRefHeadReader{err: fault.New(fault.DependencyUnavailable, "store unavailable")}
		ts := newTestServiceWithRefHeads(t, heads)
		exec := seedRunningGitRequest(t, ts, scope, "repo-1", "refs/heads/main", shaBase, shaCand)

		_, err := ts.service.Resolve(context.Background(), resolveCommand(scope, exec, 0, reviewer, request.DecisionPass, nil))
		requireFaultClass(t, err, fault.DependencyUnavailable)
	})
}

func TestEvaluateGitRefStalenessBoundsReasonToMaxSubjectBytes(t *testing.T) {
	// Multibyte UTF-8 expected_base at the maximum subject field size must
	// still produce a valid UTF-8 StaleReason within api.MaxSubjectValueBytes.
	longBase := strings.Repeat("é", api.MaxSubjectValueBytes/2)
	target := request.GitRefUpdateSubject{
		Resource:     "git-repo-1",
		Ref:          "refs/heads/main",
		ExpectedBase: longBase,
		Candidate:    "cccccccccccccccccccccccccccccccccccccccc",
	}

	stale, commit, reason := request.EvaluateGitRefStaleness(target, request.RefHead{
		Found:  true,
		Commit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if !stale {
		t.Fatal("expected stale=true")
	}
	if len(reason) > api.MaxSubjectValueBytes {
		t.Fatalf("moved reason len = %d; want <= %d", len(reason), api.MaxSubjectValueBytes)
	}
	if !utf8.ValidString(reason) {
		t.Fatalf("moved reason is not valid UTF-8")
	}
	if commit != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("commit = %q", commit)
	}
	item := api.InboxItem{
		Kind:        "request",
		ExecutionID: "exec-1",
		ActionID:    "act-1",
		Assignment:  "role/reviewer",
		Subject: &api.Subject{
			Fields: []api.SubjectField{
				{Name: "operation", Kind: "string", Value: "git.ref.update"},
				{Name: "resource", Kind: "string", Value: "git-repo-1"},
				{Name: "ref", Kind: "string", Value: "refs/heads/main"},
				{Name: "expected_base", Kind: "string", Value: longBase},
			},
		},
		Version:          1,
		At:               time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		Stale:            stale,
		CurrentRefCommit: commit,
		StaleReason:      reason,
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("api.InboxItem.Validate() failed for max-length expected_base: %v", err)
	}

	// Deleted ref branch also stays bounded and valid UTF-8.
	staleDel, commitDel, reasonDel := request.EvaluateGitRefStaleness(target, request.RefHead{
		Found:   true,
		Deleted: true,
	})
	if !staleDel || commitDel != "" || len(reasonDel) > api.MaxSubjectValueBytes || !utf8.ValidString(reasonDel) {
		t.Fatalf("deleted staleness invalid: stale=%v commit=%q len=%d valid=%v", staleDel, commitDel, len(reasonDel), utf8.ValidString(reasonDel))
	}
}
