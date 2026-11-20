package readers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/store/memory"
	"github.com/optimiware/kodema/internal/shell/store/units/readers"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

func commitVerifiedGitRefUpdated(
	t *testing.T,
	backend *memory.Store,
	scope tenancy.Scope,
	eventID string,
	resourceID, ref, expectedBase, candidate string,
	deleted bool,
	correlationID string,
) {
	t.Helper()
	payload := map[string]any{
		"operation":     "git.ref.update",
		"resource":      resourceID,
		"ref":           ref,
		"expected_base": expectedBase,
		"candidate":     candidate,
	}
	if deleted {
		payload["deleted"] = true
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	envelope, err := event.NewEnvelope(
		id.EventID(eventID),
		event.Source{Kind: event.SourceService, PrincipalID: "git-service"},
		scope,
		testNow,
		event.EnvelopeInput{
			Type:           event.TypeVerifiedReport,
			ReportType:     event.ReportType("git.ref.updated"),
			SubjectID:      resourceID,
			SubjectVersion: 1,
			CorrelationID:  correlationID,
			CausationID:    "ver-" + eventID,
			Data:           data,
		},
	)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Events: []event.Envelope{envelope}}); err != nil {
		t.Fatalf("commit git.ref.updated %s: %v", eventID, err)
	}
}

