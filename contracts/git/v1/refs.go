package gitv1

import (
	"fmt"
	"strconv"
	"strings"
)

// Branch namespace. Every Kodama-managed branch lives under refs/heads/kodama/,
// every server-only ref under refs/kodama/.
const (
	// TaskBranchPrefix is followed by the work item ID.
	TaskBranchPrefix = "refs/heads/kodama/work/"
	// AgentBranchPrefix is followed by <workItemId>/<runId>/<label>.
	AgentBranchPrefix = "refs/heads/kodama/agents/"
	// RunBranchPrefix is followed by <runId>/<label>: the branches of a run that
	// has no work item (a developer-push pipeline, a provisioning run).
	RunBranchPrefix = "refs/heads/kodama/runs/"
	// RetainedRefPrefix is followed by <runId>/<n>: the hidden refs that keep a
	// run's pinned commits reachable.
	RetainedRefPrefix = "refs/kodama/runs/"
	// ServerRefPrefix is the server-only namespace, hidden from fetch.
	ServerRefPrefix = "refs/kodama/"
	// HeadsPrefix and TagsPrefix are the ordinary namespaces.
	HeadsPrefix = "refs/heads/"
	TagsPrefix  = "refs/tags/"

	// DefaultBranchName is the default branch of a repository created without one.
	DefaultBranchName = "main"
)

// RefClass is the policy class of a ref (git-repositories.md, "Access and ref
// policy").
type RefClass string

// Ref classes.
const (
	ClassDefault RefClass = "default"      // refs/heads/<defaultBranch>, protected
	ClassTask    RefClass = "task"         // refs/heads/kodama/work/<workItemId>
	ClassAgent   RefClass = "agent"        // refs/heads/kodama/agents/<workItemId>/<runId>/<label>
	ClassRun     RefClass = "run"          // refs/heads/kodama/runs/<runId>/<label>
	ClassBranch  RefClass = "other_branch" // any other branch
	ClassTag     RefClass = "tag"          // refs/tags/...
	ClassServer  RefClass = "server"       // refs/kodama/..., server only
	ClassOther   RefClass = "other"        // refs outside heads, tags and refs/kodama (notes, changes, ...)
)

// TaskBranch returns the task branch of a work item.
func TaskBranch(workItemID string) (string, error) {
	if err := checkID("workItemId", workItemID); err != nil {
		return "", err
	}
	return TaskBranchPrefix + workItemID, nil
}

// AgentBranch returns the run-scoped agent branch refs/heads/kodama/agents/<workItemId>/<runId>/<label>.
func AgentBranch(workItemID, runID, label string) (string, error) {
	if err := firstError(checkID("workItemId", workItemID), checkID("runId", runID), checkLabel(label)); err != nil {
		return "", err
	}
	return AgentBranchPrefix + workItemID + "/" + runID + "/" + label, nil
}

// AgentBranchPattern returns the pattern naming every agent branch of a run:
// refs/heads/kodama/agents/<workItemId>/<runId>/*.
func AgentBranchPattern(workItemID, runID string) (string, error) {
	if err := firstError(checkID("workItemId", workItemID), checkID("runId", runID)); err != nil {
		return "", err
	}
	return AgentBranchPrefix + workItemID + "/" + runID + "/*", nil
}

// RunBranch returns the branch of a run without a work item:
// refs/heads/kodama/runs/<runId>/<label>.
func RunBranch(runID, label string) (string, error) {
	if err := firstError(checkID("runId", runID), checkLabel(label)); err != nil {
		return "", err
	}
	return RunBranchPrefix + runID + "/" + label, nil
}

// RunBranchPattern returns refs/heads/kodama/runs/<runId>/*.
func RunBranchPattern(runID string) (string, error) {
	if err := checkID("runId", runID); err != nil {
		return "", err
	}
	return RunBranchPrefix + runID + "/*", nil
}

// RetainedRef returns the hidden ref refs/kodama/runs/<runId>/<n> that pins the
// n-th retained commit (n >= 1) of a run.
func RetainedRef(runID string, n int64) (string, error) {
	if err := checkID("runId", runID); err != nil {
		return "", err
	}
	if n < 1 || n > MaxSafeInteger {
		return "", fail("n", "must be between 1 and 2^53-1")
	}
	return RetainedRefPrefix + runID + "/" + strconv.FormatInt(n, 10), nil
}

