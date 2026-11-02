package repochk

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Task statuses. Planned and blocked tasks are the active ones: they are
// scheduled (or waiting for a named decision), carry a check contract and own
// paths exclusively.
const (
	StatusPlanned  = "planned"
	StatusBlocked  = "blocked"
	StatusDeferred = "deferred"
	StatusDone     = "done"
)

var (
	taskIDPattern   = regexp.MustCompile(`^T-[0-9]{3}$`)
	taskFilePattern = regexp.MustCompile(`^(T-[0-9]{3})-[A-Za-z0-9][A-Za-z0-9-]*\.md$`)
	taskKeys        = map[string]bool{
		"id": true, "milestone": true, "status": true, "depends_on": true, "decision_blockers": true,
		"owns": true, "check": true, "resources": true, "acceptance": true, "plan": true, "review": true,
	}
	requiredTaskKeys = []string{"id", "milestone", "status", "depends_on", "decision_blockers", "owns"}
)

// Task is the validated frontmatter of one docs/tasks/T-*.md record.
type Task struct {
	ID               string
	File             string // repository-relative, slash-separated
	Milestone        string
	Status           string
	DependsOn        []string
	DecisionBlockers []string
	Owns             []string
	Check            string
	Resources        []string
	Acceptance       []string
	// HasAcceptance records that the key is present (an empty list is valid).
	HasAcceptance bool
}

// Active reports whether the task is scheduled or waiting for a decision.
func (task Task) Active() bool {
	return task.Status == StatusPlanned || task.Status == StatusBlocked
}

// Tasks validates the task graph and the dependency index under root and
// prints the result.
func Tasks(root string) error {
	tasks, err := LoadTaskGraph(root)
	if err != nil {
		return err
	}
	fmt.Printf("PASS task graph (%d tasks)\n", len(tasks))
	return nil
}

// LoadTaskGraph loads and validates every task record, the dependency graph,
// active ownership and the README dependency index. All problems are
// reported together.
func LoadTaskGraph(root string) ([]Task, error) {
	tasks, problems := LoadTasks(root)
	problems = append(problems, validateGraph(tasks)...)
	index := filepath.Join(root, "docs", "tasks", "README.md")
	data, err := os.ReadFile(index)
	if err != nil {
		problems = append(problems, fmt.Errorf("docs/tasks/README.md: %v", err))
	} else {
		problems = append(problems, validateIndex("docs/tasks/README.md", string(data), tasks)...)
	}
	if len(problems) > 0 {
		return nil, joinProblems(problems)
	}
	return tasks, nil
}