func TestRefHeadReaderLatestVerifiedCommit(t *testing.T) {
	backend := newStore()
	scope := testScope()
	other := otherScope()

	reader, err := readers.NewRefHeadReader(backend)
	if err != nil {
		t.Fatalf("NewRefHeadReader: %v", err)
	}

	// No events yet -> Found: false.
	head, err := reader.LatestRefHead(context.Background(), scope, "repo-1", "refs/heads/main")
	if err != nil {
		t.Fatalf("LatestRefHead empty: %v", err)
	}
	if head.Found || head.Commit != "" || head.Deleted {
		t.Fatalf("empty LatestRefHead = %+v; want zero", head)
	}

	// Commit verified updates in sequence order with identical timestamps:
	// seq 1: main base-0 -> commit-1 (human push, no correlation)
	// seq 2: main commit-1 -> commit-2 (execution push, with correlation — must still count!)
	// seq 3: work/feat -> feat-1 (different ref on same resource)
	// seq 4: repo-2 main -> repo2-1 (different resource)
	// seq 5: otherScope repo-1 main -> other-scope-1 (different project)
	commitVerifiedGitRefUpdated(t, backend, scope, "evt-1", "repo-1", "refs/heads/main", "base-0", "commit-1", false, "")
	commitVerifiedGitRefUpdated(t, backend, scope, "evt-2", "repo-1", "refs/heads/main", "commit-1", "commit-2", false, "exec-1")
	commitVerifiedGitRefUpdated(t, backend, scope, "evt-3", "repo-1", "refs/heads/work/feat", "", "feat-1", false, "exec-2")
	commitVerifiedGitRefUpdated(t, backend, scope, "evt-4", "repo-2", "refs/heads/main", "", "repo2-1", false, "")
	commitVerifiedGitRefUpdated(t, backend, other, "evt-5", "repo-1", "refs/heads/main", "commit-2", "other-scope-1", false, "")

	// Also commit a custom.observation with an identical payload targeting
	// repo-1 refs/heads/main: it MUST be ignored (unverified events never count).
	spoofData, _ := json.Marshal(map[string]any{
		"operation":     "git.ref.update",
		"resource":      "repo-1",
		"ref":           "refs/heads/main",
		"expected_base": "commit-2",
		"candidate":     "spoofed-commit",
	})
	spoofEnvelope, err := event.NewEnvelope(
		id.EventID("evt-spoof"),
		event.Source{Kind: event.SourceService, PrincipalID: "git-service"},
		scope,
		testNow,
		event.EnvelopeInput{
			Type:           event.TypeCustomObservation,
			SubjectID:      "repo-1",
			SubjectVersion: 1,
			Data:           spoofData,
		},
	)
	if err != nil {
		t.Fatalf("NewEnvelope spoof: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Events: []event.Envelope{spoofEnvelope}}); err != nil {
		t.Fatalf("commit spoof event: %v", err)
	}

	// Also commit a verified report of a different report_type: must be ignored.
	otherReport, err := event.NewEnvelope(
		id.EventID("evt-other-report"),
		event.Source{Kind: event.SourceService, PrincipalID: "git-service"},
		scope,
		testNow,
		event.EnvelopeInput{
			Type:           event.TypeVerifiedReport,
			ReportType:     event.ReportType("resource.merged"),
			SubjectID:      "repo-1",
			SubjectVersion: 1,
			Data:           spoofData,
		},
	)
	if err != nil {
		t.Fatalf("NewEnvelope other report: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Events: []event.Envelope{otherReport}}); err != nil {
		t.Fatalf("commit other report: %v", err)
	}

	head, err = reader.LatestRefHead(context.Background(), scope, "repo-1", "refs/heads/main")
	if err != nil {
		t.Fatalf("LatestRefHead main: %v", err)
	}
	if !head.Found || head.Deleted || head.Commit != "commit-2" {
		t.Fatalf("LatestRefHead(repo-1, main) = %+v; want Found=true Commit=commit-2", head)
	}

	head, err = reader.LatestRefHead(context.Background(), scope, "repo-1", "refs/heads/work/feat")
	if err != nil {
		t.Fatalf("LatestRefHead work/feat: %v", err)
	}
	if !head.Found || head.Commit != "feat-1" {
		t.Fatalf("LatestRefHead(repo-1, work/feat) = %+v; want feat-1", head)
	}

	head, err = reader.LatestRefHead(context.Background(), other, "repo-1", "refs/heads/main")
	if err != nil {
		t.Fatalf("LatestRefHead otherScope: %v", err)
	}
	if !head.Found || head.Commit != "other-scope-1" {
		t.Fatalf("LatestRefHead(otherScope, repo-1, main) = %+v; want other-scope-1", head)
	}
}

func TestRefHeadReaderDeletedRefAndPagination(t *testing.T) {
	backend := newStore()
	scope := testScope()

	reader, err := readers.NewRefHeadReader(backend)
	if err != nil {
		t.Fatalf("NewRefHeadReader: %v", err)
	}

	// Commit one update to target ref, then > MaxQueryLimit updates to another
	// ref on the same resource so LatestRefHead must paginate descending across
	// multiple scanner pages.
	commitVerifiedGitRefUpdated(t, backend, scope, "evt-main-1", "repo-1", "refs/heads/main", "", "commit-main-1", false, "")
	for i := range store.MaxQueryLimit + 5 {
		commitVerifiedGitRefUpdated(
			t, backend, scope,
			fmt.Sprintf("evt-other-%04d", i),
			"repo-1",
			"refs/heads/work/noise",
			"",
			fmt.Sprintf("noise-%04d", i),
			false,
			"",
		)
	}

	head, err := reader.LatestRefHead(context.Background(), scope, "repo-1", "refs/heads/main")
	if err != nil {
		t.Fatalf("LatestRefHead paginated: %v", err)
	}
	if !head.Found || head.Deleted || head.Commit != "commit-main-1" {
		t.Fatalf("paginated LatestRefHead = %+v; want commit-main-1", head)
	}

	// Now delete refs/heads/main via zero OID candidate.
	commitVerifiedGitRefUpdated(t, backend, scope, "evt-main-del", "repo-1", "refs/heads/main", "commit-main-1", "0000000000000000000000000000000000000000", false, "")
	head, err = reader.LatestRefHead(context.Background(), scope, "repo-1", "refs/heads/main")
	if err != nil {
		t.Fatalf("LatestRefHead deleted: %v", err)
	}
	if !head.Found || !head.Deleted || head.Commit != "" {
		t.Fatalf("deleted LatestRefHead = %+v; want Found=true Deleted=true Commit=''", head)
	}
}

func TestRefHeadReaderValidation(t *testing.T) {
	if _, err := readers.NewRefHeadReader(nil); err == nil {
		t.Fatal("NewRefHeadReader(nil) succeeded; want invalid_input")
	}
	backend := memory.New()
	reader, err := readers.NewRefHeadReader(backend)
	if err != nil {
		t.Fatalf("NewRefHeadReader: %v", err)
	}
	var _ request.RefHeadReader = reader

	cases := []struct {
		name       string
		scope      tenancy.Scope
		resourceID string
		ref        string
	}{
		{"tenant-only scope", tenancy.Scope{TenantID: testTenant}, "repo-1", "refs/heads/main"},
		{"empty resource", testScope(), "", "refs/heads/main"},
		{"empty ref", testScope(), "repo-1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reader.LatestRefHead(context.Background(), tc.scope, tc.resourceID, tc.ref)
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("LatestRefHead(%s) = %v; want invalid_input", tc.name, err)
			}
		})
	}
}

