// Package tasks owns project-defined task workflows and asynchronous inbox
// decisions. It does not depend on pipeline scheduling, agents or transports.
package tasks

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/optimiware/kodama/internal/domain"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

type Transition struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Inbox struct {
	OpenState     string `json:"openState"`
	ResolvedState string `json:"resolvedState"`
}

// Workflow is stored whole in projects.task_workflow. Names, including inbox
// states, belong to the project. Parking does not add a product-wide state.
type Workflow struct {
	InitialState   string            `json:"initialState"`
	States         []string          `json:"states"`
	TerminalStates []string          `json:"terminalStates"`
	Transitions    []Transition      `json:"transitions"`
	Triggers       map[string]string `json:"triggers,omitempty"`
	Inbox          Inbox             `json:"inbox"`
}

func (w Workflow) Validate() error {
	if len(w.States) == 0 || len(w.States) > 64 || len(w.Transitions) > 1024 {
		return invalid("workflow size")
	}
	seen := map[string]bool{}
	for _, state := range w.States {
		if !namePattern.MatchString(state) || seen[state] {
			return invalid("invalid or duplicate state")
		}
		seen[state] = true
	}
	if !seen[w.InitialState] || !seen[w.Inbox.OpenState] || !seen[w.Inbox.ResolvedState] {
		return invalid("initial and inbox states must be declared")
	}
	terminals := map[string]bool{}
	for _, state := range w.TerminalStates {
		if !seen[state] || terminals[state] {
			return invalid("invalid or duplicate terminal state")
		}
		terminals[state] = true
	}
	if terminals[w.InitialState] || terminals[w.Inbox.OpenState] || !terminals[w.Inbox.ResolvedState] {
		return invalid("initial/open states must be nonterminal and resolved state terminal")
	}
	edges := map[[2]string]bool{}
	for _, edge := range w.Transitions {
		key := [2]string{edge.From, edge.To}
		if !seen[edge.From] || !seen[edge.To] || edge.From == edge.To || edges[key] || terminals[edge.From] {
			return invalid("invalid or duplicate transition")
		}
		edges[key] = true
	}
	for state, pipeline := range w.Triggers {
		if !seen[state] || terminals[state] || !namePattern.MatchString(pipeline) {
			return invalid("invalid state trigger")
		}
	}
	return nil
}

func (w Workflow) Edge(from, to string) (Transition, bool) {
	for _, edge := range w.Transitions {
		if edge.From == from && edge.To == to {
			return edge, true
		}
	}
	return Transition{}, false
}

func (w Workflow) Terminal(state string) bool { return slices.Contains(w.TerminalStates, state) }

func (w Workflow) Clone() Workflow {
	w.States = slices.Clone(w.States)
	w.TerminalStates = slices.Clone(w.TerminalStates)
	w.Transitions = slices.Clone(w.Transitions)
	w.Triggers = maps.Clone(w.Triggers)
	return w
}

func invalid(message string) error { return fmt.Errorf("%w: %s", domain.ErrInvalidValue, message) }
