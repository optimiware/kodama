package pipelines_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/pipeline"
	"github.com/optimiware/kodama/internal/shell/pipelines"
)

// Tenants every fixture knows.
const (
	tenantA domain.TenantID = "tenant-a"
	tenantB domain.TenantID = "tenant-b"
)

// fixture is what the contract suite needs from an adapter: a registry plus a
// way to make fresh projects and repositories exist (the registry does not own
// them). The memory and PostgreSQL adapters both run this one suite.
type fixture struct {
	svc *pipelines.Service
	// project returns a fresh project of the tenant; repo a fresh repository of it.
	project func(t *testing.T, tenant domain.TenantID) domain.Scope
	repo    func(t *testing.T, scope domain.Scope) string
}

var counter atomic.Int64

func fresh(prefix string) string { return fmt.Sprintf("%s%d", prefix, counter.Add(1)) }

func commit(n int) string { return fmt.Sprintf("%040x", n) }

// doc is a valid pipeline document; marker distinguishes two documents of one name.
func doc(name, marker string) []byte {
	return []byte(fmt.Sprintf("name: %s\nsteps:\n  - name: build\n    image: golang:1.27\n    commands:\n      - echo %s\n", name, marker))
}

func decode(t *testing.T, raw json.RawMessage) pipeline.Document {
	t.Helper()
	var d pipeline.Document
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("stored document is not a pipeline document: %v", err)
	}
	return d
}

func index(f fixture, scope domain.Scope, repo, event string, n int, document []byte) pipelines.IndexRequest {
	return pipelines.IndexRequest{Scope: scope, RepositoryID: repo, EventID: event, Ref: "refs/heads/main", Commit: commit(n), Document: document}
}

