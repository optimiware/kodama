package http_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/workspace"
	wshttp "github.com/optimiware/kodama/internal/workspace/http"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// stubService serves a fixed workspace and its associations: the handler's use
// of the repository associations needs no database.
type stubService struct {
	ws     *workspace.Workspace
	assocs []*workspace.RepositoryAssociation
	err    error
}

func (s stubService) Provision(context.Context, workspace.Actor, domain.Scope, domain.WorkItemID) (*workspace.Workspace, bool, error) {
	return s.ws, false, s.err
}
func (s stubService) Get(context.Context, workspace.Actor, domain.Scope, workspace.ID) (*workspace.Workspace, error) {
	return s.ws, s.err
}
func (s stubService) Sync(context.Context, workspace.Actor, domain.Scope, workspace.ID, domain.Version) (*workspace.Workspace, error) {
	return s.ws, s.err
}
func (s stubService) Archive(context.Context, workspace.Actor, domain.Scope, workspace.ID, domain.Version) (*workspace.Workspace, error) {
	return s.ws, s.err
}
func (s stubService) Restore(context.Context, workspace.Actor, domain.Scope, workspace.ID, domain.Version) (*workspace.Workspace, error) {
	return s.ws, s.err
}
func (s stubService) Repositories(context.Context, workspace.Actor, domain.Scope, workspace.ID) ([]*workspace.RepositoryAssociation, error) {
	return s.assocs, nil
}

func stubHarness(t *testing.T, svc stubService) *harness {
	t.Helper()
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: tokenAuth{}, Authorizer: routerAuthz{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := wshttp.NewHandler(svc)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Register(rt); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, rt: rt, api: loadSchemas(t)}
}

