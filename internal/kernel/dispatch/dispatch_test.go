package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/bus"
	"github.com/optimiware/kodama/internal/kernel/dispatch"
	"github.com/optimiware/kodama/internal/kernel/pipeline"
	"github.com/optimiware/kodama/internal/kernel/store"
)

func TestMatches(t *testing.T) {
	const (
		main    = "refs/heads/main"
		release = "refs/heads/release/1.0"
		feature = "refs/heads/feature/a/b"
		tag     = "refs/tags/v1.2"
	)
	g := func(branches, tags, events []string) pipeline.GitTrigger {
		return pipeline.GitTrigger{Branches: branches, Tags: tags, Events: events}
	}
	for _, c := range []struct {
		name string
		t    pipeline.GitTrigger
		ref  string
		want bool
	}{
		{"no trigger", g(nil, nil, nil), main, false},
		{"short branch name", g([]string{"main"}, nil, nil), main, true},
		{"short name is exact", g([]string{"main"}, nil, nil), "refs/heads/main2", false},
		{"full ref", g([]string{"refs/heads/main"}, nil, nil), main, true},
		{"wildcard", g([]string{"release/*"}, nil, nil), release, true},
		{"full wildcard", g([]string{"refs/heads/release/*"}, nil, nil), release, true},
		{"wildcard crosses slashes", g([]string{"feature/*"}, nil, nil), feature, true},
		{"wildcard needs the prefix", g([]string{"release/*"}, nil, nil), main, false},
		{"star alone", g([]string{"*"}, nil, nil), feature, true},
		{"star in the middle", g([]string{"feature/*/b"}, nil, nil), feature, true},
		{"star in the middle misses", g([]string{"feature/*/c"}, nil, nil), feature, false},
		{"branches never match a tag", g([]string{"*"}, nil, []string{"push", "tag"}), tag, false},
		{"tag needs the tag event", g(nil, []string{"v*"}, nil), tag, false},
		{"tag with the tag event", g(nil, []string{"v*"}, []string{"tag"}), tag, true},
		{"tag pattern is a tag pattern", g(nil, []string{"v*"}, []string{"tag"}), "refs/heads/v1", false},
		{"full tag ref", g(nil, []string{"refs/tags/v1.2"}, []string{"tag"}), tag, true},
		{"a push needs the push event", g([]string{"main"}, nil, []string{"tag"}), main, false},
		{"merge is not a push", g([]string{"main"}, nil, []string{"merge"}), main, false},
		{"both events", g([]string{"main"}, []string{"v*"}, []string{"push", "tag"}), tag, true},
		{"a server ref is no event", g([]string{"*"}, []string{"*"}, []string{"push", "tag"}), "refs/kodama/runs/run_1/1", false},
	} {
		if got := dispatch.Matches(c.t, c.ref); got != c.want {
			t.Errorf("%s: Matches(%+v, %s) = %v, want %v", c.name, c.t, c.ref, got, c.want)
		}
	}
}

func TestTriggerKeyIsPerEventAndRef(t *testing.T) {
	if dispatch.TriggerKey("e1", "refs/heads/a") == dispatch.TriggerKey("e1", "refs/heads/b") ||
		dispatch.TriggerKey("e1", "refs/heads/a") == dispatch.TriggerKey("e2", "refs/heads/a") ||
		dispatch.TriggerKey("e1", "refs/heads/a") != dispatch.TriggerKey("e1", "refs/heads/a") {
		t.Fatal("the trigger key must be a function of exactly the event and the ref")
	}
}

func TestPlanPinsTheNewCommitAndSkipsDeletes(t *testing.T) {
	z, a, b := "0000000000000000000000000000000000000000", "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	def := json.RawMessage(`{"name":"build","triggers":{"git":{"branches":["*"]}},"steps":[{"name":"test","image":"x","commands":["c"]},{"name":"lint","image":"y","commands":["c"],"depends_on":["test"]}]}`)
	plan := dispatch.Plan("evt_1", "repo_1", []eventsv1.GitRefUpdate{
		{Ref: "refs/heads/a", Old: z, New: a, Kind: eventsv1.GitKindCreate},
		{Ref: "refs/heads/b", Old: a, New: z, Kind: eventsv1.GitKindDelete},
		{Ref: "refs/heads/c", Old: a, New: b, Kind: eventsv1.GitKindFastForward},
	}, slog.New(slog.DiscardHandler))
	got, err := plan([]store.Pipeline{{ID: "pl_1", Name: "build", Definition: def}, {ID: "pl_2", Name: "broken", Definition: json.RawMessage(`{"steps":"x"}`)}})
	if err != nil || len(got) != 2 {
		t.Fatalf("plan = %+v, %v", got, err)
	}
	for i, want := range []struct{ ref, commit string }{{"refs/heads/a", a}, {"refs/heads/c", b}} {
		var src []gitv1.Source
		if err := json.Unmarshal(got[i].Sources, &src); err != nil || len(src) != 1 || src[0].Commit != want.commit || src[0].RepositoryID != "repo_1" ||
			src[0].Path != "." || src[0].Access != gitv1.AccessRead || src[0].Branch != nil {
			t.Fatalf("sources %d = %s, %v", i, got[i].Sources, err)
		}
		if got[i].TriggerKey != dispatch.TriggerKey("evt_1", want.ref) || got[i].PipelineID != "pl_1" || got[i].Run.ID == "" ||
			len(got[i].Steps) != 2 || got[i].Steps[0].StepKey != "test" || got[i].Steps[1].StepKey != "lint" || len(got[i].Run.ExecutionOrder) != 2 {
			t.Fatalf("admission %d = %+v", i, got[i])
		}
	}
	if got[0].Run.ID == got[1].Run.ID {
		t.Fatal("two runs share an ID")
	}
}