// runProjectUnique is the project-unique name contract: one name, one project,
// one immutable ID; a duplicate is refused by the registry; scope is enforced.
func runProjectUnique(t *testing.T, f fixture) {
	ctx := t.Context()

	t.Run("register_resolve_get", func(t *testing.T) {
		scope := f.project(t, tenantA)
		res, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("deploy", "one")})
		if err != nil || !res.Created || res.Replayed {
			t.Fatalf("register = %+v, %v", res, err)
		}
		d := res.Definition
		if d.ID == "" || d.Name != "deploy" || d.Source.Kind != pipelines.SourceAPI || d.Version != 1 || d.Scope != scope || d.Commit != "" {
			t.Fatalf("definition = %+v", d)
		}
		id, err := f.svc.Resolve(ctx, scope, "deploy")
		if err != nil || id != d.ID {
			t.Fatalf("resolve = %q, %v, want %q", id, err, d.ID)
		}
		got, err := f.svc.Get(ctx, scope, d.ID)
		if err != nil || got.ID != d.ID || decode(t, got.Document).Steps[0].Commands[0] != "echo one" {
			t.Fatalf("get = %+v, %v", got, err)
		}
		if _, err := f.svc.Resolve(ctx, scope, "missing"); !errors.Is(err, pipelines.ErrNotFound) {
			t.Fatalf("resolve of an unknown name: %v", err)
		}
		if _, err := f.svc.Resolve(ctx, scope, "Not A Name"); !errors.Is(err, pipelines.ErrNotFound) {
			t.Fatalf("resolve of a malformed name: %v", err)
		}
	})

	t.Run("duplicate_name_is_refused_by_the_registry", func(t *testing.T) {
		scope := f.project(t, tenantA)
		first, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("deploy", "one")})
		if err != nil {
			t.Fatal(err)
		}
		// A different document and the identical one are both duplicates: the
		// name is taken, and registering never replaces.
		for _, marker := range []string{"two", "one"} {
			_, err = f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("deploy", marker)})
			if !errors.Is(err, pipelines.ErrDuplicateName) {
				t.Fatalf("duplicate (%s) = %v, want ErrDuplicateName", marker, err)
			}
			var de *domain.Error
			if !errors.As(err, &de) || de.Code != pipelines.CodeDuplicateName {
				t.Fatalf("duplicate error code = %v", err)
			}
		}
		got, err := f.svc.Get(ctx, scope, first.Definition.ID)
		if err != nil || got.Version != 1 || decode(t, got.Document).Steps[0].Commands[0] != "echo one" {
			t.Fatalf("the original was disturbed: %+v, %v", got, err)
		}
		if id, _ := f.svc.Resolve(ctx, scope, "deploy"); id != first.Definition.ID {
			t.Fatalf("the name no longer resolves to the original: %q", id)
		}
		if c, err := f.svc.Conflicts(ctx, scope); err != nil || len(c) != 0 {
			t.Fatalf("a duplicate is not a source conflict: %+v, %v", c, err)
		}
	})

	t.Run("concurrent_registrations_have_one_winner", func(t *testing.T) {
		scope := f.project(t, tenantA)
		const n = 12
		var wg sync.WaitGroup
		results := make([]error, n)
		ids := make([]string, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("race", fmt.Sprint("m", i))})
				results[i], ids[i] = err, res.Definition.ID
			}()
		}
		wg.Wait()
		winners := 0
		for i, err := range results {
			switch {
			case err == nil:
				winners++
			case !errors.Is(err, pipelines.ErrDuplicateName):
				t.Fatalf("registration %d: %v", i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("%d registrations won the name, want 1", winners)
		}
	})

	t.Run("names_are_unique_per_project_not_wider", func(t *testing.T) {
		a1, a2 := f.project(t, tenantA), f.project(t, tenantA)
		b1 := f.project(t, tenantB)
		ids := map[string]bool{}
		for _, scope := range []domain.Scope{a1, a2, b1} {
			res, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("shared", "x")})
			if err != nil {
				t.Fatalf("%s: %v", scope, err)
			}
			if ids[res.Definition.ID] {
				t.Fatalf("two pipelines share ID %s", res.Definition.ID)
			}
			ids[res.Definition.ID] = true
		}
	})

	t.Run("id_is_immutable_across_updates", func(t *testing.T) {
		scope := f.project(t, tenantA)
		repo := f.repo(t, scope)
		first, err := f.svc.Index(ctx, index(f, scope, repo, "e1", 1, doc("build", "v1")))
		if err != nil || !first.Created {
			t.Fatalf("first index = %+v, %v", first, err)
		}
		second, err := f.svc.Index(ctx, index(f, scope, repo, "e2", 2, doc("build", "v2")))
		if err != nil || !second.Updated || second.Created {
			t.Fatalf("second index = %+v, %v", second, err)
		}
		if second.Definition.ID != first.Definition.ID || second.Definition.Version != 2 || second.Definition.Commit != commit(2) {
			t.Fatalf("update = %+v after %+v", second.Definition, first.Definition)
		}
		if id, err := f.svc.Resolve(ctx, scope, "build"); err != nil || id != first.Definition.ID {
			t.Fatalf("resolve = %q, %v", id, err)
		}
	})

	t.Run("scope_is_enforced", func(t *testing.T) {
		a := f.project(t, tenantA)
		repo := f.repo(t, a)
		res, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: a, Document: doc("secret-flow", "x")})
		if err != nil {
			t.Fatal(err)
		}
		gitRes, err := f.svc.Index(ctx, index(f, a, repo, "e1", 1, doc("from-git", "x")))
		if err != nil {
			t.Fatal(err)
		}

		sibling := f.project(t, tenantA) // another project of the same tenant
		foreign := f.project(t, tenantB) // another tenant
		unknown := domain.Scope{TenantID: tenantA, ProjectID: domain.ProjectID(fresh("nope"))}
		crossTenant := domain.Scope{TenantID: tenantB, ProjectID: a.ProjectID} // A's project ID under B
		for name, scope := range map[string]domain.Scope{"sibling": sibling, "foreign": foreign, "unknown": unknown, "cross_tenant": crossTenant} {
			if _, err := f.svc.Resolve(ctx, scope, "secret-flow"); !errors.Is(err, pipelines.ErrNotFound) {
				t.Errorf("%s: resolve = %v, want not found", name, err)
			}
			for _, id := range []string{res.Definition.ID, gitRes.Definition.ID} {
				if _, err := f.svc.Get(ctx, scope, id); !errors.Is(err, pipelines.ErrNotFound) {
					t.Errorf("%s: get %s = %v, want not found", name, id, err)
				}
			}
		}
		// Writes into a project that is not the caller's own are not-found too,
		// and create nothing.
		for name, scope := range map[string]domain.Scope{"unknown": unknown, "cross_tenant": crossTenant} {
			if _, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("intruder", "x")}); !errors.Is(err, pipelines.ErrNotFound) {
				t.Errorf("%s: register = %v, want not found", name, err)
			}
			if _, err := f.svc.Index(ctx, index(f, scope, repo, "e9", 9, doc("intruder", "x"))); !errors.Is(err, pipelines.ErrNotFound) {
				t.Errorf("%s: index = %v, want not found", name, err)
			}
			if _, err := f.svc.Conflicts(ctx, scope); !errors.Is(err, pipelines.ErrNotFound) {
				t.Errorf("%s: conflicts = %v, want not found", name, err)
			}
		}
		if _, err := f.svc.Resolve(ctx, a, "intruder"); !errors.Is(err, pipelines.ErrNotFound) {
			t.Errorf("a refused write left a pipeline behind: %v", err)
		}
		// A repository of another project, another tenant or no project at all
		// is not a source for this project.
		for name, repoID := range map[string]string{"sibling_repo": f.repo(t, sibling), "foreign_repo": f.repo(t, foreign), "unknown_repo": fresh("norepo")} {
			if _, err := f.svc.Index(ctx, index(f, a, repoID, "e2", 2, doc("stolen", "x"))); !errors.Is(err, pipelines.ErrNotFound) {
				t.Errorf("%s: index = %v, want not found", name, err)
			}
		}
		if _, err := f.svc.Resolve(ctx, a, "stolen"); !errors.Is(err, pipelines.ErrNotFound) {
			t.Errorf("a refused index left a pipeline behind: %v", err)
		}
		// Malformed scope or identifiers are refused before any lookup.
		if _, err := f.svc.Register(ctx, pipelines.RegisterRequest{Document: doc("x", "x")}); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("empty scope = %v", err)
		}
		if _, err := f.svc.Get(ctx, a, "bad id!"); !errors.Is(err, pipelines.ErrNotFound) {
			t.Errorf("malformed id = %v", err)
		}
	})
}