// Test_workspace_http_repositories: the workspace resource shows the repository
// associations of an internally hosted workspace and none for an external one,
// and the fragment's schema accepts exactly that.
func Test_workspace_http_repositories(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	a40, b40 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	internal, err := workspace.NewInternal(workspace.NewInternalParams{ID: "ws_1", Scope: wt.ScopeA, WorkItemID: "wi_1", Primary: "repo_app",
		URL: "https://git.example.test/git/acme/repo_app.git", CheckoutSecretRef: "kodama-git-read:repo_app"}, now)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := workspace.NewAssociation(internal, "repo_app", workspace.Commit(a40), now)
	docs, _ := workspace.NewAssociation(internal, "repo_docs", workspace.Commit(b40), now)
	if _, err := app.MarkCreated(now); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.RecordError(workspace.ReasonBranchExists, now); err != nil {
		t.Fatal(err)
	}
	if err := internal.CompleteProvisioning(workspace.Commit(a40), now); err != nil {
		t.Fatal(err)
	}
	external, err := workspace.New(workspace.NewParams{ID: "ws_2", Scope: wt.ScopeA, WorkItemID: "wi_2", RepositoryID: "repo_1",
		RepositoryURL: "https://git.example.test/o/r.git", CheckoutSecretRef: "secret://c", PublisherSecretRef: "secret://p"}, now)
	if err != nil {
		t.Fatal(err)
	}
	get := func(h *harness, id string) response {
		return h.do("GET", pA+"/workspaces/"+id, wt.Engineer.ID, nil, "")
	}

	t.Run("an internal workspace lists its associations", func(t *testing.T) {
		h := stubHarness(t, stubService{ws: internal, assocs: []*workspace.RepositoryAssociation{app, docs}})
		r := get(h, "ws_1")
		if r.Code != 200 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if err := h.api.validate(t, "workspace.json", "Workspace", r.Body.Bytes()); err != nil {
			t.Fatalf("the response does not match the schema: %v\n%s", err, r.Body)
		}
		w := r.workspace(t)
		if w.Hosting != wshttp.Internal || w.RepositoryId != "repo_app" || w.Branch != "refs/heads/kodama/work/wi_1" || w.AcceptedCommit == nil || *w.AcceptedCommit != a40 || len(w.Repositories) != 2 {
			t.Fatalf("%+v", w)
		}
		first, second := w.Repositories[0], w.Repositories[1]
		if first.RepositoryId != "repo_app" || first.State != wshttp.Created || first.AcceptedCommit == nil || *first.AcceptedCommit != a40 || first.BaseCommit != a40 || first.LastError != nil ||
			first.TaskBranch != "refs/heads/kodama/work/wi_1" {
			t.Fatalf("%+v", first)
		}
		if second.RepositoryId != "repo_docs" || second.State != wshttp.Intent || second.AcceptedCommit != nil || second.LastError == nil || *second.LastError != workspace.ReasonBranchExists {
			t.Fatalf("%+v", second)
		}
		// Nothing of the checkout reference or the publisher placeholder is exposed.
		if strings.Contains(r.Body.String(), "kodama-git-read") || strings.Contains(r.Body.String(), workspace.NoPublisherRef) || strings.Contains(r.Body.String(), "git.example.test") {
			t.Fatalf("an internal detail leaked: %s", r.Body)
		}
	})

	t.Run("an external workspace has the same shape and no associations", func(t *testing.T) {
		h := stubHarness(t, stubService{ws: external})
		r := get(h, "ws_2")
		if r.Code != 200 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if err := h.api.validate(t, "workspace.json", "Workspace", r.Body.Bytes()); err != nil {
			t.Fatalf("%v\n%s", err, r.Body)
		}
		var raw map[string]any
		if err := json.Unmarshal(r.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if raw["hosting"] != "external" || raw["repositoryId"] != "repo_1" {
			t.Fatalf("%v", raw)
		}
		if list, ok := raw["repositories"].([]any); !ok || len(list) != 0 {
			t.Fatalf("repositories must be an explicit empty list: %v", raw["repositories"])
		}
	})

	t.Run("the other hosting's operation is a state conflict", func(t *testing.T) {
		h := stubHarness(t, stubService{ws: internal, err: workspace.ErrWrongHosting})
		r := h.do("POST", pA+"/workspaces/ws_1/archive", wt.Engineer.ID, ifMatch(1), "")
		if r.Code != 409 || r.errorCode(t) != string(wshttp.CodeStateConflict) {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
	})

	t.Run("the schema refuses a malformed association", func(t *testing.T) {
		h := stubHarness(t, stubService{ws: internal})
		base := func(assoc string) []byte {
			return []byte(`{"id":"a","hosting":"internal","workItemId":"w","repositoryId":"r","branch":"refs/heads/x","acceptedCommit":null,"repositories":[` + assoc + `],"state":"available",` +
				`"writerAttemptId":null,"publication":null,"archiveRequestedAt":null,"lastError":null,"version":1,"createdAt":"2026-10-19T10:00:00Z","updatedAt":"2026-10-19T10:00:00Z"}`)
		}
		good := `{"repositoryId":"r","taskBranch":"refs/heads/kodama/work/w","baseCommit":"` + a40 + `","acceptedCommit":null,"state":"intent","lastError":null,"createdAt":"2026-10-19T10:00:00Z","updatedAt":"2026-10-19T10:00:00Z"}`
		if err := h.api.validate(t, "workspace.json", "Workspace", base(good)); err != nil {
			t.Fatalf("a valid association: %v", err)
		}
		for name, doc := range map[string]string{
			"unknown state":    strings.Replace(good, `"intent"`, `"failed"`, 1),
			"not a task":       strings.Replace(good, `refs/heads/kodama/work/w`, `refs/heads/main`, 1),
			"free-text reason": strings.Replace(good, `"lastError":null`, `"lastError":"connection refused to 10.0.0.1"`, 1),
			"extra member":     strings.Replace(good, `"state"`, `"url":"https://x","state"`, 1),
			"short commit":     strings.Replace(good, a40, "abc", 1),
		} {
			if err := h.api.validate(t, "workspace.json", "Workspace", base(doc)); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		for name, doc := range map[string]string{
			"unknown hosting": strings.Replace(string(base(good)), `"internal"`, `"mirror"`, 1),
			"no repositories": strings.Replace(string(base(good)), `"repositories":[`+good+`],`, ``, 1),
		} {
			if err := h.api.validate(t, "workspace.json", "Workspace", []byte(doc)); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
	})
}