type memStore struct {
	*store.MemoryGit
	err error
}

func (m memStore) GetRepository(ctx context.Context, tenant domain.TenantID, id string) (store.Repository, error) {
	if m.err != nil {
		return store.Repository{}, m.err
	}
	return m.MemoryGit.GetRepository(ctx, tenant, id)
}

type filesFunc func(gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error)

func (f filesFunc) ReadFiles(_ context.Context, _ domain.TenantID, r gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error) {
	return f(r)
}

func message(t *testing.T, repo, project string, ev eventsv1.GitRefsUpdated) bus.Message {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return bus.Message{ID: "gp_" + ev.ReceiveID, TenantID: "t_acme", ProjectID: domain.ProjectID(project), Type: eventsv1.TypeGitRefsUpdated,
		ResourceType: "git_push", ResourceID: ev.ReceiveID, OccurredAt: time.Now().UTC(), Data: raw}
}

// TestHandleSettlesWhatRedeliveryCannotFixAndRetriesWhatItCan: a malformed
// payload, an unknown repository, an archived one and an event of another type are
// settled without effect; a worker that cannot be reached leaves the delivery to
// repeat and changes nothing.
func TestHandleSettlesWhatRedeliveryCannotFixAndRetriesWhatItCan(t *testing.T) {
	mem := store.NewMemoryGit()
	mem.AddTenant("t_acme", "acme")
	mem.AddProject("t_acme", "p_erp", "erp")
	ctx := t.Context()
	if _, _, err := mem.CreateRepository(ctx, store.Repository{TenantID: "t_acme", ProjectID: "p_erp", ID: "repo_1", Name: "backend", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	var readErr error
	d, err := dispatch.New(dispatch.Config{Store: memStore{MemoryGit: mem}, Files: filesFunc(func(gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error) {
		reads++
		return gitv1.ReadFilesResponse{Files: []gitv1.DefinitionFile{}}, readErr
	})})
	if err != nil {
		t.Fatal(err)
	}
	z, a := "0000000000000000000000000000000000000000", "1111111111111111111111111111111111111111"
	user := eventsv1.GitRefActor{Type: "user", ID: "u_1"}
	onMain := eventsv1.GitRefsUpdated{ReceiveID: "rcv_1", RepositoryID: "repo_1", Actor: user, Updates: []eventsv1.GitRefUpdate{{Ref: "refs/heads/main", Old: z, New: a, Kind: "create"}}}

	for name, m := range map[string]bus.Message{
		"another type":       {ID: "e", TenantID: "t_acme", Type: "repository.created"},
		"malformed payload":  {ID: "e", TenantID: "t_acme", ProjectID: "p_erp", Type: eventsv1.TypeGitRefsUpdated, Data: json.RawMessage(`{"receiveId":"rcv_1"}`)},
		"unknown repository": message(t, "repo_x", "p_erp", eventsv1.GitRefsUpdated{ReceiveID: "rcv_2", RepositoryID: "repo_x", Actor: user, Updates: onMain.Updates}),
		"another project":    message(t, "repo_1", "p_other", onMain),
		"no project":         message(t, "repo_1", "", onMain),
	} {
		if err := d.Handle(ctx, m); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if reads != 0 {
		t.Fatalf("a settled event read files %d times", reads)
	}
	if got := len(mem.EventsOfType(store.EventPipelinesIndexed)); got != 0 {
		t.Fatalf("a settled event left %d markers", got)
	}

	// The worker is down: the delivery repeats and nothing is recorded.
	readErr = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
	if err := d.Handle(ctx, message(t, "repo_1", "p_erp", onMain)); err == nil {
		t.Fatal("an unreachable worker settled the delivery")
	}
	if got := len(mem.EventsOfType(store.EventPipelinesIndexed)); got != 0 {
		t.Fatalf("a failed delivery left %d markers", got)
	}
	// Back up: the same event is dispatched once.
	readErr = nil
	for i := 0; i < 2; i++ {
		if err := d.Handle(ctx, message(t, "repo_1", "p_erp", onMain)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(mem.EventsOfType(store.EventPipelinesIndexed)); got != 1 {
		t.Fatalf("markers = %d", got)
	}

	// A completed dispatch is settled before any file is read: a replay does not
	// depend on the worker.
	readsBefore := reads
	readErr = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
	if err := d.Handle(ctx, message(t, "repo_1", "p_erp", onMain)); err != nil || reads != readsBefore {
		t.Fatalf("a replay with the worker down: %v (%d reads)", err, reads-readsBefore)
	}
	readErr = nil

	// A store failure is retried too.
	broken, _ := dispatch.New(dispatch.Config{Store: memStore{MemoryGit: mem, err: errors.New("db down")}, Files: filesFunc(func(gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error) {
		return gitv1.ReadFilesResponse{Files: []gitv1.DefinitionFile{}}, nil
	})})
	fresh := onMain
	fresh.ReceiveID = "rcv_fresh"
	if err := broken.Handle(ctx, message(t, "repo_1", "p_erp", fresh)); err == nil {
		t.Fatal("a store failure settled the delivery")
	}
	if _, err := dispatch.New(dispatch.Config{}); err == nil {
		t.Fatal("a dispatcher without a store")
	}
}