// runSourceConflict is the source ownership contract: one source owns a name; a
// definition from any other source is recorded and refused, never replacing;
// indexing is idempotent; refused documents surface stable diagnostics only.
func runSourceConflict(t *testing.T, f fixture) {
	ctx := t.Context()

	t.Run("other_repository_is_recorded_and_refused", func(t *testing.T) {
		scope := f.project(t, tenantA)
		r1, r2 := f.repo(t, scope), f.repo(t, scope)
		owner, err := f.svc.Index(ctx, index(f, scope, r1, "e1", 1, doc("ci", "owner")))
		if err != nil {
			t.Fatal(err)
		}
		res, err := f.svc.Index(ctx, index(f, scope, r2, "e2", 2, doc("ci", "intruder")))
		if !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatalf("index from another repository = %+v, %v, want ErrSourceConflict", res, err)
		}
		var ce *pipelines.ConflictError
		if !errors.As(err, &ce) || ce.Name != "ci" ||
			ce.Existing != (pipelines.Source{Kind: pipelines.SourceGit, RepositoryID: r1}) ||
			ce.Attempted != (pipelines.Source{Kind: pipelines.SourceGit, RepositoryID: r2}) {
			t.Fatalf("conflict detail = %+v", ce)
		}
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != pipelines.CodeSourceConflict {
			t.Fatalf("conflict code = %v", err)
		}
		// The owner is untouched.
		got, err := f.svc.Get(ctx, scope, owner.Definition.ID)
		if err != nil || got.Version != 1 || got.Commit != commit(1) || got.Source.RepositoryID != r1 ||
			decode(t, got.Document).Steps[0].Commands[0] != "echo owner" {
			t.Fatalf("the owner was replaced: %+v, %v", got, err)
		}
		if id, _ := f.svc.Resolve(ctx, scope, "ci"); id != owner.Definition.ID {
			t.Fatalf("the name moved to %q", id)
		}
		// The refusal is recorded, without the refused document.
		conflicts, err := f.svc.Conflicts(ctx, scope)
		if err != nil || len(conflicts) != 1 {
			t.Fatalf("conflicts = %+v, %v", conflicts, err)
		}
		c := conflicts[0]
		if c.Name != "ci" || c.PipelineID != owner.Definition.ID || c.Existing.RepositoryID != r1 || c.Attempted.RepositoryID != r2 ||
			c.EventID != "e2" || c.Ref != "refs/heads/main" || c.Commit != commit(2) || c.Recorded.IsZero() || c.ID == "" {
			t.Fatalf("recorded conflict = %+v", c)
		}
		if strings.Contains(fmt.Sprintf("%+v", c), "intruder") {
			t.Fatalf("the conflict record carries document content: %+v", c)
		}
		// Neither source can be displaced by the other repository later either:
		// the owner keeps updating and the other stays refused.
		if _, err := f.svc.Index(ctx, index(f, scope, r1, "e3", 3, doc("ci", "owner2"))); err != nil {
			t.Fatalf("the owner's update: %v", err)
		}
		if _, err := f.svc.Index(ctx, index(f, scope, r2, "e4", 4, doc("ci", "intruder2"))); !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatalf("second attempt = %v", err)
		}
	})

	t.Run("api_and_git_never_replace_each_other", func(t *testing.T) {
		scope := f.project(t, tenantA)
		repo := f.repo(t, scope)
		api, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("api-owned", "api")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Index(ctx, index(f, scope, repo, "e1", 1, doc("api-owned", "git"))); !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatalf("git over api = %v", err)
		}
		git, err := f.svc.Index(ctx, index(f, scope, repo, "e2", 2, doc("git-owned", "git")))
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("git-owned", "api")})
		if !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatalf("api over git = %v, want ErrSourceConflict (not a plain duplicate)", err)
		}
		// Registering the same refused document again is answered from the record.
		if _, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: doc("git-owned", "api")}); !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatalf("repeated api over git = %v", err)
		}
		for _, id := range []string{api.Definition.ID, git.Definition.ID} {
			d, err := f.svc.Get(ctx, scope, id)
			if err != nil || d.Version != 1 {
				t.Fatalf("definition %s was changed: %+v, %v", id, d, err)
			}
		}
		if d, _ := f.svc.Get(ctx, scope, api.Definition.ID); d.Source.Kind != pipelines.SourceAPI || decode(t, d.Document).Steps[0].Commands[0] != "echo api" {
			t.Fatalf("api definition replaced: %+v", d)
		}
		conflicts, err := f.svc.Conflicts(ctx, scope)
		if err != nil || len(conflicts) != 2 {
			t.Fatalf("conflicts = %+v, %v (one per distinct attempt)", conflicts, err)
		}
	})

	t.Run("repeated_conflict_records_once", func(t *testing.T) {
		scope := f.project(t, tenantA)
		r1, r2 := f.repo(t, scope), f.repo(t, scope)
		if _, err := f.svc.Index(ctx, index(f, scope, r1, "e1", 1, doc("ci", "owner"))); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if _, err := f.svc.Index(ctx, index(f, scope, r2, "e2", 2, doc("ci", "intruder"))); !errors.Is(err, pipelines.ErrSourceConflict) {
				t.Fatalf("replayed conflict = %v", err)
			}
		}
		if c, _ := f.svc.Conflicts(ctx, scope); len(c) != 1 {
			t.Fatalf("a replayed event recorded %d conflicts, want 1", len(c))
		}
		// A different event for the same refusal is a new attempt.
		if _, err := f.svc.Index(ctx, index(f, scope, r2, "e3", 2, doc("ci", "intruder"))); !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatal(err)
		}
		if c, _ := f.svc.Conflicts(ctx, scope); len(c) != 2 {
			t.Fatalf("conflicts = %d, want 2", len(c))
		}
	})

	t.Run("conflict_is_per_project", func(t *testing.T) {
		a1, a2 := f.project(t, tenantA), f.project(t, tenantA)
		r1, r2 := f.repo(t, a1), f.repo(t, a2)
		if _, err := f.svc.Index(ctx, index(f, a1, r1, "e1", 1, doc("ci", "x"))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Index(ctx, index(f, a2, r2, "e1", 1, doc("ci", "x"))); err != nil {
			t.Fatalf("the same name in another project must not conflict: %v", err)
		}
		if c, _ := f.svc.Conflicts(ctx, a2); len(c) != 0 {
			t.Fatalf("conflicts leaked across projects: %+v", c)
		}
	})

	t.Run("index_is_idempotent_on_event_ref_commit", func(t *testing.T) {
		scope := f.project(t, tenantA)
		repo := f.repo(t, scope)
		first, err := f.svc.Index(ctx, index(f, scope, repo, "e1", 1, doc("ci", "v1")))
		if err != nil || !first.Created {
			t.Fatalf("first = %+v, %v", first, err)
		}
		again, err := f.svc.Index(ctx, index(f, scope, repo, "e1", 1, doc("ci", "v1")))
		if err != nil || again.Created || again.Updated || !again.Replayed || again.Definition.ID != first.Definition.ID || again.Definition.Version != 1 {
			t.Fatalf("replay = %+v, %v", again, err)
		}
		// A newer commit advances the definition; replaying the older event then
		// answers from its record and does not regress the definition.
		if _, err := f.svc.Index(ctx, index(f, scope, repo, "e2", 2, doc("ci", "v2"))); err != nil {
			t.Fatal(err)
		}
		old, err := f.svc.Index(ctx, index(f, scope, repo, "e1", 1, doc("ci", "v1")))
		if err != nil || !old.Replayed || old.Definition.Version != 2 || old.Definition.Commit != commit(2) {
			t.Fatalf("late replay = %+v, %v", old, err)
		}
		got, _ := f.svc.Get(ctx, scope, first.Definition.ID)
		if got.Version != 2 || decode(t, got.Document).Steps[0].Commands[0] != "echo v2" {
			t.Fatalf("the definition regressed: %+v", got)
		}
		// A different event at the same commit and document changes nothing.
		same, err := f.svc.Index(ctx, index(f, scope, repo, "e3", 2, doc("ci", "v2")))
		if err != nil || same.Created || same.Updated || same.Replayed || same.Definition.Version != 2 {
			t.Fatalf("unchanged = %+v, %v", same, err)
		}
		// Another ref at the same commit is a distinct attempt.
		req := index(f, scope, repo, "e1", 2, doc("ci", "v2"))
		req.Ref = "refs/heads/other"
		if r, err := f.svc.Index(ctx, req); err != nil || r.Replayed {
			t.Fatalf("other ref = %+v, %v", r, err)
		}
	})

	t.Run("concurrent_replays_create_one_definition", func(t *testing.T) {
		scope := f.project(t, tenantA)
		repo := f.repo(t, scope)
		var wg sync.WaitGroup
		var created atomic.Int64
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := f.svc.Index(ctx, index(f, scope, repo, "e1", 1, doc("ci", "v1")))
				if err != nil {
					t.Errorf("index: %v", err)
				} else if res.Created {
					created.Add(1)
				}
			}()
		}
		wg.Wait()
		if created.Load() != 1 {
			t.Fatalf("%d calls created the definition, want 1", created.Load())
		}
	})

	t.Run("invalid_definitions_surface_stable_codes_and_echo_nothing", func(t *testing.T) {
		scope := f.project(t, tenantA)
		repo := f.repo(t, scope)
		const secret = "TOPSECRET-value-7f3a"
		cases := []struct {
			name, code string
			doc        string
		}{
			{"unknown_field", pipeline.CodeUnknownField, "name: ci\nbogus_field: " + secret + "\nsteps: []\n"},
			{"bad_name", pipeline.CodeInvalidName, "name: Bad Name\nsteps:\n  - name: a\n    image: x\n    commands: [\"echo " + secret + "\"]\n"},
			{"bad_env_key", pipeline.CodeInvalidEnv, "name: ci\nsteps:\n  - name: a\n    image: x\n    commands: [run]\n    env:\n      \"bad key\": " + secret + "\n"},
			{"not_yaml", pipeline.CodeInvalidYAML, "name: [" + secret + "\n"},
			{"empty", pipeline.CodeInvalidYAML, ""},
		}
		for _, tc := range cases {
			var errs []error
			_, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: []byte(tc.doc)})
			errs = append(errs, err)
			_, err = f.svc.Index(ctx, index(f, scope, repo, "e1", 1, []byte(tc.doc)))
			errs = append(errs, err)
			for _, err := range errs {
				var diag *pipeline.Diagnostic
				if !errors.As(err, &diag) || diag.Code != tc.code {
					t.Errorf("%s: err = %v, want diagnostic %s", tc.name, err, tc.code)
					continue
				}
				if !errors.Is(err, domain.ErrInvalidValue) {
					t.Errorf("%s: %v does not classify as invalid", tc.name, err)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s: error echoes document content: %v", tc.name, err)
				}
			}
		}
		for _, bad := range []pipelines.IndexRequest{
			{Scope: scope, RepositoryID: repo, EventID: "e1", Ref: "refs/heads/main", Commit: "abc", Document: doc("ci", "x")},
			{Scope: scope, RepositoryID: repo, EventID: "", Ref: "refs/heads/main", Commit: commit(1), Document: doc("ci", "x")},
			{Scope: scope, RepositoryID: repo, EventID: "e1", Ref: "", Commit: commit(1), Document: doc("ci", "x")},
		} {
			if _, err := f.svc.Index(ctx, bad); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("index %+v = %v, want invalid", bad, err)
			}
		}
		huge := bytes.Repeat([]byte("# padding\n"), 40000)
		if _, err := f.svc.Register(ctx, pipelines.RegisterRequest{Scope: scope, Document: append(huge, doc("ci", "x")...)}); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("oversized document = %v", err)
		}
		// None of it created a pipeline or a record.
		if _, err := f.svc.Resolve(ctx, scope, "ci"); !errors.Is(err, pipelines.ErrNotFound) {
			t.Errorf("a refused document left a pipeline: %v", err)
		}
		if c, err := f.svc.Conflicts(ctx, scope); err != nil || len(c) != 0 {
			t.Errorf("conflicts = %+v, %v", c, err)
		}
	})

	t.Run("equivalent_documents_are_unchanged", func(t *testing.T) {
		scope := f.project(t, tenantA)
		repo := f.repo(t, scope)
		first, err := f.svc.Index(ctx, index(f, scope, repo, "e1", 1, doc("ci", "v1")))
		if err != nil {
			t.Fatal(err)
		}
		// Same document with different YAML formatting is the same definition.
		spaced := []byte("name: ci\n\nsteps:\n  - {name: build, image: golang:1.27, commands: [\"echo v1\"]}\n")
		res, err := f.svc.Index(ctx, index(f, scope, repo, "e2", 1, spaced))
		if err != nil || res.Updated || res.Created || res.Definition.Version != first.Definition.Version {
			t.Fatalf("reformatted document = %+v, %v", res, err)
		}
	})
}
