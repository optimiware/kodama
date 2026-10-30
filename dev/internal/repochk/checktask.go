package repochk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/dev/internal/devenv"
)

// TaskRun describes one local run of a task's declared check. It does not
// schedule, create worktrees, review or merge: it only executes the command a
// task record declares and verifies the acceptance names it lists.
type TaskRun struct {
	Root    string    // repository root holding docs/tasks
	TaskID  string    // T-NNN
	Environ []string  // base environment, KEY=VALUE
	TempDir string    // parent of the per-run results directory; os.TempDir when empty
	Stdout  io.Writer // check output and the verdict
	Stderr  io.Writer
}

// CheckTask is the command-line entry of `repoctl check-task T-NNN`.
func CheckTask(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: repoctl check-task T-NNN")
		return 2
	}
	return TaskRun{
		Root: devenv.Root(), TaskID: args[0], Environ: os.Environ(), Stdout: os.Stdout, Stderr: os.Stderr,
	}.Run()
}

// TaskResourcePrefix is the lowercase ID without the hyphen: T-126 → t126.
func TaskResourcePrefix(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, "-", ""))
}

// Run executes the check and returns the process exit status: 0 passed,
// ExitTempFail when the infrastructure (not the code) is unavailable, 2 for a
// usage or contract error, and otherwise the failing command's status or 1.
func (run TaskRun) Run() int {
	if run.Stdout == nil {
		run.Stdout = io.Discard
	}
	if run.Stderr == nil {
		run.Stderr = io.Discard
	}
	for _, name := range []string{"LAYER", "STAGE"} {
		if environmentValue(run.Environ, name, "") != "" {
			fmt.Fprintf(run.Stderr, "check-task: %s is no longer supported: a task declares its own check in docs/tasks/<id>-*.md; unset %s\n", name, name)
			return 2
		}
	}
	task, err := FindTask(run.Root, run.TaskID)
	if err != nil {
		fmt.Fprintf(run.Stderr, "check-task: %v\n", err)
		return 2
	}
	if task.Check == "" {
		fmt.Fprintf(run.Stderr, "check-task: %s declares no check\n", task.ID)
		return 2
	}
	parent := run.TempDir
	if parent == "" {
		parent = os.TempDir()
	}
	directory, err := os.MkdirTemp(parent, "kodama-"+TaskResourcePrefix(task.ID)+"-")
	if err != nil {
		fmt.Fprintf(run.Stderr, "check-task: %v\n", err)
		return 2
	}
	results := filepath.Join(directory, "results.json")
	_ = os.Remove(results) // never trust a stale file

	prefix := TaskResourcePrefix(task.ID)
	environment := append([]string{}, run.Environ...)
	environment = setValue(environment, "KODAMA_TASK_ID", task.ID)
	environment = setValue(environment, "KODAMA_RESOURCE_PREFIX", prefix)
	if environmentValue(environment, "DEV_SCOPE", "") == "" {
		environment = setValue(environment, "DEV_SCOPE", prefix)
	}
	environment = setValue(environment, "FORGE_RESULTS", results)

	fmt.Fprintf(run.Stdout, "check-task: %s: %s\n", task.ID, task.Check)
	command := exec.Command("bash", "-c", task.Check)
	command.Dir = run.Root
	command.Env = environment
	command.Stdout, command.Stderr = run.Stdout, run.Stderr
	code := 0
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
			if code <= 0 {
				code = 1
			}
		} else {
			fmt.Fprintf(run.Stderr, "check-task: %s: %v\n", task.ID, err)
			code = 1
		}
	}

	outcome := readTaskResults(results)
	// An infrastructure receipt wins over the exit status: a wrapper may mask
	// it (make reports 2) or even exit 0 after recording it.
	if code == ExitTempFail || outcome.infraPause != "" {
		reason := outcome.infraPause
		if reason == "" {
			reason = fmt.Sprintf("the check exited %d", ExitTempFail)
		}
		fmt.Fprintf(run.Stderr, "check-task: %s: infrastructure unavailable: %s\n", task.ID, reason)
		os.RemoveAll(directory)
		return ExitTempFail
	}
	if code != 0 {
		fmt.Fprintf(run.Stderr, "check-task: %s: FAIL: the check exited %d (results kept in %s)\n", task.ID, code, directory)
		return code
	}
	if len(task.Acceptance) > 0 {
		if problem := outcome.verify(task.Acceptance); problem != "" {
			fmt.Fprintf(run.Stderr, "check-task: %s: FAIL: acceptance: %s (results kept in %s)\n", task.ID, problem, directory)
			return 1
		}
	}
	os.RemoveAll(directory)
	fmt.Fprintf(run.Stdout, "PASS %s (%d acceptance names)\n", task.ID, len(task.Acceptance))
	return 0
}

