//go:build e2e

// Package smoke holds the installation-neutral helpers shared by the release
// smoke scenarios (TestReleaseSmoke/*). Every helper works on public API
// observations only, so its negative cases are proved hermetically by the
// TestSmoke* tests in this package. The non-Git scenarios use the request-wait,
// operator-retry and conversation-handoff helpers; the Git scenarios build the
// repository seed, protected task and human push on the same
// names/version/reference helpers.
package smoke

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/e2e/internal/harness"
)

// identityPattern accepts an attributable, bounded installation identity. The
// product's opaque prefixes are not part of the public contract.
var identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)

// MissingInputs names every installation input the release-smoke scenarios
// read. With Git enabled (the default) S2 and S5 need the served Git endpoints,
// a registered repository and the external-harness pin as well as the four
// personas, so the full installation preflight set is required. With
// Q01_SMOKE_GIT=0 (a non-Git run) the Git endpoint and repository are
// omitted and the three API-only scenarios still require the four personas and
// both digest pins. A missing credential or pin is a named failure in required
// mode, never a scenario skip.
func MissingInputs(config harness.Config) harness.MissingInputs {
	if config.GitSmokeEnabled() {
		return config.InstallationMissing()
	}
	return config.InstallationMissingNonGit()
}

// CheckMarker requires exactly want occurrences of marker in retained log
// bytes. A missing positive marker and a duplicated continuation marker are
// both refused.
func CheckMarker(text []byte, marker string, want int) error {
	if marker == "" {
		return fmt.Errorf("marker is empty")
	}
	if want < 0 {
		return fmt.Errorf("wanted marker count %d is negative", want)
	}
	if got := bytes.Count(text, []byte(marker)); got != want {
		return fmt.Errorf("marker %q appears %d time(s); want exactly %d", marker, got, want)
	}
	return nil
}

// CheckMutationVersion requires a mutation to carry the version it observed. A
// caller that sends any other value issued a wrong-version request, which the
// product must refuse rather than mutate.
func CheckMutationVersion(sent, observed uint64) error {
	if observed == 0 {
		return fmt.Errorf("observed version is zero; a mutation needs a positive observed version")
	}
	if sent != observed {
		return fmt.Errorf("mutation carried expected version %d; want the observed %d", sent, observed)
	}
	return nil
}

// CheckReplayConverged requires an identical replay to return exactly the first
// mutation result.
func CheckReplayConverged[T comparable](first, replay T) error {
	if first != replay {
		return fmt.Errorf("replay returned %v; want the original result %v", replay, first)
	}
	return nil
}

// CheckExactHistory requires the task execution history to be exactly want, in
// order, so a replay that created a second execution is refused.
func CheckExactHistory(got []string, want ...string) error {
	if len(got) != len(want) {
		return fmt.Errorf("task history has %d execution(s) %v; want exactly %d %v", len(got), got, len(want), want)
	}
	for index := range want {
		if got[index] != want[index] {
			return fmt.Errorf("task history[%d] = %q; want %q (history %v)", index, got[index], want[index], got)
		}
	}
	return nil
}

// CheckExecutionSet requires the project's execution identity set to be
// unchanged. On a shared installation the project-wide set is not a valid
// isolate for one settlement: an unrelated background execution (for example a
// held push rescan left by an earlier test) can start inside the same window.
// Prefer CheckScopedExecutionSet for a settlement unless the caller owns the
// whole project for the duration.
func CheckExecutionSet(before, after []string) error {
	left := append([]string(nil), before...)
	right := append([]string(nil), after...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return fmt.Errorf("execution set has %d entries after; want the unchanged %d (before %v, after %v)", len(right), len(left), left, right)
	}
	for index := range left {
		if left[index] != right[index] {
			return fmt.Errorf("execution set changed: before %v, after %v", left, right)
		}
	}
	return nil
}

// ExecutionSetDelta returns the execution identities present in after but not
// in before, sorted. An identity of before that is missing from after is an
// error. Unlike CheckExecutionSet it tolerates executions admitted meanwhile,
// so a scenario running beside others can still inspect exactly what appeared.
func ExecutionSetDelta(before, after []string) ([]string, error) {
	known := make(map[string]bool, len(before))
	for _, id := range before {
		known[id] = true
	}
	present := make(map[string]bool, len(after))
	var added []string
	for _, id := range after {
		if !known[id] && !present[id] {
			added = append(added, id)
		}
		present[id] = true
	}
	for _, id := range before {
		if !present[id] {
			return nil, fmt.Errorf("execution %q disappeared from the project list (before %v, after %v)", id, before, after)
		}
	}
	sort.Strings(added)
	return added, nil
}

// CheckForbiddenActionNames requires an execution to carry none of the named
// actions. With ExecutionSetDelta it proves a refused or terminal request
// admitted nothing: an execution a concurrent scenario admitted passes, one
// that carries the scenario's own action does not.
func CheckForbiddenActionNames(view api.ExecutionInspectResult, forbidden ...string) error {
	for _, action := range view.Actions {
		for _, name := range forbidden {
			if action.Name == name {
				return fmt.Errorf("execution %q carries the action %q, which must not have been admitted", view.ExecutionID, action.Name)
			}
		}
	}
	return nil
}

