package api_test

import (
	"net/http"
	"strconv"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/api"
	"github.com/optimiware/kodama/internal/kernel/store"
)

// The documented route names the project by key. A person bound only to that
// project (no tenant-wide role) is authorized through the resolved project ID.
func TestGitRoutesResolveTheProjectKeyBeforeAuthorizing(t *testing.T) {
	e := newEnv(t)
	byKey := "/api/v1/tenants/acme/projects/erp/repositories"
	byID := "/api/v1/tenants/acme/projects/p_erp/repositories"

	r := e.do(http.MethodPost, byKey, "tok-eng", map[string]any{"name": "backend"})
	if r.status != http.StatusCreated || r.body["projectId"] != "p_erp" || r.body["path"] != "acme/erp/backend" {
		t.Fatalf("a project-scoped engineer creating by key = %d %s", r.status, r.raw)
	}
	id := str(r.body["id"])
	for name, path := range map[string]string{"list by key": byKey, "list by ID": byID, "item by key": byKey + "/" + id, "item by ID": byID + "/" + id} {
		if r := e.do(http.MethodGet, path, "tok-viewer", nil); r.status != http.StatusOK {
			t.Errorf("a project-scoped reader, %s = %d %s", name, r.status, r.raw)
		}
	}
	// The reader cannot write; a reader of another project and an outsider see nothing.
	if r := e.do(http.MethodPost, byKey, "tok-viewer", map[string]any{"name": "other"}); r.status != http.StatusForbidden {
		t.Errorf("a project-scoped reader creating = %d", r.status)
	}
	e.mem.Grant(tenantID, "u_web", "p_web", "engineer")
	tokens["tok-web"] = api.Principal{UserID: "u_web"}
	defer delete(tokens, "tok-web")
	for name, path := range map[string]string{"by key": byKey, "by ID": byID, "item": byKey + "/" + id} {
		if r := e.do(http.MethodGet, path, "tok-web", nil); r.status != http.StatusNotFound {
			t.Errorf("an engineer of another project, %s = %d", name, r.status)
		}
	}
	if r := e.do(http.MethodGet, "/api/v1/tenants/acme/projects/nope/repositories", "tok-eng", nil); r.status != http.StatusNotFound {
		t.Errorf("an unknown project key = %d", r.status)
	}
	// The same engineer works by key in the credential exchange and the Git path.
	repo := r.body
	if _, pw := e.mustCredential("tok-eng", repo, false); pw == "" {
		t.Fatal("no credential")
	} else if g := e.gitGet("/git/acme/erp/backend.git/info/refs?service=git-receive-pack", pw, http.MethodGet); g.status != http.StatusOK {
		t.Errorf("push access of the project-scoped engineer = %d", g.status)
	}
}