// LoadTasks parses every docs/tasks/T-*.md under root, sorted by ID. Each
// record is validated on its own; cross-record rules are in the graph check.
func LoadTasks(root string) ([]Task, []error) {
	directory := filepath.Join(root, "docs", "tasks")
	matches, err := filepath.Glob(filepath.Join(directory, "T-*.md"))
	if err != nil {
		return nil, []error{err}
	}
	sort.Strings(matches)
	var tasks []Task
	var problems []error
	for _, match := range matches {
		name := relative(root, match)
		data, err := os.ReadFile(match)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %v", filepath.ToSlash(name), err))
			continue
		}
		task, taskProblems := ParseTask(filepath.ToSlash(name), data)
		problems = append(problems, taskProblems...)
		if len(taskProblems) == 0 {
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks, problems
}

// FindTask returns the validated record of one task ID.
func FindTask(root, id string) (Task, error) {
	if !taskIDPattern.MatchString(id) {
		return Task{}, fmt.Errorf("invalid task ID %q: expected T-NNN", id)
	}
	matches, err := filepath.Glob(filepath.Join(root, "docs", "tasks", id+"-*.md"))
	if err != nil {
		return Task{}, err
	}
	if len(matches) == 0 {
		return Task{}, fmt.Errorf("task %s: no record docs/tasks/%s-*.md", id, id)
	}
	if len(matches) > 1 {
		return Task{}, fmt.Errorf("task %s: more than one record %v", id, matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return Task{}, err
	}
	task, problems := ParseTask(filepath.ToSlash(relative(root, matches[0])), data)
	if len(problems) > 0 {
		return Task{}, joinProblems(problems)
	}
	return task, nil
}

// ParseTask parses and validates the frontmatter of one task record. file is
// the repository-relative path used in messages and checked against the ID.
func ParseTask(file string, data []byte) (Task, []error) {
	fail := func(field, format string, args ...any) error {
		return fmt.Errorf("%s: %s: %s", file, field, fmt.Sprintf(format, args...))
	}
	task := Task{File: file}
	base := path.Base(file)
	match := taskFilePattern.FindStringSubmatch(base)
	if match == nil {
		return task, []error{fail("file name", "%q must be T-NNN-<slug>.md", base)}
	}
	raw, err := frontmatter(data)
	if err != nil {
		return task, []error{fmt.Errorf("%s: frontmatter: %v", file, err)}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return task, []error{fmt.Errorf("%s: frontmatter: %v", file, err)}
	}
	if document == nil {
		return task, []error{fmt.Errorf("%s: frontmatter: empty", file)}
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return task, []error{fmt.Errorf("%s: frontmatter: more than one YAML document", file)}
	}
	var problems []error
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !taskKeys[key] {
			problems = append(problems, fail(key, "unknown frontmatter key"))
		}
	}
	for _, key := range requiredTaskKeys {
		if _, ok := document[key]; !ok {
			problems = append(problems, fail(key, "required key is missing"))
		}
	}
	text := func(key string) string {
		value, ok := document[key]
		if !ok {
			return ""
		}
		str, isString := value.(string)
		if !isString || strings.TrimSpace(str) == "" || str != strings.TrimSpace(str) {
			problems = append(problems, fail(key, "must be a non-empty string without surrounding whitespace"))
			return ""
		}
		return str
	}
	list := func(key string) []string {
		value, ok := document[key]
		if !ok {
			return nil
		}
		items, isList := value.([]any)
		if !isList {
			problems = append(problems, fail(key, "must be a list"))
			return nil
		}
		result := []string{}
		for _, item := range items {
			str, isString := item.(string)
			if !isString || strings.TrimSpace(str) == "" || str != strings.TrimSpace(str) {
				problems = append(problems, fail(key, "entries must be non-empty strings, got %v", item))
				continue
			}
			result = append(result, str)
		}
		return result
	}
	task.ID = text("id")
	task.Milestone = text("milestone")
	task.Status = text("status")
	task.Check = text("check")
	task.DependsOn = list("depends_on")
	task.DecisionBlockers = list("decision_blockers")
	task.Owns = list("owns")
	task.Resources = list("resources")
	task.Acceptance = list("acceptance")
	_, task.HasAcceptance = document["acceptance"]
	if value, ok := document["plan"]; ok {
		if _, isBool := value.(bool); !isBool {
			problems = append(problems, fail("plan", "must be true or false"))
		}
	}
	if value, ok := document["review"]; ok && value != "human" {
		problems = append(problems, fail("review", "must be \"human\" when present"))
	}
	if task.ID != "" {
		if !taskIDPattern.MatchString(task.ID) {
			problems = append(problems, fail("id", "%q must match T-NNN", task.ID))
		} else if task.ID != match[1] {
			problems = append(problems, fail("id", "%q does not match the file name prefix %s", task.ID, match[1]))
		}
	}
	switch task.Status {
	case "", StatusPlanned, StatusBlocked, StatusDeferred, StatusDone:
	default:
		problems = append(problems, fail("status", "%q must be planned, blocked, deferred or done", task.Status))
	}
	for _, dependency := range task.DependsOn {
		if !taskIDPattern.MatchString(dependency) {
			problems = append(problems, fail("depends_on", "%q must be a task ID T-NNN", dependency))
		} else if dependency == task.ID {
			problems = append(problems, fail("depends_on", "%s depends on itself", dependency))
		}
	}
	problems = append(problems, duplicates(file, "depends_on", task.DependsOn)...)
	problems = append(problems, duplicates(file, "acceptance", task.Acceptance)...)
	problems = append(problems, duplicates(file, "resources", task.Resources)...)
	problems = append(problems, duplicates(file, "decision_blockers", task.DecisionBlockers)...)
	for _, owned := range task.Owns {
		if reason := invalidOwnedPath(owned); reason != "" {
			problems = append(problems, fail("owns", "%q %s", owned, reason))
		}
	}
	problems = append(problems, duplicates(file, "owns", task.Owns)...)
	if task.Active() {
		if len(task.Owns) == 0 {
			problems = append(problems, fail("owns", "a %s task must own at least one path", task.Status))
		}
		if _, ok := document["check"]; !ok {
			problems = append(problems, fail("check", "required for a %s task", task.Status))
		}
		if !task.HasAcceptance {
			problems = append(problems, fail("acceptance", "required for a %s task (use [] when there are no named tests)", task.Status))
		}
	}
	if task.Status == StatusBlocked && len(task.DecisionBlockers) == 0 {
		problems = append(problems, fail("decision_blockers", "a blocked task must name at least one decision"))
	}
	return task, problems
}

// frontmatter returns the YAML between the leading and the next `---` lines.
func frontmatter(data []byte) ([]byte, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("the file must start with a --- line")
	}
	rest := text[len("---\n"):]
	end := strings.Index("\n"+rest, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			return []byte(strings.TrimSuffix(rest, "---")), nil
		}
		return nil, fmt.Errorf("the closing --- line is missing")
	}
	return []byte(rest[:end]), nil
}

