package pipeline

import (
	"fmt"
	"sort"
)

// ExecutionOrder returns the executable order of a document's steps as step IDs
// (gates included), a pure function of the definition.
//
// Let steps be indexed by declaration order. Repeat until every step is
// emitted: among the not-yet-emitted steps whose needs are all emitted, select
// the smallest declaration index and append it. This is the lexicographically
// smallest topological order by declaration index; it is not breadth-first, and
// a gate occupies its position. needs keep controlling logical dependency; the
// order only serializes. It returns ErrInvalid for duplicate step IDs, unknown
// needs and cycles, which a Validated document never has.
func ExecutionOrder(steps []Step) ([]string, error) {
	index := make(map[string]int, len(steps))
	for i, s := range steps {
		if _, dup := index[s.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate step id in the execution order", ErrInvalid)
		}
		index[s.ID] = i
	}
	pending := make([]int, len(steps))
	dependents := make([][]int, len(steps))
	for i, s := range steps {
		seen := map[string]bool{}
		for _, need := range s.Needs {
			j, ok := index[need]
			if !ok || j == i {
				return nil, fmt.Errorf("%w: a dependency is not a distinct step of the pipeline", ErrInvalid)
			}
			if seen[need] {
				continue
			}
			seen[need] = true
			pending[i]++
			dependents[j] = append(dependents[j], i)
		}
	}
	var ready []int // kept sorted ascending: the head is the smallest declaration index
	for i := range steps {
		if pending[i] == 0 {
			ready = append(ready, i)
		}
	}
	order := make([]string, 0, len(steps))
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		order = append(order, steps[i].ID)
		for _, k := range dependents[i] {
			pending[k]--
			if pending[k] == 0 {
				at := sort.SearchInts(ready, k)
				ready = append(ready, 0)
				copy(ready[at+1:], ready[at:])
				ready[at] = k
			}
		}
	}
	if len(order) != len(steps) {
		return nil, fmt.Errorf("%w: the dependencies contain a cycle", ErrInvalid)
	}
	return order, nil
}

// ExecutionOrder is the executable order of the document's steps.
func (d Document) ExecutionOrder() ([]string, error) { return ExecutionOrder(d.Spec.Steps) }

// ExecutionOrder decodes the validated document and returns its executable
// order.
func (v Validated) ExecutionOrder() ([]string, error) {
	doc, err := v.Document()
	if err != nil {
		return nil, err
	}
	return doc.ExecutionOrder()
}
