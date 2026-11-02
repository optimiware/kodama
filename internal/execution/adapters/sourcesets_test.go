package adapters_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/repository"
)

type srcRepos struct {
	byName map[string]*repository.Repository
}

func (r srcRepos) Get(_ context.Context, sc domain.Scope, id repository.ID) (*repository.Repository, error) {
	for _, x := range r.byName {
		if x.ID == id && x.Scope == sc {
			return x, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (r srcRepos) GetByName(_ context.Context, sc domain.Scope, name string) (*repository.Repository, error) {
	if x, ok := r.byName[name]; ok && x.Scope == sc {
		return x, nil
	}
	return nil, repository.ErrNotFound
}

type srcURLs struct{}

func (srcURLs) CloneURL(_ context.Context, sc domain.Scope, name string) (string, error) {
	return fmt.Sprintf("https://git.example.invalid/git/%s/%s/%s.git", sc.TenantID, sc.ProjectID, name), nil
}

type srcHosts struct {
	values   map[string]string
	retained map[string]string
	taken    map[string]string
	branches []gitv1.CreateBranchRequest
	err      error
}

func (h *srcHosts) ReadRefValues(_ context.Context, _ domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error) {
	if h.err != nil {
		return gitv1.ReadRefValuesResponse{}, h.err
	}
	var out gitv1.ReadRefValuesResponse
	for _, ref := range req.Refs {
		v := gitv1.RefValue{Ref: ref}
		if c, ok := h.values[req.RepositoryID+"|"+ref]; ok {
			v.Commit = &c
		} else if c, ok := h.retained[req.RepositoryID+"|"+ref]; ok {
			v.Commit = &c
		}
		out.Values = append(out.Values, v)
	}
	return out, nil
}

func (h *srcHosts) SetRetainedRef(_ context.Context, _ domain.TenantID, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error) {
	if h.err != nil {
		return gitv1.SetRetainedRefResponse{}, h.err
	}
	key := req.RepositoryID + "|" + req.Ref
	if c, ok := h.retained[key]; ok && c != req.Commit {
		return gitv1.SetRetainedRefResponse{}, &repository.CallError{Outcome: repository.Refused, Err: &gitv1.ContractError{Code: gitv1.CodeRetainedRefExists, Message: "taken"}}
	}
	_, existed := h.retained[key]
	h.retained[key] = req.Commit
	return gitv1.SetRetainedRefResponse{Ref: req.Ref, Commit: req.Commit, Created: !existed}, nil
}

func (h *srcHosts) CreateBranch(_ context.Context, _ domain.TenantID, req gitv1.CreateBranchRequest) (gitv1.CreateBranchResponse, error) {
	h.branches = append(h.branches, req)
	return gitv1.CreateBranchResponse{Ref: req.Ref, Commit: req.Commit, Created: true}, nil
}

type srcPushes struct{ rec gitv1.PushRecord }

func (p srcPushes) Reload(_ context.Context, sc domain.Scope, repo, id string) (gitv1.PushRecord, error) {
	if id != p.rec.PushID {
		return gitv1.PushRecord{}, &gitv1.ContractError{Code: domain.CodeNotFound, Message: "no push"}
	}
	return p.rec, nil
}

// Test_source_sets_adapter: the adapter resolves repositories of the project
// only, pins the exact commit (the push entry's new value, never a tip),
// and retains every pinned commit with a run branch for each write source,
// converging after an earlier try or a taken retained ref number (T-148, PX-23,
// PX-24, GR-13).
func Test_source_sets_adapter(t *testing.T) {
	ctx := context.Background()
	mk := func(id, name string, mut func(*repository.Repository)) *repository.Repository {
		r := &repository.Repository{ID: repository.ID(id), Scope: scope, Name: name, DefaultBranch: "main", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady}
		if mut != nil {
			mut(r)
		}
		return r
	}
	repos := srcRepos{byName: map[string]*repository.Repository{
		"backend":  mk("repo_b", "backend", nil),
		"archived": mk("repo_a", "archived", func(r *repository.Repository) { r.Status = gitv1.StatusArchived }),
		"fresh":    mk("repo_f", "fresh", func(r *repository.Repository) { r.Status = gitv1.StatusProvisioning }),
		"external": mk("repo_x", "external", func(r *repository.Repository) { r.Hosting = gitv1.HostingExternal }),
		"foreign":  {ID: "repo_o", Scope: other, Name: "foreign", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady},
	}}
	hosts := &srcHosts{values: map[string]string{"repo_b|refs/heads/main": strings.Repeat("a", 40), "repo_b|refs/heads/rel": strings.Repeat("b", 40)}, retained: map[string]string{}}
	zero := strings.Repeat("0", 40)
	rec := gitv1.PushRecord{PushID: "push_1", RepositoryID: "repo_b", ReceiveID: "rcv_1", Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: "u"}, GrantID: "gg_1", Status: gitv1.PushApplied,
		CreatedAt: "2026-10-30T10:00:00Z", Entries: []gitv1.PushEntry{{Ref: "refs/heads/main", Old: zero, New: strings.Repeat("c", 40), Kind: gitv1.KindCreate, State: gitv1.EntryApplied}}}
	a, err := adapters.NewSourceSets(repos, srcURLs{}, hosts, srcPushes{rec}, adapters.SourceEndpoint{BaseURL: "https://git.example.invalid/git", CAFile: "/kodama/git/ca"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.NewSourceSets(repos, srcURLs{}, hosts, srcPushes{rec}, adapters.SourceEndpoint{BaseURL: "http://git"}); err == nil {
		t.Fatal("the endpoint must be https")
	}

	t.Run("resolve", func(t *testing.T) {
		res, err := a.Resolve(ctx, scope, []execution.SourceRef{{Name: "backend"}, {ID: "repo_b"}})
		if err != nil || len(res.Repositories) != 2 || res.BaseURL != "https://git.example.invalid/git" || res.CAFile == nil || *res.CAFile != "/kodama/git/ca" ||
			res.Repositories[0].URL != "https://git.example.invalid/git/t_acme/p_inv/backend.git" {
			t.Fatalf("%+v %v", res, err)
		}
		if res, err := a.Resolve(ctx, scope, nil); err != nil || len(res.Repositories) != 0 || res.BaseURL == "" {
			t.Fatalf("a run without sources still pins the endpoint: %+v %v", res, err)
		}
		for name, want := range map[string]error{
			"foreign": execution.ErrNotFound, "ghost": execution.ErrNotFound, "archived": execution.ErrAdmissionRefused,
			"fresh": execution.ErrAdmissionRefused, "external": execution.ErrAdmissionRefused,
		} {
			if _, err := a.Resolve(ctx, scope, []execution.SourceRef{{Name: name}}); !errors.Is(err, want) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("pin", func(t *testing.T) {
		repo := execution.SourceRepository{ID: "repo_b", Name: "backend", DefaultBranch: "main"}
		rel, exact := "refs/heads/rel", strings.Repeat("d", 40)
		got, err := a.Pin(ctx, scope, []execution.PinRequest{
			{Repository: repo}, {Repository: repo, Ref: &rel}, {Repository: repo, Commit: &exact},
			{Repository: repo, Push: &execution.PushPin{PushID: "push_1", Ref: "refs/heads/main"}},
		})
		if err != nil || got[0] != strings.Repeat("a", 40) || got[1] != strings.Repeat("b", 40) || got[2] != exact || got[3] != strings.Repeat("c", 40) {
			t.Fatalf("%v %v", got, err)
		}
		missing := "refs/heads/none"
		for name, r := range map[string]execution.PinRequest{
			"an absent ref":   {Repository: repo, Ref: &missing},
			"unknown push":    {Repository: repo, Push: &execution.PushPin{PushID: "push_9", Ref: "refs/heads/main"}},
			"a ref not in it": {Repository: repo, Push: &execution.PushPin{PushID: "push_1", Ref: "refs/heads/other"}},
			"another repo":    {Repository: execution.SourceRepository{ID: "repo_z", Name: "z"}, Push: &execution.PushPin{PushID: "push_1", Ref: "refs/heads/main"}},
		} {
			if _, err := a.Pin(ctx, scope, []execution.PinRequest{r}); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Errorf("%s: %v", name, err)
			}
		}
		hosts.err = &repository.CallError{Outcome: repository.Unknown}
		if _, err := a.Pin(ctx, scope, []execution.PinRequest{{Repository: repo}}); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("an unknown outcome is never a pin: %v", err)
		}
		hosts.err = nil
	})

	t.Run("retain converges", func(t *testing.T) {
		branch := "refs/heads/kodama/runs/run_1/work"
		set := gitv1.SourceSet{Sources: []gitv1.Source{{RepositoryID: "repo_b", Commit: strings.Repeat("a", 40), Path: ".", Access: gitv1.AccessWrite, Branch: &branch}}}
		// Another module numbered the run's first ref with another commit.
		hosts.retained["repo_b|refs/kodama/runs/run_1/1"] = strings.Repeat("e", 40)
		if err := a.Retain(ctx, scope, "run_1", set); err != nil {
			t.Fatal(err)
		}
		if hosts.retained["repo_b|refs/kodama/runs/run_1/2"] != strings.Repeat("a", 40) || len(hosts.branches) != 1 || hosts.branches[0].Ref != branch || hosts.branches[0].Commit != strings.Repeat("a", 40) {
			t.Fatalf("%+v %+v", hosts.retained, hosts.branches)
		}
		// A repeat (a crash after the first try) retains nothing new and holds the same pin.
		before := len(hosts.retained)
		if err := a.Retain(ctx, scope, "run_1", set); err != nil || len(hosts.retained) != before {
			t.Fatalf("repeat: %v %+v", err, hosts.retained)
		}
		read := gitv1.SourceSet{Sources: []gitv1.Source{{RepositoryID: "repo_b", Commit: strings.Repeat("a", 40), Path: ".", Access: gitv1.AccessRead}}}
		hosts.branches = nil
		if err := a.Retain(ctx, scope, "run_2", read); err != nil || len(hosts.branches) != 0 || hosts.retained["repo_b|refs/kodama/runs/run_2/1"] == "" {
			t.Fatalf("a read source is retained and gets no branch: %v %+v", err, hosts.branches)
		}
		task := "refs/heads/kodama/work/wi_1"
		hosts.branches = nil
		tset := gitv1.SourceSet{Sources: []gitv1.Source{{RepositoryID: "repo_b", Commit: strings.Repeat("a", 40), Path: ".", Access: gitv1.AccessWrite, Branch: &task}}}
		if err := a.Retain(ctx, scope, "run_3", tset); err != nil || len(hosts.branches) != 0 {
			t.Fatalf("the task branch belongs to the workspace: %v %+v", err, hosts.branches)
		}
		hosts.err = &repository.CallError{Outcome: repository.Unavailable}
		if err := a.Retain(ctx, scope, "run_4", read); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("a worker outage is retried, never an assumed pin: %v", err)
		}
	})
}