// taskResults is the common form of both result formats.
type taskResults struct {
	present    bool
	problem    string // unreadable or empty results
	infraPause string
	// packageFailed is set for raw go test -json streams in which a package
	// failed, was skipped as a whole, or never reported completion.
	packageFailed bool
	passed        map[string]bool
	failed        map[string]bool
	skipped       map[string]bool
}

// readTaskResults accepts the aggregated object written by `repoctl go`
// ({"passed":[...],"failed":[...],"skipped":[...],"infra_pause":"..."}) or raw
// `go test -json` event lines.
func readTaskResults(path string) taskResults {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return taskResults{problem: "the check wrote no FORGE_RESULTS file"}
		}
		return taskResults{problem: err.Error()}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return taskResults{problem: "FORGE_RESULTS is empty"}
	}
	outcome := taskResults{present: true}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err == nil {
		_, hasPassed := object["passed"]
		_, hasFailed := object["failed"]
		_, hasSkipped := object["skipped"]
		_, hasPause := object["infra_pause"]
		if hasPassed || hasFailed || hasSkipped || hasPause {
			if hasPause {
				_ = json.Unmarshal(object["infra_pause"], &outcome.infraPause)
				outcome.infraPause = strings.TrimSpace(outcome.infraPause)
			}
			var err error
			if outcome.passed, err = nameSet(object["passed"]); err != nil {
				return taskResults{problem: "passed: " + err.Error()}
			}
			if outcome.failed, err = nameSet(object["failed"]); err != nil {
				return taskResults{problem: "failed: " + err.Error()}
			}
			if outcome.skipped, err = nameSet(object["skipped"]); err != nil {
				return taskResults{problem: "skipped: " + err.Error()}
			}
			return outcome
		}
	}
	passed, failed, skipped, packageFailed := ParseGoTestJSON(bytes.NewReader(data), io.Discard)
	outcome.packageFailed = packageFailed
	outcome.passed, outcome.failed, outcome.skipped = setOf(passed), setOf(failed), setOf(skipped)
	return outcome
}

func nameSet(raw json.RawMessage) (map[string]bool, error) {
	set := map[string]bool{}
	if len(raw) == 0 || string(raw) == "null" {
		return set, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("must be a list of strings")
	}
	for _, name := range names {
		set[name] = true
	}
	return set, nil
}

func setOf(values []string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	return set
}

// verify requires every acceptance name to be recorded as passed and none as
// failed or skipped. A name matches exactly or by its normalized form
// (unit_a_b matches Test_unit_a_b and TestUnitAB).
func (outcome taskResults) verify(names []string) string {
	if !outcome.present {
		return outcome.problem
	}
	if outcome.packageFailed {
		return "the go test -json stream records a package that failed or did not complete"
	}
	var missing, failed, skipped []string
	for _, name := range names {
		switch {
		case outcome.has(outcome.failed, name):
			failed = append(failed, name)
		case outcome.has(outcome.skipped, name):
			skipped = append(skipped, name)
		case !outcome.has(outcome.passed, name):
			missing = append(missing, name)
		}
	}
	var parts []string
	for _, group := range []struct {
		label string
		names []string
	}{{"failed", failed}, {"skipped", skipped}, {"not passed (missing from the results)", missing}} {
		if len(group.names) > 0 {
			parts = append(parts, group.label+": "+strings.Join(group.names, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

func (outcome taskResults) has(set map[string]bool, name string) bool {
	return set[name] || set[normalize(name)]
}
