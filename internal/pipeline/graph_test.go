package pipeline_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
)

func writer(id string, from string, extra ...string) string {
	return container(id, append([]string{"workspace: {access: write, from: " + from + "}"}, extra...)...)
}

func Test_parallel_writers(t *testing.T) {
	e := newEnv(t)
	t.Run("two independent writers are rejected", func(t *testing.T) {
		_, err := e.validate(doc(writer("a", "run"), writer("b", "run")))
		expectCode(t, err, pipeline.CodeParallelWriters)
	})
	t.Run("conditional writers still count", func(t *testing.T) {
		_, err := e.validate(docWith("{fast: {type: boolean, default: false}}",
			writer("a", "run", "when: {input: fast, equals: true}"),
			writer("b", "run", "when: {input: fast, equals: false}")))
		expectCode(t, err, pipeline.CodeParallelWriters)
	})
	t.Run("writers on different branches of a diamond are rejected", func(t *testing.T) {
		_, err := e.validate(doc(container("root"), writer("l", "run", "needs: [root]"), writer("r", "run", "needs: [root]"), container("join", "needs: [l, r]")))
		expectCode(t, err, pipeline.CodeParallelWriters)
	})
	t.Run("a writer concurrent with a deeper writer is rejected", func(t *testing.T) {
		_, err := e.validate(doc(writer("a", "run"), container("x"), writer("b", "run", "needs: [x]")))
		expectCode(t, err, pipeline.CodeParallelWriters)
	})
	t.Run("writers ordered directly are accepted", func(t *testing.T) {
		e.mustValidate(t, doc(writer("a", "run"), writer("b", "a", "needs: [a]")))
	})
	t.Run("writers ordered transitively are accepted", func(t *testing.T) {
		e.mustValidate(t, doc(writer("a", "run"), container("m", "needs: [a]"), writer("b", "a", "needs: [m]"), container("m2", "needs: [b]"), writer("c", "b", "needs: [m2]")))
	})
	t.Run("many readers beside a writer are fine", func(t *testing.T) {
		e.mustValidate(t, doc(writer("a", "run"), container("r1", "workspace: {access: read, from: run}"), container("r2", "needs: [a]", "workspace: {access: read, from: a}")))
	})
	t.Run("a gate is not a writer", func(t *testing.T) {
		e.mustValidate(t, doc(writer("a", "run"), "{id: review, kind: gate, needs: [a]}", writer("b", "a", "needs: [review]")))
	})
	t.Run("agent writers are ordered like container writers", func(t *testing.T) {
		agent := "{id: impl, kind: agent, agent: {profile: coding-v1}, workspace: {access: write, from: run}, timeoutSeconds: 60}"
		_, err := e.validate(doc(agent, writer("b", "run")))
		expectCode(t, err, pipeline.CodeParallelWriters)
		e.mustValidate(t, doc(agent, writer("b", "impl", "needs: [impl]")))
	})
	t.Run("a declared commit output contradicts the implicit one", func(t *testing.T) {
		_, err := e.validate(doc(writer("a", "run", "outputs: {commit: {type: string}}")))
		expectCode(t, err, pipeline.CodeReservedOutput)
		e.mustValidate(t, doc(container("reader", "outputs: {commit: {type: string}}")))
	})
	t.Run("an unordered pair is reported once at the later step", func(t *testing.T) {
		_, err := e.validate(doc(writer("a", "run"), writer("b", "run")))
		if !hasDiag(err, pipeline.CodeParallelWriters, "steps[1]") || len(codes(err)) != 1 {
			t.Fatalf("unexpected diagnostics: %v", err)
		}
	})
}