type countingScanner struct {
	inner store.Scanner
	calls int
}

func (c *countingScanner) Query(ctx context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	c.calls++
	return c.inner.Query(ctx, scope, query)
}

func TestRefHeadReaderDeletedFormatsMalformedDataAndBatchScan(t *testing.T) {
	backend := memory.New()
	scope := testScope()
	counter := &countingScanner{inner: backend}
	reader, err := readers.NewRefHeadReader(counter)
	if err != nil {
		t.Fatalf("NewRefHeadReader: %v", err)
	}

	commitRaw := func(idVal string, raw []byte) {
		t.Helper()
		unit := store.Unit{
			Events: []event.Envelope{{
				ID:             id.EventID(idVal),
				Type:           event.TypeVerifiedReport,
				ReportType:     event.ReportType("git.ref.updated"),
				Source:         event.Source{Kind: event.SourceService, PrincipalID: "svc-git"},
				Scope:          scope,
				SubjectID:      "repo-1",
				SubjectVersion: 1,
				CausationID:    "ver-" + idVal,
				At:             testNow,
				Data:           raw,
			}},
		}
		if _, err := backend.Execute(context.Background(), scope, unit); err != nil {
			t.Fatalf("commit %s: %v", idVal, err)
		}
	}

	// Native boolean deleted: true on refs/heads/bool-del.
	commitVerifiedGitRefUpdated(t, backend, scope, "evt-bool-del", "repo-1", "refs/heads/bool-del", "commit-old", "commit-old", true, "")
	// String-encoded deleted: "true" (matching verify.reportEventData) on refs/heads/str-del.
	commitRaw("evt-str-del", []byte(`{"operation":"git.ref.update","resource":"repo-1","ref":"refs/heads/str-del","expected_base":"commit-old","candidate":"commit-old","deleted":"true"}`))
	// String-encoded deleted: "false" on refs/heads/main.
	commitRaw("evt-main-ok", []byte(`{"operation":"git.ref.update","resource":"repo-1","ref":"refs/heads/main","expected_base":"commit-0","candidate":"commit-1","deleted":"false"}`))
	// Non-object / empty JSON rows newer than valid events must be skipped cleanly.
	commitRaw("evt-malformed", []byte(`"not-an-object"`))
	commitRaw("evt-empty", []byte(`{}`))

	heads, err := reader.LatestRefHeads(context.Background(), scope, "repo-1", []string{
		"refs/heads/main",
		"refs/heads/bool-del",
		"refs/heads/str-del",
	})
	if err != nil {
		t.Fatalf("LatestRefHeads: %v", err)
	}
	if counter.calls != 1 {
		t.Fatalf("batch LatestRefHeads executed %d scanner queries; want 1", counter.calls)
	}
	if h := heads["refs/heads/main"]; !h.Found || h.Deleted || h.Commit != "commit-1" {
		t.Fatalf("main head = %+v; want commit-1", h)
	}
	if h := heads["refs/heads/bool-del"]; !h.Found || !h.Deleted || h.Commit != "" {
		t.Fatalf("bool-del head = %+v; want Deleted=true", h)
	}
	if h := heads["refs/heads/str-del"]; !h.Found || !h.Deleted || h.Commit != "" {
		t.Fatalf("str-del head = %+v; want Deleted=true", h)
	}
}