// ParseRetainedRef is the inverse of RetainedRef.
func ParseRetainedRef(ref string) (runID string, n int64, err error) {
	rest, ok := strings.CutPrefix(ref, RetainedRefPrefix)
	if !ok {
		return "", 0, fail("ref", "is not a retained ref")
	}
	runID, num, ok := strings.Cut(rest, "/")
	if !ok || checkID("runId", runID) != nil || num == "" || num[0] == '0' || len(num) > 16 {
		return "", 0, fail("ref", "is not a retained ref")
	}
	n, perr := strconv.ParseInt(num, 10, 64)
	if perr != nil || n < 1 || n > MaxSafeInteger || strconv.FormatInt(n, 10) != num {
		return "", 0, fail("ref", "is not a retained ref")
	}
	return runID, n, nil
}

// checkLabel validates a label, which is one ref component: the Label syntax
// without the sequences the ref syntax forbids ("..", ".lock", a trailing ".").
func checkLabel(label string) error {
	if err := matches("label", "Label", label); err != nil {
		return err
	}
	if compiled["forbidden"].MatchString(label) {
		return fail("label", "is not a valid ref component")
	}
	return nil
}

// Ref is a parsed ref of the Kodama namespaces. Fields not meaningful for the
// class are empty.
type Ref struct {
	Class      RefClass
	WorkItemID string
	RunID      string
	Label      string
}

// ClassifyRef parses ref and returns its policy class under the repository's
// defaultBranch. The ref must be a valid ref name. The default branch wins over
// every other class. A branch under refs/heads/kodama/ whose remainder does not
// have the exact shape of a task, agent or run branch is an ordinary
// other_branch: only engineers (or a grant naming it) may update it.
func ClassifyRef(defaultBranch, ref string) (Ref, error) {
	if err := ValidateRefName(ref); err != nil {
		return Ref{}, err
	}
	if err := ValidateBranchName(defaultBranch); err != nil {
		return Ref{}, err
	}
	switch {
	case ref == HeadsPrefix+defaultBranch:
		return Ref{Class: ClassDefault}, nil
	case strings.HasPrefix(ref, ServerRefPrefix):
		return Ref{Class: ClassServer}, nil
	case strings.HasPrefix(ref, TagsPrefix):
		return Ref{Class: ClassTag}, nil
	case !strings.HasPrefix(ref, HeadsPrefix):
		return Ref{Class: ClassOther}, nil
	}
	if rest, ok := strings.CutPrefix(ref, TaskBranchPrefix); ok && checkID("workItemId", rest) == nil {
		return Ref{Class: ClassTask, WorkItemID: rest}, nil
	}
	if rest, ok := strings.CutPrefix(ref, AgentBranchPrefix); ok {
		if parts := strings.Split(rest, "/"); len(parts) == 3 &&
			checkID("workItemId", parts[0]) == nil && checkID("runId", parts[1]) == nil && checkLabel(parts[2]) == nil {
			return Ref{Class: ClassAgent, WorkItemID: parts[0], RunID: parts[1], Label: parts[2]}, nil
		}
	}
	if rest, ok := strings.CutPrefix(ref, RunBranchPrefix); ok {
		if parts := strings.Split(rest, "/"); len(parts) == 2 && checkID("runId", parts[0]) == nil && checkLabel(parts[1]) == nil {
			return Ref{Class: ClassRun, RunID: parts[0], Label: parts[1]}, nil
		}
	}
	return Ref{Class: ClassBranch}, nil
}

// UpdateKind classifies one ref update for the git.refs_updated event.
type UpdateKind string

// Update kinds.
const (
	KindCreate      UpdateKind = "create"
	KindFastForward UpdateKind = "fast_forward"
	KindRewrite     UpdateKind = "rewrite"
	KindDelete      UpdateKind = "delete"
)

// KindOf derives the event kind of an update from its object IDs and the
// worker's fast-forward fact (GitFact.FastForward; nil for a create or delete).
func KindOf(old, new string, fastForward *bool) (UpdateKind, error) {
	if err := firstError(checkObjectID("old", old), checkObjectID("new", new)); err != nil {
		return "", err
	}
	if len(old) != len(new) || old == new {
		return "", fail("update", "old and new must be different object IDs of one length")
	}
	switch {
	case IsZeroObjectID(old):
		return KindCreate, nil
	case IsZeroObjectID(new):
		return KindDelete, nil
	case fastForward == nil:
		return "", fail("fastForward", "is required for an update of an existing ref")
	case *fastForward:
		return KindFastForward, nil
	}
	return KindRewrite, nil
}

func indexed(path string, i int) string { return fmt.Sprintf("%s[%d]", path, i) }
