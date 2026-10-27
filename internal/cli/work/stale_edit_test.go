package work_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/root"
	cliwork "github.com/optimiware/kodama/internal/cli/work"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workhttp "github.com/optimiware/kodama/internal/work/http"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Resource prefix of this task's fixtures; the database itself is provisioned
// by the shared helper with its own random, isolated name.
const prefix = "t025"

// principals maps bearer tokens to verified principals: the router
// authenticates, never the request body, and never a saved CLI context.
var principals = map[string]string{"tok-analyst": "u_analyst", "tok-engineer": "u_engineer"}

type tokenAuth struct{}

func (tokenAuth) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || principals[tok] == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: principals[tok]}, nil
}

type allowRoutes struct{}

func (allowRoutes) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil // the use cases enforce the fine-grained permission rules
}

// authority knows the two fixture principals as human users and grants them
// every permission; this test is about versions, not roles.
type authority struct{}

func (authority) ResolveActor(_ context.Context, _ domain.Scope, p string) (work.Actor, error) {
	if p != "u_analyst" && p != "u_engineer" {
		return work.Actor{}, work.ErrHidden
	}
	return work.Actor{ID: p, Type: work.ActorUser}, nil
}

func (authority) Authorize(context.Context, work.Actor, domain.Scope, work.Permission) error {
	return nil
}

type guards struct{}

func (guards) Authorize(context.Context, work.Actor, domain.Scope, work.Permission) error {
	return nil
}
func (guards) WriterSettled(context.Context, domain.Scope, domain.WorkItemID) (bool, error) {
	return true, nil
}
func (guards) VerifySubject(context.Context, domain.Scope, domain.WorkItemID, work.ReviewClaim) (work.VerifiedSubject, error) {
	return work.VerifiedSubject{}, work.ErrNotFound
}
func (guards) VerifyEvidence(context.Context, domain.Scope, []work.EvidenceRef) error { return nil }

func bindGuards(pgx.Tx) work.TxGuards {
	return work.TxGuards{Permissions: guards{}, Writers: guards{}, Dependencies: work.NoDependencyGraph{}, Subjects: guards{}}
}

// realFactory serves the real work HTTP handler, use cases and PostgreSQL
// store over loopback HTTP.
type realFactory struct {
	srv     *httptest.Server
	patches atomic.Int32
}

