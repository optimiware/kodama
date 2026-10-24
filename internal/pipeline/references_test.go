package pipeline_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
)

func Test_unknown_output(t *testing.T) {
	e := newEnv(t)
	producer := container("build", "outputs: {report: {type: artifact, path: /kodama/outputs/r.json}, version: {type: string}, ok: {type: boolean}, n: {type: integer}, image: {type: ociArtifact}}")
	consumer := func(env string) string { return container("use", "needs: [build]", "env: {"+env+"}") }

	t.Run("a declared ancestor output is accepted", func(t *testing.T) {
		e.mustValidate(t, doc(producer, consumer("V: '${{ steps.build.outputs.version }}'")))
	})
	t.Run("scalar outputs of every scalar type are accepted in env and args", func(t *testing.T) {
		e.mustValidate(t, doc(producer, container("use", "needs: [build]",
			"env: {V: '${{ steps.build.outputs.version }}', B: '${{ steps.build.outputs.ok }}', N: '${{ steps.build.outputs.n }}'}",
			"args: ['${{ steps.build.outputs.version }}']")))
	})
	t.Run("an undeclared output is rejected", func(t *testing.T) {
		_, err := e.validate(doc(producer, consumer("V: '${{ steps.build.outputs.missing }}'")))
		expectCode(t, err, pipeline.CodeUnknownOutput)
	})
	t.Run("an output of an unknown step is rejected", func(t *testing.T) {
		_, err := e.validate(doc(producer, consumer("V: '${{ steps.ghost.outputs.version }}'")))
		expectCode(t, err, pipeline.CodeUnknownStep)
	})
	t.Run("a nonancestor producer is rejected", func(t *testing.T) {
		_, err := e.validate(doc(producer, container("use", "env: {V: '${{ steps.build.outputs.version }}'}")))
		expectCode(t, err, pipeline.CodeNotAncestor)
	})
	t.Run("a descendant is not a producer", func(t *testing.T) {
		_, err := e.validate(doc(container("early", "env: {V: '${{ steps.late.outputs.x }}'}"), container("late", "needs: [early]", "outputs: {x: {type: string}}")))
		expectCode(t, err, pipeline.CodeNotAncestor)
	})
	t.Run("a step cannot reference its own outputs", func(t *testing.T) {
		_, err := e.validate(doc(container("a", "outputs: {x: {type: string}}", "env: {V: '${{ steps.a.outputs.x }}'}")))
		expectCode(t, err, pipeline.CodeNotAncestor)
	})
	t.Run("a transitive ancestor is enough", func(t *testing.T) {
		e.mustValidate(t, doc(producer, container("mid", "needs: [build]"), container("use", "needs: [mid]", "env: {V: '${{ steps.build.outputs.version }}'}")))
	})
	t.Run("the implicit commit output of an ancestor writer", func(t *testing.T) {
		w := container("edit", "workspace: {access: write, from: run}")
		e.mustValidate(t, doc(w, container("use", "needs: [edit]", "env: {C: '${{ steps.edit.outputs.commit }}'}")))
		_, err := e.validate(doc(container("reader"), container("use", "needs: [reader]", "env: {C: '${{ steps.reader.outputs.commit }}'}")))
		expectCode(t, err, pipeline.CodeUnknownOutput)
		_, err = e.validate(doc(container("reader", "workspace: {access: read, from: run}"), container("use", "needs: [reader]", "env: {C: '${{ steps.reader.outputs.commit }}'}")))
		expectCode(t, err, pipeline.CodeUnknownOutput)
	})
	t.Run("a gate has no outputs", func(t *testing.T) {
		_, err := e.validate(doc(container("a"), "{id: g, kind: gate, needs: [a]}", container("use", "needs: [g]", "env: {V: '${{ steps.g.outputs.x }}'}")))
		expectCode(t, err, pipeline.CodeUnknownOutput)
	})

	t.Run("reference types are checked", func(t *testing.T) {
		for name, env := range map[string]string{
			"artifact in env":     "V: '${{ steps.build.outputs.report }}'",
			"oci artifact in env": "V: '${{ steps.build.outputs.image }}'",
		} {
			t.Run(name, func(t *testing.T) {
				_, err := e.validate(doc(producer, consumer(env)))
				expectCode(t, err, pipeline.CodeTypeMismatch)
			})
		}
		_, err := e.validate(docWith("{a: {type: artifact}}", container("use", "env: {V: '${{ inputs.a }}'}")))
		expectCode(t, err, pipeline.CodeTypeMismatch)
	})

	t.Run("gate references", func(t *testing.T) {
		gate := func(g string) string { return "{id: review, kind: gate, needs: [build], gate: " + g + "}" }
		e.mustValidate(t, doc(producer, gate("{evidence: ['${{ steps.build.outputs.report }}']}")))
		e.mustValidate(t, doc(producer, gate("{deploymentId: '${{ steps.build.outputs.version }}'}")))
		e.mustValidate(t, docWith("{shot: {type: artifact}}", producer, gate("{evidence: ['${{ inputs.shot }}']}")))

		_, err := e.validate(doc(producer, gate("{evidence: ['${{ steps.build.outputs.version }}']}")))
		expectCode(t, err, pipeline.CodeTypeMismatch)
		_, err = e.validate(doc(producer, gate("{evidence: ['https://example.invalid/report.json']}")))
		expectCode(t, err, pipeline.CodeInvalidValue)
		_, err = e.validate(doc(producer, gate("{deploymentId: dep_1}")))
		expectCode(t, err, pipeline.CodeInvalidValue)
		_, err = e.validate(doc(producer, gate("{evidence: ['${{ steps.build.outputs.nope }}']}")))
		expectCode(t, err, pipeline.CodeUnknownOutput)
		_, err = e.validate(doc(producer, "{id: review, kind: gate, gate: {evidence: ['${{ steps.build.outputs.report }}']}}"))
		expectCode(t, err, pipeline.CodeNotAncestor)
		_, err = e.validate(doc(producer, gate("{deploymentId: '${{ steps.build.outputs.ok }}'}")))
		expectCode(t, err, pipeline.CodeTypeMismatch)
	})
}