func duplicates(file, field string, values []string) []error {
	seen := map[string]bool{}
	var problems []error
	for _, value := range values {
		if seen[value] {
			problems = append(problems, fmt.Errorf("%s: %s: duplicate entry %q", file, field, value))
		}
		seen[value] = true
	}
	return problems
}

// invalidOwnedPath explains why an owned path is not a clean repository-
// relative file or directory (a trailing slash marks a directory).
func invalidOwnedPath(value string) string {
	switch {
	case strings.ContainsAny(value, `\*?[`):
		return "must not contain backslashes or glob characters"
	case strings.HasPrefix(value, "/"):
		return "must be relative, not absolute"
	}
	trimmed := strings.TrimSuffix(value, "/")
	switch {
	case trimmed == "" || trimmed == ".":
		return "must name a path below the repository root"
	case trimmed == ".." || strings.HasPrefix(trimmed, "../") || strings.Contains(trimmed, "/../"):
		return "must not contain .."
	case path.Clean(trimmed) != trimmed:
		return "must be clean (no empty, . or repeated segments)"
	}
	return ""
}

func validateGraph(tasks []Task) []error {
	var problems []error
	byID := map[string]Task{}
	for _, task := range tasks {
		if previous, duplicate := byID[task.ID]; duplicate {
			problems = append(problems, fmt.Errorf("%s: id: duplicate of %s", task.File, previous.File))
			continue
		}
		byID[task.ID] = task
	}
	for _, task := range tasks {
		for _, dependency := range task.DependsOn {
			if _, ok := byID[dependency]; !ok {
				problems = append(problems, fmt.Errorf("%s: depends_on: %s has no task record", task.File, dependency))
			}
		}
	}
	if cycle := findCycle(tasks, byID); cycle != nil {
		problems = append(problems, fmt.Errorf("docs/tasks: dependency cycle %s", strings.Join(cycle, " -> ")))
		return problems
	}
	ancestors := map[string]map[string]bool{}
	var closure func(id string) map[string]bool
	closure = func(id string) map[string]bool {
		if known, ok := ancestors[id]; ok {
			return known
		}
		result := map[string]bool{}
		ancestors[id] = result
		for _, dependency := range byID[id].DependsOn {
			if _, ok := byID[dependency]; !ok {
				continue
			}
			result[dependency] = true
			for transitive := range closure(dependency) {
				result[transitive] = true
			}
		}
		return result
	}
	var active []Task
	for _, task := range tasks {
		if task.Active() {
			active = append(active, task)
		}
	}
	for i := range active {
		for j := i + 1; j < len(active); j++ {
			a, b := active[i], active[j]
			if closure(a.ID)[b.ID] || closure(b.ID)[a.ID] {
				continue
			}
			for _, left := range a.Owns {
				for _, right := range b.Owns {
					if ownedPathsOverlap(left, right) {
						problems = append(problems, fmt.Errorf("%s: owns: %q overlaps %q owned by unordered active task %s (%s); order them with depends_on or split the paths", a.File, left, right, b.ID, b.File))
					}
				}
			}
		}
	}
	return problems
}

func ownedPathsOverlap(left, right string) bool {
	// A path may name a file or a directory with or without a trailing slash,
	// so any separator-bounded prefix counts as nesting.
	leftBase, rightBase := strings.TrimSuffix(left, "/"), strings.TrimSuffix(right, "/")
	return leftBase == rightBase ||
		strings.HasPrefix(rightBase, leftBase+"/") ||
		strings.HasPrefix(leftBase, rightBase+"/")
}