func newRealFactory(t *testing.T) *realFactory {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owner, rt := db.MigrationPool(ctx, 4), db.RuntimePool(ctx, 16)
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO tenants (id, key) VALUES ('` + tenant + `', '` + tenant + `')`,
		`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('` + tenant + `', '` + project + `', '` + project + `', 5)`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	opts := eventstest.DefaultOptions()
	opts.Clock = (&eventstest.Clock{}).Now
	events, err := eventspg.NewStore(rt, opts)
	if err != nil {
		t.Fatal(err)
	}
	store, err := workpg.NewStore(rt, events, bindGuards)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := work.NewService(work.Config{Repository: store, Authority: authority{}})
	if err != nil {
		t.Fatal(err)
	}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: tokenAuth{}, Authorizer: allowRoutes{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := workhttp.NewHandler(svc, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	f := &realFactory{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			f.patches.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// as runs one CLI invocation authenticated as the principal behind token.
func (f *realFactory) as(t *testing.T, token string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	r, err := root.NewRunner(root.RunnerConfig{
		Streams: root.Streams{Out: &out, Err: &errb}, Tokens: tokenResolver{clienttest.Tokens(token)}, AllowInsecureLoopback: true,
	}, cliwork.NewCommands())
	if err != nil {
		t.Fatal(err)
	}
	full := append([]string{}, args[:2]...)
	full = append(full, "--factory", f.srv.URL, "--tenant", tenant, "--project", project, "--output", "json")
	full = append(full, args[2:]...)
	code := r.Run(context.Background(), full)
	return result{code, out.String(), errb.String()}
}

// Test_stale_edit_exit: concurrent edits made from one observed version yield
// exactly one accepted mutation and one exit-4 conflict; the winning content
// and a single history entry survive, the loser's failure carries the server's
// request ID, and the CLI never retries. Real PostgreSQL through the real work
// HTTP handler and use cases (WM-02, AC-03, WM-A02).
func Test_stale_edit_exit(t *testing.T) {
	f := newRealFactory(t)
	files := t.TempDir()
	spec := func(name, text string) string {
		p := filepath.Join(files, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	specA := spec("a.md", "# Plan\nAnalyst version\n")
	specB := spec("b.md", "# Plan\nEngineer version\n")

	for round := 0; round < 4; round++ {
		created := f.as(t, "tok-analyst", "work", "create", "--kind", "story", "--title", "Resolve inventory exceptions", "--spec-file", spec("seed.md", "# Plan\nSeed\n"))
		if created.code != 0 {
			t.Fatalf("create: exit %d: %s", created.code, created.err)
		}
		item := decodeJSON(t, created.out)
		id := item["id"].(string)
		if itoa(item["version"]) != "1" || itoa(item["contentRevision"]) != "1" {
			t.Fatalf("created %v", item)
		}
		f.patches.Store(0)

		results := make([]result, 2)
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i, edit := range []struct{ token, file string }{{"tok-analyst", specA}, {"tok-engineer", specB}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				results[i] = f.as(t, edit.token, "work", "update", id, "--spec-file", edit.file, "--if-version", "1")
			}()
		}
		close(gate)
		wg.Wait()

		wins, conflicts := 0, 0
		var winner, loser result
		for _, r := range results {
			switch r.code {
			case 0:
				wins++
				winner = r
			case 4:
				conflicts++
				loser = r
			default:
				t.Fatalf("unexpected exit %d: %s", r.code, r.err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d accepted, %d conflicts (%q, %q)", round, wins, conflicts, results[0].err, results[1].err)
		}
		if loser.out != "" {
			t.Fatalf("the conflicting command wrote to stdout: %q", loser.out)
		}
		env := decodeJSON(t, loser.err)["error"].(map[string]any)
		if env["code"] != "version_conflict" || env["requestId"] == "" || env["requestId"] == nil {
			t.Fatalf("conflict envelope %v", env)
		}
		if !strings.Contains(env["details"].(map[string]any)["recovery"].(string), "work history") {
			t.Fatalf("no reread advice: %v", env)
		}
		if n := f.patches.Load(); n != 2 {
			t.Fatalf("%d edit requests reached the server: the CLI must not retry a conflict", n)
		}

		// The winner's content and a single appended history entry survive.
		won := decodeJSON(t, winner.out)
		got := decodeJSON(t, f.as(t, "tok-engineer", "work", "get", id).out)
		if itoa(got["version"]) != "2" || itoa(got["contentRevision"]) != "2" || got["spec"] != won["spec"] {
			t.Fatalf("after the race: %v (winner %v)", got, won)
		}
		if s := got["spec"].(string); s != "# Plan\nAnalyst version\n" && s != "# Plan\nEngineer version\n" {
			t.Fatalf("spec %q is neither edit", s)
		}
		hist := decodeJSON(t, f.as(t, "tok-engineer", "work", "history", id).out)["items"].([]any)
		if len(hist) != 2 {
			t.Fatalf("history has %d entries: %v", len(hist), hist)
		}
		if k := hist[1].(map[string]any)["kind"]; k != "updated" || itoa(hist[1].(map[string]any)["version"]) != "2" {
			t.Fatalf("history %v", hist)
		}

		// The loser re-reads: the observed version still conflicts, the fresh
		// one applies on top and never overwrites silently.
		stale := f.as(t, "tok-analyst", "work", "update", id, "--spec-file", specA, "--if-version", "1")
		if stale.code != 4 {
			t.Fatalf("stale retry: exit %d", stale.code)
		}
		fresh := f.as(t, "tok-analyst", "work", "update", id, "--spec-file", spec("merged.md", "# Plan\nMerged after rereading\n"), "--if-version", "2")
		if fresh.code != 0 || itoa(decodeJSON(t, fresh.out)["version"]) != "3" {
			t.Fatalf("fresh retry: exit %d %s", fresh.code, fresh.err)
		}
	}
}
