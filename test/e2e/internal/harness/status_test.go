//go:build e2e

package harness

import (
	"testing"

	"github.com/optimiware/kodama/api"
)

// apiAction builds one valid action observation.
func apiAction(id string, index int, name, state string) api.ExecutionAction {
	return api.ExecutionAction{
		ActionID: id,
		Index:    index,
		Name:     name,
		Kind:     "container",
		State:    state,
		Version:  1,
	}
}

// TestHarnessStatusInterpretation proves every declared execution and cleanup
// state is interpreted, terminal and success are distinguished, and the
// single-active-action invariant is enforced.
func TestHarnessStatusInterpretation(t *testing.T) {
	cases := []struct {
		state    string
		terminal bool
		succeeds bool
	}{
		{state: "queued"},
		{state: "running"},
		{state: "succeeded", terminal: true, succeeds: true},
		{state: "failed", terminal: true},
		{state: "cancelled", terminal: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.state, func(t *testing.T) {
			status, err := Interpret(inspectResult(testCase.state, "none"))
			if err != nil {
				t.Fatalf("interpret %q: %v", testCase.state, err)
			}
			if status.Terminal != testCase.terminal {
				t.Errorf("terminal = %v; want %v", status.Terminal, testCase.terminal)
			}
			if status.Succeeded != testCase.succeeds {
				t.Errorf("succeeded = %v; want %v", status.Succeeded, testCase.succeeds)
			}
			if IsTerminalState(testCase.state) != testCase.terminal {
				t.Errorf("IsTerminalState(%q) = %v; want %v", testCase.state, IsTerminalState(testCase.state), testCase.terminal)
			}
		})
	}

	for _, cleanup := range []string{"none", "pending", "complete"} {
		status, err := Interpret(inspectResult("running", cleanup))
		if err != nil {
			t.Fatalf("interpret cleanup %q: %v", cleanup, err)
		}
		if status.CleanupState != cleanup {
			t.Errorf("cleanup state = %q; want %q", status.CleanupState, cleanup)
		}
		if status.CleanupComplete != (cleanup == "complete") {
			t.Errorf("cleanup complete = %v for %q", status.CleanupComplete, cleanup)
		}
	}
}

// TestHarnessStatusRefusesUnknownAndOverlapping proves an unknown state, an
// unknown cleanup state, overlapping running actions and out-of-order indexes
// are refused.
func TestHarnessStatusRefusesUnknownAndOverlapping(t *testing.T) {
	first := apiAction("act-1", 0, "first", "running")
	second := apiAction("act-2", 1, "second", "running")
	if _, err := Interpret(inspectResult("running", "none", first, second)); err == nil {
		t.Fatal("two running actions were accepted")
	}

	outOfOrder := inspectResult("running", "none", apiAction("act-1", 1, "first", "pending"))
	if _, err := Interpret(outOfOrder); err == nil {
		t.Fatal("an out-of-order action index was accepted")
	}

	if _, err := Interpret(inspectResult("done", "none")); err == nil {
		t.Fatal("an unknown execution state was accepted")
	}
	if _, err := Interpret(inspectResult("running", "done")); err == nil {
		t.Fatal("an unknown cleanup state was accepted")
	}

	single, err := Interpret(inspectResult("running", "none", first))
	if err != nil {
		t.Fatalf("single active action refused: %v", err)
	}
	if single.RunningActions != 1 || single.ActiveActionIndex != 0 {
		t.Fatalf("running actions = %d at %d; want 1 at 0", single.RunningActions, single.ActiveActionIndex)
	}
}
