package pipeline

import (
	"strconv"
	"strings"
)

// graph is the dependency structure of a document's steps. needs holds only
// edges to known, distinct, non-self steps; findings about the others were
// already reported.
type graph struct {
	index map[string]int // step id -> position, first occurrence wins
	needs [][]int
	// anc[i][j] reports that step j is a (transitive) dependency of step i. It is
	// valid only when acyclic.
	anc     [][]bool
	acyclic bool
}

func stepPath(i int) string { return "$.spec.steps[" + strconv.Itoa(i) + "]" }

// buildGraph resolves dependencies and detects cycles. It reports unknown
// steps, duplicate needs, self-dependencies and cycles.
func buildGraph(steps []Step, d *diagnostics) *graph {
	g := &graph{index: make(map[string]int, len(steps)), needs: make([][]int, len(steps))}
	for i, s := range steps {
		if _, dup := g.index[s.ID]; !dup {
			g.index[s.ID] = i
		}
	}
	selfLoop := false
	for i, s := range steps {
		seen := map[string]bool{}
		for k, need := range s.Needs {
			p := stepPath(i) + ".needs[" + strconv.Itoa(k) + "]"
			j, known := g.index[need]
			switch {
			case seen[need]:
				d.add(CodeInvalidValue, p, "the dependency is listed more than once")
			case !known:
				d.add(CodeUnknownStep, p, "the dependency is not a step of this pipeline")
			case j == i:
				selfLoop = true
				d.add(CodeCycle, p, "a step cannot depend on itself")
			default:
				g.needs[i] = append(g.needs[i], j)
			}
			seen[need] = true
		}
	}
	g.acyclic = !selfLoop && g.detectCycles(steps, d)
	if g.acyclic {
		g.closure()
	}
	return g
}

// detectCycles reports one cycle per strongly connected back edge found by a
// depth-first search and returns true when the graph is acyclic.
func (g *graph) detectCycles(steps []Step, d *diagnostics) bool {
	const (
		white = iota
		grey
		black
	)
	color := make([]int, len(steps))
	var stack []int
	acyclic := true
	var visit func(i int)
	visit = func(i int) {
		color[i] = grey
		stack = append(stack, i)
		for _, j := range g.needs[i] {
			switch color[j] {
			case white:
				visit(j)
			case grey:
				acyclic = false
				start := len(stack) - 1
				for stack[start] != j {
					start--
				}
				names := make([]string, 0, len(stack)-start+1)
				for _, k := range stack[start:] {
					names = append(names, steps[k].ID)
				}
				names = append(names, steps[j].ID)
				d.add(CodeCycle, stepPath(j), "dependency cycle: %s", strings.Join(names, " -> "))
			}
		}
		stack = stack[:len(stack)-1]
		color[i] = black
	}
	for i := range steps {
		if color[i] == white {
			visit(i)
		}
	}
	return acyclic
}

// closure computes transitive dependencies of an acyclic graph.
func (g *graph) closure() {
	n := len(g.needs)
	g.anc = make([][]bool, n)
	var fill func(i int) []bool
	fill = func(i int) []bool {
		if g.anc[i] != nil {
			return g.anc[i]
		}
		row := make([]bool, n)
		g.anc[i] = row // marked before recursion; safe because the graph is acyclic
		for _, j := range g.needs[i] {
			row[j] = true
			for k, ok := range fill(j) {
				if ok {
					row[k] = true
				}
			}
		}
		return row
	}
	for i := 0; i < n; i++ {
		fill(i)
	}
}

// isAncestor reports whether step j is a transitive dependency of step i.
func (g *graph) isAncestor(i, j int) bool {
	return g.acyclic && i != j && g.anc[i][j]
}

// checkWriters requires every pair of workspace writers to be ordered by the
// graph: concurrent writers could publish conflicting source states. Conditional
// writers are still writers, because a condition can hold at run time.
func (g *graph) checkWriters(steps []Step, d *diagnostics) {
	if !g.acyclic {
		return
	}
	var writers []int
	for i, s := range steps {
		if s.IsWriter() {
			writers = append(writers, i)
		}
	}
	for a := 0; a < len(writers); a++ {
		for b := a + 1; b < len(writers); b++ {
			i, j := writers[a], writers[b]
			if !g.isAncestor(i, j) && !g.isAncestor(j, i) {
				d.add(CodeParallelWriters, stepPath(j), "steps %s and %s both write the workspace but neither depends on the other", steps[i].ID, steps[j].ID)
			}
		}
	}
}

// checkSource validates a read or write step's workspace.from: the run commit,
// or the published commit of a writer the step depends on.
func (g *graph) checkSource(steps []Step, i int, d *diagnostics) {
	s := steps[i]
	if !s.Executable() || s.Workspace == nil {
		return
	}
	p := stepPath(i) + ".workspace"
	switch s.Workspace.Access {
	case AccessNone:
		if s.Workspace.From != "" {
			d.add(CodeInvalidSource, p+".from", "workspace access none cannot name a source")
		}
	case AccessRead, AccessWrite:
		from := s.Workspace.From
		if from == RunSource {
			return
		}
		j, ok := g.index[from]
		switch {
		case from == "":
			d.add(CodeInvalidSource, p+".from", "read and write access must name a source: run or an ancestor writer")
		case !ok:
			d.add(CodeInvalidSource, p+".from", "the source is neither run nor a step of this pipeline")
		case !steps[j].IsWriter():
			d.add(CodeInvalidSource, p+".from", "the source step does not write the workspace")
		case g.acyclic && !g.isAncestor(i, j):
			d.add(CodeInvalidSource, p+".from", "the source writer is not a dependency of this step")
		}
	}
}