func findCycle(tasks []Task, byID map[string]Task) []string {
	const (
		visiting = 1
		visited  = 2
	)
	state := map[string]int{}
	var stack []string
	var visit func(id string) []string
	visit = func(id string) []string {
		switch state[id] {
		case visited:
			return nil
		case visiting:
			for index, entry := range stack {
				if entry == id {
					return append(append([]string{}, stack[index:]...), id)
				}
			}
			return []string{id, id}
		}
		state[id] = visiting
		stack = append(stack, id)
		dependencies := append([]string{}, byID[id].DependsOn...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if _, ok := byID[dependency]; !ok {
				continue
			}
			if cycle := visit(dependency); cycle != nil {
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = visited
		return nil
	}
	for _, task := range tasks {
		if cycle := visit(task.ID); cycle != nil {
			return cycle
		}
	}
	return nil
}

var indexRow = regexp.MustCompile(`^\|\s*\[(T-[0-9]{3})[^\]]*\]\(([^)]+)\)\s*\|([^|]*)\|([^|]*)\|\s*$`)

// validateIndex checks the "Dependency index" table of docs/tasks/README.md
// against the task records. Every active task needs a row. Done and deferred
// tasks may be absent (the index keeps scheduled work; completed records are
// pruned per the repository's task policy), but a row that is present must
// describe an existing record faithfully.
func validateIndex(file, text string, tasks []Task) []error {
	byID := map[string]Task{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	section, ok := markdownSection(text, "Dependency index")
	if !ok {
		return []error{fmt.Errorf("%s: missing the \"Dependency index\" section", file)}
	}
	var problems []error
	rows := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || !strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(line, "|")), "[T-") {
			continue
		}
		match := indexRow.FindStringSubmatch(line)
		if match == nil {
			problems = append(problems, fmt.Errorf("%s: index row is malformed: %s", file, line))
			continue
		}
		id, target, dependencyCell, statusCell := match[1], match[2], strings.TrimSpace(match[3]), strings.TrimSpace(match[4])
		if rows[id] {
			problems = append(problems, fmt.Errorf("%s: index: duplicate row for %s", file, id))
		}
		rows[id] = true
		task, exists := byID[id]
		if !exists {
			problems = append(problems, fmt.Errorf("%s: index: row for %s has no valid task record", file, id))
			continue
		}
		if want := path.Base(task.File); target != want {
			problems = append(problems, fmt.Errorf("%s: index: %s links %q, expected %q", file, id, target, want))
		}
		var dependencies []string
		if dependencyCell != "—" && dependencyCell != "-" && dependencyCell != "" {
			for _, part := range strings.Split(dependencyCell, ",") {
				dependencies = append(dependencies, strings.TrimSpace(part))
			}
		}
		if got, want := sortedCopy(dependencies), sortedCopy(task.DependsOn); strings.Join(got, ",") != strings.Join(want, ",") {
			problems = append(problems, fmt.Errorf("%s: index: %s dependencies [%s] disagree with depends_on [%s] in %s", file, id, strings.Join(got, ", "), strings.Join(want, ", "), task.File))
		}
		word, rest, _ := strings.Cut(statusCell, " ")
		rest = strings.TrimSpace(rest)
		if word != task.Status || (rest != "" && !(strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")"))) {
			problems = append(problems, fmt.Errorf("%s: index: %s status %q disagrees with status %q in %s", file, id, statusCell, task.Status, task.File))
		}
	}
	for _, task := range tasks {
		if task.Active() && !rows[task.ID] {
			problems = append(problems, fmt.Errorf("%s: index: %s task %s (%s) has no row", file, task.Status, task.ID, task.File))
		}
	}
	return problems
}

// markdownSection returns the body of a level-two section up to the next
// level-one or level-two heading.
func markdownSection(text, title string) (string, bool) {
	lines := strings.Split(text, "\n")
	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "## "+title {
			start = index + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for index := start; index < len(lines); index++ {
		if strings.HasPrefix(lines[index], "## ") || strings.HasPrefix(lines[index], "# ") {
			end = index
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

func sortedCopy(values []string) []string {
	result := append([]string{}, values...)
	sort.Strings(result)
	return result
}

func joinProblems(problems []error) error {
	lines := make([]string, 0, len(problems))
	for _, problem := range problems {
		lines = append(lines, problem.Error())
	}
	return fmt.Errorf("task graph invalid (%d problems):\n%s", len(lines), strings.Join(lines, "\n"))
}
