package repochk

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureTask struct {
	file, body string
}

const activeBody = `id: %[1]s
milestone: production
status: %[2]s
depends_on: [%[3]s]
decision_blockers: [%[4]s]
owns: [%[5]s]
check: make check-go PKGS=./internal/%[6]s/...
acceptance: [unit_%[6]s]
`

func task(id, status, depends, owns string) fixtureTask {
	blockers := ""
	if status == StatusBlocked {
		blockers = "O-1"
	}
	slug := strings.ToLower(strings.ReplaceAll(id, "-", ""))
	return fixtureTask{id + "-" + slug + ".md", fmt.Sprintf(activeBody, id, status, depends, blockers, owns, slug)}
}

func writeTasks(t *testing.T, index string, tasks ...fixtureTask) string {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, entry := range tasks {
		text := "---\n" + entry.body + "---\n# " + entry.file + "\n"
		if err := os.WriteFile(filepath.Join(directory, entry.file), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("# Tasks\n\n## Dependency index\n\n| Task | Dependencies | Status / decision |\n| --- | --- | --- |\n"+index+"\n## Next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func row(id, slug, dependencies, status string) string {
	return fmt.Sprintf("| [%s Title](%s-%s.md) | %s | %s |\n", id, id, slug, dependencies, status)
}

func expectProblem(t *testing.T, root, want string) {
	t.Helper()
	_, err := LoadTaskGraph(root)
	if err == nil {
		t.Fatalf("expected a problem containing %q, got a pass", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

func TestUnitTaskGraphAccepts(t *testing.T) {
	root := writeTasks(t,
		row("T-001", "t001", "—", "done")+row("T-002", "t002", "T-001", "planned")+row("T-003", "t003", "T-001, T-002", "blocked (O-1)"),
		task("T-001", StatusDone, "", "internal/a/"),
		task("T-002", StatusPlanned, "T-001", "internal/b/"),
		task("T-003", StatusBlocked, "T-001, T-002", "internal/c/, internal/b/file.go"),
	)
	tasks, err := LoadTaskGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("tasks = %d", len(tasks))
	}
}

func TestUnitTaskGraphAllowsOrderedOverlapAndDoneOmission(t *testing.T) {
	// T-003 depends transitively on T-001 through T-002, so their shared
	// directory is ordered; the done task has no index row.
	root := writeTasks(t,
		row("T-002", "t002", "T-001", "planned")+row("T-003", "t003", "T-002", "planned"),
		task("T-001", StatusDone, "", "internal/a/"),
		task("T-002", StatusPlanned, "T-001", "internal/a/"),
		task("T-003", StatusPlanned, "T-002", "internal/a/x.go"),
	)
	if _, err := LoadTaskGraph(root); err != nil {
		t.Fatal(err)
	}
}

func TestUnitTaskGraphRejectsUnorderedOverlap(t *testing.T) {
	index := row("T-001", "t001", "—", "planned") + row("T-002", "t002", "—", "blocked")
	for name, owns := range map[string][2]string{
		"equal":               {"internal/a/", "internal/a/"},
		"equal file":          {"internal/a.go", "internal/a.go"},
		"prefix":              {"internal/", "internal/a/x.go"},
		"dir and name":        {"internal/a", "internal/a/"},
		"nested prefix":       {"internal/a/b/c.go", "internal/a/"},
		"bare dir over file":  {"internal/example", "internal/example/file.go"},
		"file under bare dir": {"internal/example/file.go", "internal/example"},
		"bare dir over dir":   {"internal/example", "internal/example/sub/"},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTasks(t, index,
				task("T-001", StatusPlanned, "", owns[0]),
				task("T-002", StatusBlocked, "", owns[1]),
			)
			expectProblem(t, root, "overlaps")
		})
	}
	// Similar names that are not a directory prefix do not overlap.
	root := writeTasks(t, index,
		task("T-001", StatusPlanned, "", "internal/a/"),
		task("T-002", StatusBlocked, "", "internal/ab/, internal/a.go"),
	)
	if _, err := LoadTaskGraph(root); err != nil {
		t.Fatal(err)
	}
	// Deferred and done tasks do not take part in the overlap rule.
	root = writeTasks(t, row("T-001", "t001", "—", "planned"),
		task("T-001", StatusPlanned, "", "internal/a/"),
		task("T-002", StatusDeferred, "", "internal/a/"),
		task("T-003", StatusDone, "", "internal/a/"),
	)
	if _, err := LoadTaskGraph(root); err != nil {
		t.Fatal(err)
	}
}

func TestUnitTaskGraphRejectsInvalidRecords(t *testing.T) {
	base := task("T-001", StatusPlanned, "", "internal/a/")
	index := row("T-001", "t001", "—", "planned")
	mutate := func(from, to string) fixtureTask {
		if !strings.Contains(base.body, from) {
			t.Fatalf("fixture lacks %q", from)
		}
		return fixtureTask{base.file, strings.Replace(base.body, from, to, 1)}
	}
	cases := []struct {
		name, want string
		record     fixtureTask
	}{
		{"missing owns", "owns: required key is missing", mutate("owns: [internal/a/]\n", "")},
		{"missing milestone", "milestone: required key is missing", mutate("milestone: production\n", "")},
		{"missing depends_on", "depends_on: required key is missing", mutate("depends_on: []\n", "")},
		{"missing blockers", "decision_blockers: required key is missing", mutate("decision_blockers: []\n", "")},
		{"id mismatch", "does not match the file name prefix T-001", mutate("id: T-001", "id: T-009")},
		{"bad id", "must match T-NNN", mutate("id: T-001", "id: T-1")},
		{"bad status", "must be planned, blocked, deferred or done", mutate("status: planned", "status: ready")},
		{"no check", "check: required for a planned task", mutate("check: make check-go PKGS=./internal/t001/...\n", "")},
		{"no acceptance", "acceptance: required for a planned task", mutate("acceptance: [unit_t001]\n", "")},
		{"blocked without blocker", "must name at least one decision", mutate("status: planned", "status: blocked")},
		{"unknown key", "extra: unknown frontmatter key", mutate("owns:", "extra: 1\nowns:")},
		{"absolute path", "must be relative", mutate("[internal/a/]", "[/etc/passwd]")},
		{"parent path", "must not contain ..", mutate("[internal/a/]", "[internal/../x/]")},
		{"leading parent", "must not contain ..", mutate("[internal/a/]", "[../x]")},
		{"unclean path", "must be clean", mutate("[internal/a/]", "[internal//a/]")},
		{"dot path", "must name a path below the repository root", mutate("[internal/a/]", "[./]")},
		{"glob path", "glob characters", mutate("[internal/a/]", "[internal/*.go]")},
		{"self dependency", "depends on itself", mutate("depends_on: []", "depends_on: [T-001]")},
		{"bad dependency", "must be a task ID", mutate("depends_on: []", "depends_on: [one]")},
		{"duplicate acceptance", "duplicate entry", mutate("[unit_t001]", "[unit_t001, unit_t001]")},
		{"owns not a list", "owns: must be a list", mutate("owns: [internal/a/]", "owns: internal/a/")},
		{"empty owns", "must own at least one path", mutate("owns: [internal/a/]", "owns: []")},
		{"check not a string", "check: must be a non-empty string", mutate("check: make check-go PKGS=./internal/t001/...", "check: [make]")},
		{"bad plan", "plan: must be true or false", mutate("owns:", "plan: maybe\nowns:")},
		{"duplicate key", "frontmatter", mutate("owns:", "milestone: again\nowns:")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			expectProblem(t, writeTasks(t, index, test.record), test.want)
		})
	}
}

func TestUnitTaskGraphRejectsMalformedFrontmatter(t *testing.T) {
	for name, text := range map[string]string{
		"no opening":  "id: T-001\n",
		"no closing":  "---\nid: T-001\nstatus: planned\n",
		"empty":       "---\n---\n",
		"not a map":   "---\n- a\n---\n",
		"bad yaml":    "---\nid: [T-001\n---\n",
		"two streams": "---\nid: T-001\n---\n---\nid: T-001\n---\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, problems := ParseTask("docs/tasks/T-001-x.md", []byte(text))
			if len(problems) == 0 {
				t.Fatal("malformed frontmatter was accepted")
			}
		})
	}
	if _, problems := ParseTask("docs/tasks/not-a-task.md", []byte("---\nid: T-001\n---\n")); len(problems) == 0 {
		t.Fatal("a file name without a T-NNN prefix was accepted")
	}
}

