package serve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/dispatch"
	"github.com/optimiware/kodama/internal/shell/serve"
	"github.com/optimiware/kodama/internal/shell/store"
)

// dispatchEnv is the push-to-run path over the real Git worker: a developer's
// push commits git.refs_updated in the memory store, the publisher drains it to
// the in-process bus, and the dispatcher reads the definitions at the pushed
// commit from the worker, indexes them and admits runs.
type dispatchEnv struct {
	*gitEnv
	ev    *serve.Events
	bus   *bus.Memory
	woken atomic.Int32
	repo  map[string]any
	id    string
	work  string
	pw    string // a human credential for direct pushes
}

func newDispatchEnv(t *testing.T) *dispatchEnv {
	t.Helper()
	d := &dispatchEnv{gitEnv: newGitEnv(t), bus: bus.NewMemory()}
	ev, err := serve.NewEvents(t.Context(), serve.EventsConfig{Store: d.mem, Files: d.workers, Bus: d.bus, Wake: func() { d.woken.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ev.Close)
	d.ev = ev
	d.repo = d.createRepo("backend", map[string]any{"directPushToDefault": true})
	d.id = d.repo["id"].(string)
	return d
}

// pipelineDoc is a .kodama definition named name that echoes marker and, when
// triggers is not empty, is started by those git triggers.
func pipelineDoc(name, marker, triggers string) string {
	doc := "name: " + name + "\n"
	if triggers != "" {
		doc += "triggers:\n  git:\n" + triggers
	}
	return doc + "steps:\n  - name: test\n    image: golang:1.27\n    commands: [\"echo " + marker + "\"]\n"
}

// commitFiles writes files (a nil content removes the file) and commits them.
func (d *dispatchEnv) commitFiles(dir, message string, files map[string]*string) string {
	d.t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if content == nil {
			if err := os.Remove(path); err != nil {
				d.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			d.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(*content), 0o644); err != nil {
			d.t.Fatal(err)
		}
	}
	d.mustGit(dir, "add", "-A")
	d.mustGit(dir, "commit", "-q", "--allow-empty", "-m", message)
	return d.mustGit(dir, "rev-parse", "HEAD")
}

func files(kv ...string) map[string]*string {
	out := map[string]*string{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "\x00" {
			out[kv[i]] = nil
			continue
		}
		v := kv[i+1]
		out[kv[i]] = &v
	}
	return out
}

// seed creates the working repository with a first commit and seeds the hosted one.
func (d *dispatchEnv) seed(fs map[string]*string) string {
	d.t.Helper()
	d.work = filepath.Join(d.dir, "dev")
	if err := os.MkdirAll(d.work, 0o755); err != nil {
		d.t.Fatal(err)
	}
	d.mustGit(d.work, "init", "-q", "-b", "main")
	commit := d.commitFiles(d.work, "first", fs)
	_, seedPW, status, body := d.credential("tok-eng", d.repo, true)
	if status != 201 {
		d.t.Fatalf("seed credential: %d %v", status, body)
	}
	if out, err := d.git(d.work, "push", d.cloneURL(d.repo, seedPW), "main:refs/heads/main"); err != nil {
		d.t.Fatalf("seed push: %v\n%s", err, out)
	}
	_, d.pw, _, _ = d.credential("tok-eng", d.repo, false)
	return commit
}

func (d *dispatchEnv) push(refspec ...string) {
	d.t.Helper()
	if out, err := d.git(d.work, append([]string{"push", d.cloneURL(d.repo, d.pw)}, refspec...)...); err != nil {
		d.t.Fatalf("push %v: %v\n%s", refspec, err, out)
	}
}

func (d *dispatchEnv) drain() {
	d.t.Helper()
	res, err := d.ev.Publisher.Drain(d.t.Context())
	if err != nil || res.Failed != 0 {
		d.t.Fatalf("drain = %+v, %v", res, err)
	}
}

func message(ev store.MemoryEvent) bus.Message {
	return bus.Message{ID: ev.ID, TenantID: ev.TenantID, ProjectID: ev.ProjectID, Type: ev.Type, ResourceType: ev.ResourceType,
		ResourceID: ev.ResourceID, Actor: bus.Actor{Type: ev.ActorType, ID: ev.ActorID}, OccurredAt: ev.CreatedAt, Data: ev.Payload}
}

func (d *dispatchEnv) handle(ev store.MemoryEvent) {
	d.t.Helper()
	if err := d.ev.Dispatcher.Handle(d.t.Context(), message(ev)); err != nil {
		d.t.Fatalf("handle %s: %v", ev.ID, err)
	}
}

func (d *dispatchEnv) pipeline(name string) store.Pipeline {
	d.t.Helper()
	for _, p := range d.mem.Pipelines("t_acme", d.id) {
		if p.Name == name {
			return p
		}
	}
	d.t.Fatalf("pipeline %q is not indexed: %+v", name, d.mem.Pipelines("t_acme", d.id))
	return store.Pipeline{}
}

func (d *dispatchEnv) hasPipeline(name string) bool {
	for _, p := range d.mem.Pipelines("t_acme", d.id) {
		if p.Name == name {
			return true
		}
	}
	return false
}

func commandOf(t *testing.T, p store.Pipeline) string {
	t.Helper()
	var doc struct {
		Steps []struct{ Commands []string }
	}
	if err := json.Unmarshal(p.Definition, &doc); err != nil || len(doc.Steps) != 1 || len(doc.Steps[0].Commands) != 1 {
		t.Fatalf("definition of %s: %s (%v)", p.Name, p.Definition, err)
	}
	return doc.Steps[0].Commands[0]
}

func (d *dispatchEnv) runs(pipeline string) []store.MemoryRun {
	var out []store.MemoryRun
	for _, r := range d.mem.AdmittedRuns() {
		if r.Pipeline == pipeline {
			out = append(out, r)
		}
	}
	return out
}

func (d *dispatchEnv) refused() []map[string]any {
	var out []map[string]any
	for _, e := range d.mem.EventsOfType(store.EventPipelineRefused) {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		out = append(out, p)
	}
	return out
}

const onMain = "    branches: [main]\n"

// Test_unit_kernel_git_pipeline_index_exact_commit: the definitions are read at the
// commit each event names, not at the branch tip, indexed with that commit as their
// source, replaced by the next push and disabled when their file is gone; a
// redelivered event changes nothing; a stale event does not displace a newer index.
func Test_unit_kernel_git_pipeline_index_exact_commit(t *testing.T) {
	d := newDispatchEnv(t)
	a := d.seed(files(
		".kodama/build.yaml", pipelineDoc("build", "A", onMain),
		".kodama/deploy.yml", pipelineDoc("deploy", "A", ""),
		".kodama/notes.txt", "not a pipeline\n",
		"README.md", "hello\n"))
	// Two further pushes land before the first dispatch runs: the branch tip is C
	// while the event of A is processed.
	b := d.commitFiles(d.work, "second", files(".kodama/build.yaml", pipelineDoc("build", "B", onMain), ".kodama/deploy.yml", "\x00"))
	d.push("main:refs/heads/main")
	c := d.commitFiles(d.work, "third", files(".kodama/build.yaml", pipelineDoc("build", "C", onMain), ".kodama/deploy.yml", pipelineDoc("deploy", "C", "")))
	d.push("main:refs/heads/main")
	events := d.refsEvents()
	if len(events) != 3 {
		t.Fatalf("%d push events", len(events))
	}
	if tip := d.mustGit(d.work, "ls-remote", d.cloneURL(d.repo, d.pw), "refs/heads/main"); !strings.Contains(tip, c+"\trefs/heads/main") {
		t.Fatalf("the hosted tip is %s, want %s", tip, c)
	}

	d.handle(events[0])
	build, deploy := d.pipeline("build"), d.pipeline("deploy")
	if build.Source != "git" || build.SourceCommit != a || commandOf(t, build) != "echo A" || build.Disabled ||
		deploy.Source != "git" || deploy.SourceCommit != a || commandOf(t, deploy) != "echo A" || build.RepositoryID != d.id {
		t.Fatalf("after the push of A (the tip is C): build=%+v deploy=%+v", build, deploy)
	}
	if d.hasPipeline("notes") {
		t.Fatal("a file that is not a .yaml or .yml definition was indexed")
	}

	d.handle(events[1])
	build, deploy = d.pipeline("build"), d.pipeline("deploy")
	if build.SourceCommit != b || commandOf(t, build) != "echo B" || !deploy.Disabled || deploy.SourceCommit != a {
		t.Fatalf("after the push of B, which removed deploy.yml: build=%+v deploy=%+v", build, deploy)
	}

	// Everything now flows through the publisher: A and B are redelivered and
	// change nothing, C is indexed, and the removed file's pipeline is back.
	versionBefore := d.pipeline("build").Version
	d.drain()
	build, deploy = d.pipeline("build"), d.pipeline("deploy")
	if build.SourceCommit != c || commandOf(t, build) != "echo C" || build.Version != versionBefore+1 || deploy.Disabled || commandOf(t, deploy) != "echo C" || deploy.SourceCommit != c {
		t.Fatalf("after the push of C: build=%+v deploy=%+v", build, deploy)
	}
	for _, e := range events[:2] {
		d.handle(e)
	}
	if again := d.pipeline("build"); again.Version != build.Version || again.SourceCommit != c {
		t.Fatalf("a redelivered older event displaced the index: %+v", again)
	}
	if len(d.mem.EventsOfType(store.EventPipelinesIndexed)) != 3 {
		t.Fatalf("%d dispatch markers, want one per event", len(d.mem.EventsOfType(store.EventPipelinesIndexed)))
	}

	t.Run("a stale default-branch event leaves the newer index alone", func(t *testing.T) {
		mem := store.NewMemoryGit()
		mem.AddTenant("t_acme", "acme")
		mem.AddProject("t_acme", "p_erp", "erp")
		if _, _, err := mem.CreateRepository(t.Context(), store.Repository{TenantID: "t_acme", ProjectID: "p_erp", ID: "repo_s", Name: "stale", DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
		index := func(commit, doc string) *store.PushIndex {
			return &store.PushIndex{Ref: "refs/heads/main", Commit: commit, Present: []string{"build"},
				Definitions: []store.IndexedDefinition{{Name: "build", Definition: json.RawMessage(doc)}}}
		}
		newer, older := strings.Repeat("2", 40), strings.Repeat("1", 40)
		for _, tc := range []struct {
			id       string
			position int64
			commit   string
			stale    bool
			expected string
		}{
			{"evt_new", 20, newer, false, newer},
			{"evt_old", 10, older, true, newer},
		} {
			res, err := mem.DispatchPush(t.Context(), store.PushDispatch{TenantID: "t_acme", RepositoryID: "repo_s", EventID: tc.id, JournalPosition: tc.position,
				ReceiveID: "rcv_" + tc.id, Index: index(tc.commit, `{"name":"build"}`)})
			if err != nil || res.Stale != tc.stale {
				t.Fatalf("%s: %+v %v", tc.id, res, err)
			}
			if got := mem.Pipelines("t_acme", "repo_s"); len(got) != 1 || got[0].SourceCommit != tc.expected {
				t.Fatalf("after %s: %+v", tc.id, got)
			}
		}
	})
}

// Test_unit_kernel_git_pipeline_index_rejects_invalid: a file that fails the strict
// parse or the semantic validation is refused with a stable reason and never
// displaces the previous definition; other files of the same commit are still
// indexed; an unreadable commit refuses as a whole and disables nothing.
func Test_unit_kernel_git_pipeline_index_rejects_invalid(t *testing.T) {
	d := newDispatchEnv(t)
	a := d.seed(files(".kodama/build.yaml", pipelineDoc("build", "A", onMain), ".kodama/keep.yaml", pipelineDoc("keep", "A", "")))
	d.drain()
	if build := d.pipeline("build"); build.SourceCommit != a || commandOf(t, build) != "echo A" {
		t.Fatalf("seed index: %+v", build)
	}
	buildV1 := d.pipeline("build").Version

	secret := "echo SECRET-VALUE-NEVER-ECHOED"
	b := d.commitFiles(d.work, "invalid", files(
		".kodama/build.yaml", "name: build\nsurprise: true\nsteps:\n  - name: test\n    image: x\n    commands: [\""+secret+"\"]\n", // unknown field
		".kodama/lint.yaml", "name: lint\nsteps: []\n", // no steps
		".kodama/ci.yaml", pipelineDoc("other", "B", ""), // the name is not the file's
		".kodama/Bad.yaml", pipelineDoc("bad", "B", ""), // not a process name
		".kodama/broken.yaml", "name: [unterminated\n  - ?\n", // not YAML
		".kodama/dup.yaml", pipelineDoc("dup", "B", ""), // dup.yaml wins over dup.yml
		".kodama/dup.yml", pipelineDoc("dup", "B2", ""), // refused as a duplicate
		".kodama/ok.yaml", pipelineDoc("ok", "B", ""), // valid, indexed
		".kodama/keep.yaml", "\x00")) // removed: disabled
	d.push("main:refs/heads/main")
	d.drain()

	build := d.pipeline("build")
	if build.SourceCommit != a || commandOf(t, build) != "echo A" || build.Disabled || build.Version != buildV1 {
		t.Fatalf("an invalid build.yaml displaced the valid definition: %+v", build)
	}
	for _, name := range []string{"lint", "ci", "other", "bad", "broken"} {
		if d.hasPipeline(name) {
			t.Errorf("%s was indexed from an invalid file", name)
		}
	}
	if ok := d.pipeline("ok"); ok.SourceCommit != b || d.pipeline("dup").SourceCommit != b || commandOf(t, d.pipeline("dup")) != "echo B" {
		t.Fatalf("valid files of the same commit were not indexed: ok=%+v dup=%+v", ok, d.pipeline("dup"))
	}
	if !d.pipeline("keep").Disabled {
		t.Fatal("a removed file's pipeline is still active")
	}
	want := map[string]string{
		".kodama/build.yaml": "unknown_field", ".kodama/lint.yaml": "invalid_value", ".kodama/ci.yaml": "name_mismatch",
		".kodama/broken.yaml": "invalid_yaml", ".kodama/dup.yml": "duplicate_name",
	}
	got := map[string]string{}
	for _, r := range d.refused() {
		got[r["path"].(string)] = r["code"].(string)
		if r["commit"] != b || r["repositoryId"] != d.id || r["ref"] != "refs/heads/main" {
			t.Errorf("refusal %v does not name the commit and repository", r)
		}
	}
	for path, code := range want {
		if got[path] != code {
			t.Errorf("refusal of %s = %q, want %q (all: %v)", path, got[path], code, got)
		}
	}
	for _, e := range d.mem.EventsOfType(store.EventPipelineRefused) {
		if strings.Contains(string(e.Payload), "SECRET-VALUE") {
			t.Fatalf("a refusal echoes the file's content: %s", e.Payload)
		}
	}

	// A commit whose files cannot be read at all (too large) is one refusal and
	// changes nothing, not even by disabling the pipelines it does not list.
	refusedBefore := len(d.refused())
	huge := d.commitFiles(d.work, "huge", files(".kodama/huge.yaml", strings.Repeat("a", 300<<10)))
	d.push("main:refs/heads/main")
	d.drain()
	if r := d.refused(); len(r) != refusedBefore+1 || r[len(r)-1]["code"] != "files_refused" || r[len(r)-1]["commit"] != huge {
		t.Fatalf("refusals after the oversized file: %v", r)
	}
	if d.pipeline("ok").Disabled || d.pipeline("build").Disabled || d.pipeline("build").SourceCommit != a {
		t.Fatalf("an unreadable commit changed the index: build=%+v ok=%+v", d.pipeline("build"), d.pipeline("ok"))
	}

	// Fixing the file indexes it; removing a never-valid file disables nothing.
	fixed := d.commitFiles(d.work, "fix", files(".kodama/huge.yaml", "\x00", ".kodama/build.yaml", pipelineDoc("build", "FIXED", onMain), ".kodama/lint.yaml", "\x00"))
	d.push("main:refs/heads/main")
	d.drain()
	if build := d.pipeline("build"); build.SourceCommit != fixed || commandOf(t, build) != "echo FIXED" || build.Disabled {
		t.Fatalf("the corrected definition was not indexed: %+v", build)
	}
}

func (d *dispatchEnv) sourcesOf(r store.MemoryRun) []struct {
	RepositoryID string  `json:"repositoryId"`
	Commit       string  `json:"commit"`
	Path         string  `json:"path"`
	Access       string  `json:"access"`
	Branch       *string `json:"branch"`
} {
	d.t.Helper()
	var out []struct {
		RepositoryID string  `json:"repositoryId"`
		Commit       string  `json:"commit"`
		Path         string  `json:"path"`
		Access       string  `json:"access"`
		Branch       *string `json:"branch"`
	}
	if err := json.Unmarshal(r.Sources, &out); err != nil {
		d.t.Fatal(err)
	}
	return out
}

// Test_unit_kernel_git_trigger_pins_commit: a push starts one run per matching
// pipeline and ref, pinned to the update's new commit even when the branch has
// moved on by the time the event is processed; triggers select by branch, tag and
// event kind; a delete admits nothing; admission wakes the scheduler.
func Test_unit_kernel_git_trigger_pins_commit(t *testing.T) {
	d := newDispatchEnv(t)
	defs := func(marker string) map[string]*string {
		return files(
			".kodama/build.yaml", pipelineDoc("build", marker, onMain),
			".kodama/release.yaml", pipelineDoc("release", marker, "    tags: [\"v*\"]\n    events: [tag]\n"),
			".kodama/docs.yaml", pipelineDoc("docs", marker, "    branches: [\"refs/heads/docs/*\"]\n"),
			".kodama/both.yaml", pipelineDoc("both", marker, "    branches: [main]\n    tags: [\"v*\"]\n    events: [push, tag]\n"),
			".kodama/tagsonly.yaml", pipelineDoc("tagsonly", marker, "    tags: [\"v*\"]\n"), // events default to [push]
			".kodama/manual.yaml", pipelineDoc("manual", marker, ""))
	}
	a := d.seed(defs("A"))
	d.drain()
	// The seed itself created main: the indexing and the admission are one step, so
	// the pipelines the push defines already start from it.
	if runs := d.runs("build"); len(runs) != 1 || d.sourcesOf(runs[0])[0].Commit != a {
		t.Fatalf("runs of build after the seed: %+v", runs)
	}
	if d.woken.Load() == 0 {
		t.Fatal("an admitted run did not wake the scheduler")
	}

	// B and C land before B's event is processed: the run of B pins B.
	b := d.commitFiles(d.work, "second", files("b.txt", "b\n"))
	d.push("main:refs/heads/main")
	c := d.commitFiles(d.work, "third", files("c.txt", "c\n"))
	d.push("main:refs/heads/main")
	events := d.refsEvents()
	d.handle(events[1])
	runs := d.runs("build")
	if len(runs) != 2 || d.sourcesOf(runs[1])[0].Commit != b {
		t.Fatalf("the run of B did not pin B (the tip is C): %+v", runs)
	}
	d.drain()
	runs = d.runs("build")
	if len(runs) != 3 || d.sourcesOf(runs[2])[0].Commit != c || d.sourcesOf(runs[1])[0].Commit != b || d.sourcesOf(runs[0])[0].Commit != a {
		t.Fatalf("each push pins its own commit: %+v", runs)
	}
	src := d.sourcesOf(runs[2])
	if len(src) != 1 || src[0].RepositoryID != d.id || src[0].Path != "." || src[0].Access != "read" || src[0].Branch != nil {
		t.Fatalf("the source set = %+v", src)
	}
	if r := runs[2]; r.TriggerKey != "git.refs_updated:"+events[2].ID+":refs/heads/main" || len(r.Steps) != 1 || r.Steps[0].StepKey != "test" ||
		len(r.Order) != 1 || !strings.Contains(string(r.Definition), "echo A") {
		t.Fatalf("the run = %+v", r)
	}
	// The default-branch pushes of B and C also started "both" (push on main) and
	// nothing else: the tag, docs and manual pipelines were not selected.
	for name, n := range map[string]int{"build": 3, "both": 3, "release": 0, "docs": 0, "tagsonly": 0, "manual": 0} {
		if got := len(d.runs(name)); got != n {
			t.Errorf("%s has %d runs, want %d", name, got, n)
		}
	}

	// A tag at C starts the pipelines that react to tags; a docs branch the docs
	// one; another branch none.
	d.mustGit(d.work, "tag", "v1", c)
	d.mustGit(d.work, "checkout", "-q", "-b", "docs/x")
	d.commitFiles(d.work, "docs", files("docs.md", "x\n"))
	docsTip := d.mustGit(d.work, "rev-parse", "HEAD")
	d.mustGit(d.work, "checkout", "-q", "-b", "feature/y")
	d.push("refs/tags/v1:refs/tags/v1", "docs/x:refs/heads/docs/x", "feature/y:refs/heads/feature/y")
	d.drain()
	rel := d.runs("release")
	if len(rel) != 1 || d.sourcesOf(rel[0])[0].Commit != c || !strings.HasSuffix(rel[0].TriggerKey, ":refs/tags/v1") {
		t.Fatalf("release runs: %+v", rel)
	}
	if docs := d.runs("docs"); len(docs) != 1 || d.sourcesOf(docs[0])[0].Commit != docsTip {
		t.Fatalf("docs runs: %+v", docs)
	}
	if n := len(d.runs("both")); n != 4 { // + the tag
		t.Fatalf("both has %d runs, want 4", n)
	}
	for name, n := range map[string]int{"build": 3, "tagsonly": 0, "manual": 0} {
		if got := len(d.runs(name)); got != n {
			t.Errorf("%s has %d runs, want %d", name, got, n)
		}
	}

	// Deleting a ref admits nothing.
	before := len(d.mem.AdmittedRuns())
	d.push(":refs/tags/v1")
	d.drain()
	if after := len(d.mem.AdmittedRuns()); after != before {
		t.Fatalf("a deleted tag started %d runs", after-before)
	}
	del := d.refsEvents()
	if last := string(del[len(del)-1].Payload); !strings.Contains(last, `"kind":"delete"`) {
		t.Fatalf("the delete event is %s", last)
	}
}

// Test_unit_kernel_git_trigger_deduplicates: a redelivered event, a republished
// outbox row and two concurrent deliveries admit exactly one run per (pipeline,
// push event, ref); the next push is another event and starts its own run.
func Test_unit_kernel_git_trigger_deduplicates(t *testing.T) {
	d := newDispatchEnv(t)
	a := d.seed(files(".kodama/build.yaml", pipelineDoc("build", "A", onMain)))
	events := d.refsEvents()
	if len(events) != 1 {
		t.Fatalf("%d events", len(events))
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.ev.Dispatcher.Handle(t.Context(), message(events[0])); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if runs := d.runs("build"); len(runs) != 1 || d.sourcesOf(runs[0])[0].Commit != a {
		t.Fatalf("concurrent deliveries admitted %+v", runs)
	}
	for i := 0; i < 3; i++ {
		d.handle(events[0])
	}
	// The publisher delivers the same row, and delivers it again after the mark
	// is lost: still one run.
	d.drain()
	subject := "kodama.v1.acme.erp.git.refs_updated"
	for i := 0; i < 2; i++ {
		if err := d.bus.Publish(t.Context(), subject, message(events[0])); err != nil {
			t.Fatal(err)
		}
	}
	if runs := d.runs("build"); len(runs) != 1 {
		t.Fatalf("redelivery admitted %d runs", len(runs))
	}
	if len(d.mem.EventsOfType(store.EventPipelinesIndexed)) != 1 {
		t.Fatalf("one dispatch marker per event, got %d", len(d.mem.EventsOfType(store.EventPipelinesIndexed)))
	}

	// Even without the dispatch marker the (pipeline, trigger key) is unique: two
	// admissions of the same key in one plan, or in a later plan, start one run.
	plan := func(keys ...string) store.Planner {
		return func(ps []store.Pipeline) ([]store.RunAdmission, error) {
			var out []store.RunAdmission
			for _, k := range keys {
				id, _ := store.NewID("run")
				out = append(out, store.RunAdmission{PipelineID: ps[0].ID, TriggerKey: k, Run: store.Run{ID: id, Definition: ps[0].Definition},
					Sources: json.RawMessage(`[]`)})
			}
			return out, nil
		}
	}
	res, err := d.mem.DispatchPush(t.Context(), store.PushDispatch{TenantID: "t_acme", RepositoryID: d.id, EventID: "evt_x1", Plan: plan("k1", "k1")})
	if err != nil || len(res.Runs) != 1 {
		t.Fatalf("a repeated key in one plan: %+v %v", res, err)
	}
	res, err = d.mem.DispatchPush(t.Context(), store.PushDispatch{TenantID: "t_acme", RepositoryID: d.id, EventID: "evt_x2", Plan: plan("k1", "k2")})
	if err != nil || len(res.Runs) != 1 {
		t.Fatalf("a repeated key in a later plan: %+v %v", res, err)
	}
	if _, err := d.mem.DispatchPush(t.Context(), store.PushDispatch{TenantID: "t_acme", RepositoryID: d.id, EventID: "evt_x3", Plan: plan("")}); err == nil {
		t.Fatal("a run without a trigger key was admitted")
	}
	if got := len(d.runs("build")); got != 3 {
		t.Fatalf("%d runs", got)
	}

	// The branch moving later is another push event, with its own run.
	b := d.commitFiles(d.work, "second", files("b.txt", "b\n"))
	d.push("main:refs/heads/main")
	d.drain()
	runs := d.runs("build")
	last := runs[len(runs)-1]
	if len(runs) != 4 || d.sourcesOf(last)[0].Commit != b {
		t.Fatalf("the next push: %d runs, last %+v", len(runs), d.sourcesOf(last))
	}
}

// Test_unit_kernel_git_trigger_excludes_origin_run: a push made by a run never
// retriggers, whatever its refs match, while the same ref pushed by a person does;
// a run's push of the default branch is still indexed.
func Test_unit_kernel_git_trigger_excludes_origin_run(t *testing.T) {
	d := newDispatchEnv(t)
	d.seed(files(".kodama/watch.yaml", pipelineDoc("watch", "A", "    branches: [\"feature/*\", \"kodama/runs/*\"]\n")))
	d.drain()
	base := len(d.runs("watch"))

	// A live run with write access to its own branches pushes one.
	d.mem.SetRun(store.RunGitInfo{TenantID: "t_acme", ProjectID: "p_erp", RunID: "run_1", PipelineName: "watch", Status: "running",
		Sources: []byte(`[{"repositoryId":"` + d.id + `","commit":"` + strings.Repeat("a", 40) + `","path":".","access":"write","branch":"refs/heads/kodama/work/tsk_1"}]`), TaskID: "tsk_1"})
	issued, err := d.server.IssueRunCredential(t.Context(), "t_acme", "p_erp", "run_1", d.id)
	if err != nil {
		t.Fatal(err)
	}
	d.mustGit(d.work, "checkout", "-q", "-b", "runwork")
	runCommit := d.commitFiles(d.work, "by the run", files("run.txt", "r\n"))
	if out, err := d.git(d.work, "push", d.cloneURL(d.repo, issued.Secret), "runwork:refs/heads/kodama/runs/run_1/work"); err != nil {
		t.Fatalf("run push: %v\n%s", err, out)
	}
	events := d.refsEvents()
	if p := payload(t, events[len(events)-1]); p.OriginRunID != "run_1" || len(p.Updates) != 1 || p.Updates[0].New != runCommit {
		t.Fatalf("the run's push event = %s", events[len(events)-1].Payload)
	}
	d.drain()
	if got := len(d.runs("watch")); got != base {
		t.Fatalf("a run's own push started %d runs", got-base)
	}

	// The same push by a person starts one; so does a feature branch.
	d.mustGit(d.work, "checkout", "-q", "-b", "feature/z")
	personCommit := d.commitFiles(d.work, "by a person", files("p.txt", "p\n"))
	d.push("feature/z:refs/heads/feature/z", "runwork:refs/heads/kodama/runs/run_2/work")
	d.drain()
	runs := d.runs("watch")
	if len(runs) != base+2 {
		t.Fatalf("a person's push started %d runs, want 2", len(runs)-base)
	}
	pinned := map[string]string{}
	for _, r := range runs[base:] {
		pinned[strings.TrimPrefix(r.TriggerKey[strings.LastIndex(r.TriggerKey, ":")+1:], "refs/heads/")] = d.sourcesOf(r)[0].Commit
	}
	if pinned["feature/z"] != personCommit || pinned["kodama/runs/run_2/work"] != runCommit {
		t.Fatalf("pinned = %v", pinned)
	}

	// A run's push of the default branch (the integration case) is exclusion too,
	// but the definitions it brings are still indexed.
	d.mustGit(d.work, "checkout", "-q", "main")
	merged := d.commitFiles(d.work, "integrated", files(".kodama/added.yaml", pipelineDoc("added", "I", onMain)))
	d.push("main:refs/heads/feature/integrated") // the commit exists in the repository
	before := len(d.mem.AdmittedRuns())
	if _, err := d.mem.CommitReceive(t.Context(), store.ReceiveCommit{TenantID: "t_acme", RepositoryID: d.id, ReceiveID: "rcv_integration_gg_x",
		ActorType: "workload", ActorID: "run_1", OriginRunID: "run_1",
		Updates: []store.ReceiveUpdate{{Ref: "refs/heads/main", Old: d.mustGit(d.work, "rev-parse", merged+"~1"), New: merged, Kind: "fast_forward"}}}); err != nil {
		t.Fatal(err)
	}
	d.drain()
	if d.pipeline("added").SourceCommit != merged {
		t.Fatalf("a run's push of the default branch was not indexed: %+v", d.mem.Pipelines("t_acme", d.id))
	}
	for _, r := range d.mem.AdmittedRuns()[before:] {
		if r.Pipeline == "added" || strings.HasSuffix(r.TriggerKey, ":refs/heads/main") {
			t.Fatalf("a run's own push of main started %+v", r)
		}
	}
}

// Test_unit_kernel_git_trigger_stale_follows_push_order: staleness is decided by the
// receive's journal position, not by when its outcome was reported. The report of
// push A is lost, push B (applied after A) is indexed, then A is recovered from the
// worker's journal: its event is committed after B's, and B's definitions survive.
func Test_unit_kernel_git_trigger_stale_follows_push_order(t *testing.T) {
	d := newDispatchEnv(t)
	d.seed(files(".kodama/build.yaml", pipelineDoc("build", "A0", onMain)))
	d.drain()

	d.dropNext.Store(1) // the report of push A never reaches the control plane
	a := d.commitFiles(d.work, "A", files(".kodama/build.yaml", pipelineDoc("build", "A", onMain)))
	d.push("main:refs/heads/main")
	if n := len(d.refsEvents()); n != 1 {
		t.Fatalf("%d events after the lost report of A", n)
	}
	b := d.commitFiles(d.work, "B", files(".kodama/build.yaml", pipelineDoc("build", "B", onMain)))
	d.push("main:refs/heads/main")
	d.drain()
	if p := d.pipeline("build"); p.SourceCommit != b || commandOf(t, p) != "echo B" {
		t.Fatalf("after B: %+v", p)
	}

	// A is recovered: its event is committed after B's.
	d.workers.Reconcile(context.WithValue(t.Context(), reconcileMarker{}, true), []domain.TenantID{"t_acme"})
	events := d.refsEvents()
	if len(events) != 3 {
		t.Fatalf("%d events after the recovery", len(events))
	}
	byCommit := map[string]store.MemoryEvent{}
	positions := map[string]int64{}
	for _, e := range events[1:] {
		var p struct {
			JournalPosition int64 `json:"journalPosition"`
			Updates         []struct{ New string }
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil || len(p.Updates) != 1 {
			t.Fatalf("event %s: %v", e.Payload, err)
		}
		byCommit[p.Updates[0].New], positions[p.Updates[0].New] = e, p.JournalPosition
	}
	if !byCommit[b].CreatedAt.Before(byCommit[a].CreatedAt) || positions[a] < 1 || positions[a] >= positions[b] {
		t.Fatalf("A (recovered) must be committed later than B but ordered before it: positions A=%d B=%d", positions[a], positions[b])
	}
	versionBefore := d.pipeline("build").Version
	d.drain()
	build := d.pipeline("build")
	if build.SourceCommit != b || commandOf(t, build) != "echo B" || build.Version != versionBefore {
		t.Fatalf("the recovered older push displaced the newer definitions: %+v", build)
	}
	var marker struct{ Stale bool }
	for _, e := range d.mem.EventsOfType(store.EventPipelinesIndexed) {
		if e.ID == store.DispatchMarkerID(byCommit[a].ID) {
			_ = json.Unmarshal(e.Payload, &marker)
		}
	}
	if !marker.Stale {
		t.Fatal("the recovered push was not recorded as stale")
	}
}

// pausingFiles holds a file read until released.
type pausingFiles struct {
	inner   dispatch.Files
	reading chan struct{}
	release chan struct{}
}

func (p pausingFiles) ReadFiles(ctx context.Context, tenant domain.TenantID, req gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error) {
	resp, err := p.inner.ReadFiles(ctx, tenant, req)
	close(p.reading)
	<-p.release
	return resp, err
}

// Test_unit_kernel_git_trigger_archive_during_read: a repository archived while
// the worker's files are being read gets no index and no run (the status is read
// again under the repository lock), and the event stays dispatchable after a
// restore.
func Test_unit_kernel_git_trigger_archive_during_read(t *testing.T) {
	d := newDispatchEnv(t)
	d.seed(files(".kodama/build.yaml", pipelineDoc("build", "A", onMain)))
	event := d.refsEvents()[0]
	pause := pausingFiles{inner: d.workers, reading: make(chan struct{}), release: make(chan struct{})}
	disp, err := dispatch.New(dispatch.Config{Store: d.mem, Files: pause, Wake: func() { d.woken.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- disp.Handle(t.Context(), message(event)) }()
	<-pause.reading
	if _, err := d.mem.SetRepositoryStatus(t.Context(), "t_acme", d.id, store.RepoArchived, "", "user", "u_eng"); err != nil {
		t.Fatal(err)
	}
	close(pause.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := d.mem.Pipelines("t_acme", d.id); len(got) != 0 {
		t.Fatalf("an archived repository was indexed: %+v", got)
	}
	if runs := d.mem.AdmittedRuns(); len(runs) != 0 {
		t.Fatalf("an archived repository started %+v", runs)
	}
	if n := len(d.mem.EventsOfType(store.EventPipelinesIndexed)) + len(d.mem.EventsOfType(store.EventPipelineRefused)); n != 0 {
		t.Fatalf("an archived repository recorded %d dispatch rows", n)
	}
	if d.woken.Load() != 0 {
		t.Fatal("the scheduler was woken for nothing")
	}

	// Restored, the same event dispatches.
	if _, err := d.mem.SetRepositoryStatus(t.Context(), "t_acme", d.id, store.RepoReady, "", "user", "u_eng"); err != nil {
		t.Fatal(err)
	}
	pause2 := pausingFiles{inner: d.workers, reading: make(chan struct{}), release: make(chan struct{})}
	close(pause2.release)
	disp2, _ := dispatch.New(dispatch.Config{Store: d.mem, Files: pause2})
	if err := disp2.Handle(t.Context(), message(event)); err != nil {
		t.Fatal(err)
	}
	if len(d.mem.Pipelines("t_acme", d.id)) != 1 || len(d.mem.AdmittedRuns()) != 1 {
		t.Fatalf("after the restore: %+v / %+v", d.mem.Pipelines("t_acme", d.id), d.mem.AdmittedRuns())
	}
}