// A default branch change puts the repository back into provisioning so that the
// worker re-applies HEAD, and is refused while it cannot be applied.
func TestGitDefaultBranchChangeReconfiguresTheRepository(t *testing.T) {
	e := newEnv(t)
	repo := e.createRepo("tok-eng", "p_erp", "backend", nil)
	id, path := str(repo["id"]), reposPath+"/"+str(repo["id"])
	version := func(r reply) string { return strconv.Itoa(int(r.body["version"].(float64))) }

	// The worker is unreachable: the change is visible as provisioning with its reason,
	// and the assignment poll carries the new branch.
	e.workers.initErr = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
	seed, _ := e.mustCredential("tok-eng", repo, true)
	got := e.do(http.MethodGet, path, "tok-eng", nil)
	patched := e.do(http.MethodPatch, path, "tok-eng", map[string]any{"defaultBranch": "trunk"}, "If-Match", version(got))
	if patched.status != http.StatusOK || patched.body["status"] != "provisioning" || patched.body["reason"] != store.ReasonReconfiguration || patched.body["defaultBranch"] != "trunk" {
		t.Fatalf("patch = %d %s", patched.status, patched.raw)
	}
	assigned, err := e.control().Assignments(t.Context(), tenantID)
	if err != nil || len(assigned.Items) != 1 || assigned.Items[0].Kind != gitv1.AssignInitialize || assigned.Items[0].DefaultBranch != "trunk" {
		t.Fatalf("assignments = %+v, %v", assigned, err)
	}
	// Meanwhile nothing is issued or authorized, and the old seed is gone.
	if r := e.credential("tok-eng", patched.body, false); r.status != http.StatusConflict || errCode(r) != "repository_not_ready" {
		t.Fatalf("credential while reconfiguring = %d %s", r.status, r.raw)
	}
	if _, err := e.authorize(seed, id, []gitv1.RefUpdate{create("refs/heads/main", c1)}, factsFor(true, create("refs/heads/main", c1))); contractCode(err) != domain.CodeInvalidCredential {
		t.Fatalf("the seed of the old branch authorizes: %v", err)
	}
	// The worker's confirmation returns it to ready, with a state change and no second creation.
	created := len(e.mem.EventsOfType("repository.created"))
	resp, err := e.control().Initialized(t.Context(), tenantID, gitv1.InitializedRequest{RepositoryID: id, DefaultBranch: "trunk", Empty: true})
	if err != nil || resp.Status != gitv1.StatusReady {
		t.Fatalf("initialized = %+v, %v", resp, err)
	}
	if len(e.mem.EventsOfType("repository.created")) != created || len(e.mem.EventsOfType("repository.state_changed")) != 1 {
		t.Fatalf("events: created %d, state_changed %d", len(e.mem.EventsOfType("repository.created")), len(e.mem.EventsOfType("repository.state_changed")))
	}
	e.workers.initErr = nil

	// A reachable worker finishes the change within the request.
	got = e.do(http.MethodGet, path, "tok-eng", nil)
	again := e.do(http.MethodPatch, path, "tok-eng", map[string]any{"defaultBranch": "develop"}, "If-Match", version(got))
	if again.status != http.StatusOK || again.body["status"] != "ready" || again.body["defaultBranch"] != "develop" {
		t.Fatalf("patch with a reachable worker = %d %s", again.status, again.raw)
	}

	// Refused while a seed receive is unacknowledged, and once the repository has refs.
	seedGrant, _ := e.mustCredential("tok-eng", again.body, true)
	main := create("refs/heads/develop", c1)
	receive := gitv1.NewReceiveID(seedGrant)
	if resp, err := e.authorizeAs(receive, seedGrant, id, []gitv1.RefUpdate{main}, factsFor(true, main)); err != nil || reasons(resp)[0] != "allow" {
		t.Fatalf("seed = %v, %v", reasons(resp), err)
	}
	got = e.do(http.MethodGet, path, "tok-eng", nil)
	if r := e.do(http.MethodPatch, path, "tok-eng", map[string]any{"defaultBranch": "other"}, "If-Match", version(got)); r.status != http.StatusConflict || errCode(r) != "seed_pending" {
		t.Fatalf("patch with an unacknowledged seed = %d %s", r.status, r.raw)
	}
	if _, err := e.control().Outcome(t.Context(), tenantID, receive, gitv1.OutcomeRequest{ReceiveID: receive, Applied: []gitv1.RefUpdate{main}}); err != nil {
		t.Fatal(err)
	}
	if r := e.do(http.MethodPatch, path, "tok-eng", map[string]any{"defaultBranch": "other"}, "If-Match", version(got)); r.status != http.StatusConflict || errCode(r) != "repository_not_empty" {
		t.Fatalf("patch of a repository with refs = %d %s", r.status, r.raw)
	}
	// A policy-only patch never reconfigures.
	if r := e.do(http.MethodPatch, path, "tok-eng", map[string]any{"policy": map[string]any{"directPushToDefault": true}}, "If-Match", version(got)); r.status != http.StatusOK || r.body["status"] != "ready" {
		t.Fatalf("policy patch = %d %s", r.status, r.raw)
	}
}

// Assignment selection pages and rotates: no repository starves behind the first
// page, archived repositories are still reconciled, provisioning comes first.
func TestGitAssignmentsRotateAndIncludeArchived(t *testing.T) {
	e := newEnv(t)
	e.control().SetAssignmentPage(2)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, str(e.createRepo("tok-eng", "p_erp", "r"+strconv.Itoa(i), nil)["id"]))
	}
	// Archive one: its journal is still recovered.
	if r := e.do(http.MethodPost, reposPath+"/"+ids[1]+"/archive", "tok-eng", nil); r.status != http.StatusOK {
		t.Fatal(r.status)
	}
	seen := map[string]int{}
	for poll := 0; poll < 6; poll++ {
		resp, err := e.control().Assignments(t.Context(), tenantID)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Items) > 2 {
			t.Fatalf("a poll returned %d items with a page of 2", len(resp.Items))
		}
		for _, a := range resp.Items {
			if a.Kind != gitv1.AssignReconcile || a.JournalAck == nil {
				t.Fatalf("assignment %+v", a)
			}
			seen[a.RepositoryID]++
		}
	}
	for _, id := range ids {
		if seen[id] == 0 {
			t.Errorf("repository %s never appeared in an assignment poll (%v): it starves", id, seen)
		}
	}
	if seen[ids[1]] == 0 {
		t.Fatal("the archived repository is not reconciled")
	}

	// Provisioning is never starved by the rotation.
	e.workers.initErr = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
	slow := str(e.createRepo("tok-eng", "p_erp", "zzz-last", nil)["id"])
	e.workers.initErr = nil
	for poll := 0; poll < 3; poll++ {
		resp, _ := e.control().Assignments(t.Context(), tenantID)
		if len(resp.Items) == 0 || resp.Items[0].RepositoryID != slow || resp.Items[0].Kind != gitv1.AssignInitialize {
			t.Fatalf("poll %d: provisioning repository is not first: %+v", poll, resp.Items)
		}
	}
}