func TestUnitTaskGraphRejectsGraphErrors(t *testing.T) {
	t.Run("missing dependency", func(t *testing.T) {
		root := writeTasks(t, row("T-001", "t001", "T-099", "planned"), task("T-001", StatusPlanned, "T-099", "internal/a/"))
		expectProblem(t, root, "T-099 has no task record")
	})
	t.Run("cycle", func(t *testing.T) {
		root := writeTasks(t,
			row("T-001", "t001", "T-003", "planned")+row("T-002", "t002", "T-001", "planned")+row("T-003", "t003", "T-002", "planned"),
			task("T-001", StatusPlanned, "T-003", "internal/a/"),
			task("T-002", StatusPlanned, "T-001", "internal/b/"),
			task("T-003", StatusPlanned, "T-002", "internal/c/"),
		)
		expectProblem(t, root, "dependency cycle")
	})
	t.Run("duplicate id", func(t *testing.T) {
		first, second := task("T-001", StatusDone, "", "internal/a/"), task("T-001", StatusDone, "", "internal/b/")
		second.file = "T-001-other.md"
		expectProblem(t, writeTasks(t, "", first, second), "duplicate of")
	})
}

func TestUnitTaskIndexAgreement(t *testing.T) {
	records := []fixtureTask{
		task("T-001", StatusDone, "", "internal/a/"),
		task("T-002", StatusPlanned, "T-001", "internal/b/"),
		task("T-003", StatusBlocked, "T-001", "internal/c/"),
	}
	good := row("T-002", "t002", "T-001", "planned") + row("T-003", "t003", "T-001", "blocked (O-32)")
	if _, err := LoadTaskGraph(writeTasks(t, good, records...)); err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"missing active row":   {row("T-002", "t002", "T-001", "planned"), "has no row"},
		"wrong dependencies":   {row("T-002", "t002", "—", "planned") + row("T-003", "t003", "T-001", "blocked"), "disagree with depends_on"},
		"wrong status":         {row("T-002", "t002", "T-001", "deferred") + row("T-003", "t003", "T-001", "blocked"), "disagrees with status"},
		"status junk":          {row("T-002", "t002", "T-001", "planned soon") + row("T-003", "t003", "T-001", "blocked"), "disagrees with status"},
		"wrong link":           {row("T-002", "elsewhere", "T-001", "planned") + row("T-003", "t003", "T-001", "blocked"), "expected"},
		"unknown task":         {good + row("T-077", "t077", "—", "planned"), "no valid task record"},
		"duplicate row":        {good + row("T-002", "t002", "T-001", "planned"), "duplicate row"},
		"malformed row":        {good + "| [T-004 x](T-004-t004.md) | — |\n", "malformed"},
		"no space duplicate":   {good + "|[T-002 Title](T-002-t002.md)| T-001 | planned |\n", "duplicate row"},
		"wide space duplicate": {good + "|   [T-003 Title](T-003-t003.md) | T-001 | blocked |  \n", "duplicate row"},
		"tab duplicate":        {good + "|\t[T-002 Title](T-002-t002.md) | T-001 | planned |\n", "duplicate row"},
		"no space unknown":     {good + "|[T-077 x](T-077-t077.md)| — | planned |\n", "no valid task record"},
		"indented unknown":     {good + "  |  [T-078 x](T-078-t078.md) | — | planned |\n", "no valid task record"},
		"no space malformed":   {good + "|[T-004 x](T-004-t004.md)| — |\n", "malformed"},
		"no space bad status":  {row("T-002", "t002", "T-001", "planned") + "|[T-003 Title](T-003-t003.md)|T-001|planned|\n", "disagrees with status"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			expectProblem(t, writeTasks(t, test[0], records...), test[1])
		})
	}
	t.Run("missing section", func(t *testing.T) {
		root := writeTasks(t, good, records...)
		if err := os.WriteFile(filepath.Join(root, "docs", "tasks", "README.md"), []byte("# Tasks\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		expectProblem(t, root, "Dependency index")
	})
	t.Run("missing index file", func(t *testing.T) {
		root := writeTasks(t, good, records...)
		if err := os.Remove(filepath.Join(root, "docs", "tasks", "README.md")); err != nil {
			t.Fatal(err)
		}
		expectProblem(t, root, "README.md")
	})
}

func TestUnitOwnedPathOverlap(t *testing.T) {
	for _, test := range []struct {
		left, right string
		overlap     bool
	}{
		{"a/", "a/", true}, {"a/", "a", true}, {"a/", "a/b.go", true}, {"a/b/", "a/", true},
		{"internal/example", "internal/example/file.go", true}, {"internal/example", "internal/example/sub/", true},
		{"internal/example", "internal/example/", true}, {"internal/example", "internal/example", true},
		{"internal/example", "internal/examples", false}, {"internal/example", "internal/examples/file.go", false},
		{"internal/example/", "internal/examples/", false}, {"internal/example.go", "internal/example/x.go", false},
		{"a/b.go", "a/c.go", false}, {"a/", "ab/", false}, {"a", "ab", false}, {"a.go", "a/", false},
	} {
		if got := ownedPathsOverlap(test.left, test.right); got != test.overlap {
			t.Errorf("overlap(%q, %q) = %v", test.left, test.right, got)
		}
		if got := ownedPathsOverlap(test.right, test.left); got != test.overlap {
			t.Errorf("overlap(%q, %q) = %v", test.right, test.left, got)
		}
	}
}