// CheckExecutionContinued requires a settlement to continue the exact execution
// it was issued against rather than start or replay another one. The execution
// identity and its task/pipeline attribution survive, the version advances (a
// restart would reset it), and the admitted action identities are unchanged.
func CheckExecutionContinued(before, after api.ExecutionInspectResult) error {
	if after.ExecutionID != before.ExecutionID {
		return fmt.Errorf("settlement returned execution %q; want the continued %q", after.ExecutionID, before.ExecutionID)
	}
	if after.TaskID != before.TaskID || after.PipelineID != before.PipelineID {
		return fmt.Errorf("execution attribution changed from task %q pipeline %q to task %q pipeline %q",
			before.TaskID, before.PipelineID, after.TaskID, after.PipelineID)
	}
	if after.Version <= before.Version {
		return fmt.Errorf("execution version is %d after; want > the %d observed before the settlement (a restart resets it)", after.Version, before.Version)
	}
	if len(after.Actions) != len(before.Actions) {
		return fmt.Errorf("execution has %d action(s) after; want the admitted %d", len(after.Actions), len(before.Actions))
	}
	admitted := make(map[string]api.ExecutionAction, len(before.Actions))
	for _, action := range before.Actions {
		admitted[action.ActionID] = action
	}
	for index, action := range after.Actions {
		original, ok := admitted[action.ActionID]
		if !ok {
			return fmt.Errorf("settlement added action %q (%q); want only the admitted actions", action.Name, action.ActionID)
		}
		if action.Index != index || action.Index != original.Index || action.Name != original.Name {
			return fmt.Errorf("action %q moved from position %d/%q to %d/%q", action.ActionID,
				original.Index, original.Name, action.Index, action.Name)
		}
	}
	return nil
}

// CheckScopedExecutionSet requires the execution history attributed to one task
// or stored pipeline to be unchanged, and to still contain the execution under
// test. Unlike CheckExecutionSet it ignores executions attributed to other
// tasks and pipelines, so a shared installation cannot invalidate a settlement.
func CheckScopedExecutionSet(before, after []string, executionID string) error {
	if executionID == "" {
		return fmt.Errorf("execution identity is empty; a scoped set needs the execution under test")
	}
	if err := CheckExecutionSet(before, after); err != nil {
		return fmt.Errorf("execution set for the task/pipeline %q changed: %w", executionID, err)
	}
	if slices.Contains(after, executionID) {
		return nil
	}
	return fmt.Errorf("scoped execution set %v does not contain the execution under test %q", after, executionID)
}

// CheckDecisionActor requires a completed request action to record wantDecision
// and the exact persona principal wantActor.
func CheckDecisionActor(action api.ExecutionAction, wantDecision, wantActor string) error {
	if action.Decision != wantDecision {
		return fmt.Errorf("request decision = %q; want %q", action.Decision, wantDecision)
	}
	if action.Actor != wantActor {
		return fmt.Errorf("request decision actor = %q; want %q", action.Actor, wantActor)
	}
	if action.AnsweredAt == nil {
		return fmt.Errorf("request decision has no answered_at")
	}
	return nil
}

// CheckSubjectEqual requires two immutable request subjects to carry the same
// field set. Field order is not semantic; a changed or missing field is.
func CheckSubjectEqual(want, got *api.Subject) error {
	if want == nil || got == nil {
		if want == nil && got == nil {
			return nil
		}
		return fmt.Errorf("request subject is missing on one side (want present=%v, got present=%v)", want != nil, got != nil)
	}
	if len(want.Fields) != len(got.Fields) {
		return fmt.Errorf("request subject has %d field(s) after; want the %d observed while waiting", len(got.Fields), len(want.Fields))
	}
	expected := make(map[string]api.SubjectField, len(want.Fields))
	for _, field := range want.Fields {
		expected[field.Name] = field
	}
	for _, field := range got.Fields {
		original, ok := expected[field.Name]
		if !ok {
			return fmt.Errorf("request subject gained field %q after resolution", field.Name)
		}
		if original.Kind != field.Kind || original.Value != field.Value {
			return fmt.Errorf("request subject field %q changed from kind=%q value=%q to kind=%q value=%q", field.Name, original.Kind, original.Value, field.Kind, field.Value)
		}
	}
	return nil
}

// CheckDistinctWorkspaces requires two nonempty, bounded and different cleanup
// workspace identities, so a handoff or retry cannot reuse a workspace.
func CheckDistinctWorkspaces(first, second string) error {
	if !identityPattern.MatchString(first) {
		return fmt.Errorf("first workspace identity %q is not a nonempty bounded identity", first)
	}
	if !identityPattern.MatchString(second) {
		return fmt.Errorf("second workspace identity %q is not a nonempty bounded identity", second)
	}
	if first == second {
		return fmt.Errorf("both executions reported workspace %q; a fresh attempt must not reuse it", first)
	}
	return nil
}

// CheckFreshActionIDs requires every action of the fresh execution to carry a
// nonempty identity that the earlier execution never used.
func CheckFreshActionIDs(first, fresh api.ExecutionInspectResult) error {
	used := make(map[string]bool, len(first.Actions))
	for _, action := range first.Actions {
		used[action.ActionID] = true
	}
	seen := make(map[string]bool, len(fresh.Actions))
	for _, action := range fresh.Actions {
		if action.ActionID == "" {
			return fmt.Errorf("fresh action %q has no identity", action.Name)
		}
		if used[action.ActionID] {
			return fmt.Errorf("fresh action %q reused the earlier action id %q", action.Name, action.ActionID)
		}
		if seen[action.ActionID] {
			return fmt.Errorf("fresh execution repeats action id %q", action.ActionID)
		}
		seen[action.ActionID] = true
	}
	return nil
}

// CheckBodyMessage requires one exact durable message body authored by want in
// the task conversation.
func CheckBodyMessage(messages []api.TaskMessage, body, want string) error {
	for _, message := range messages {
		if message.Body == body && message.Author == want {
			return nil
		}
	}
	for _, message := range messages {
		if message.Body == body {
			return fmt.Errorf("durable message %q was authored by %q; want %q", body, message.Author, want)
		}
	}
	return fmt.Errorf("task conversation has no durable message %q", body)
}
