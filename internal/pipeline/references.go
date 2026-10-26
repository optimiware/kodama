package pipeline

import (
	"regexp"
	"strings"
)

// exprMarker opens every expression. Only a whole scalar of the form
// ${{ inputs.NAME }} or ${{ steps.ID.outputs.NAME }} is an expression; the same
// marker anywhere else, including inside a longer string, is rejected so no
// value is ever interpolated into a command, path or shell program.
const exprMarker = "${{"

var exprRE = regexp.MustCompile(`^\$\{\{[ \t]*(?:inputs\.([a-z][a-z0-9_-]{0,62})|steps\.([a-z][a-z0-9-]{0,62})\.outputs\.([a-z][a-z0-9_-]{0,62}))[ \t]*\}\}$`)

// reference is a parsed whole-scalar expression.
type reference struct {
	input  string // set for inputs.NAME
	step   string // set for steps.ID.outputs.NAME
	output string
}

func (r reference) isInput() bool { return r.input != "" }

// parseReference classifies a string: not an expression at all, a valid
// whole-scalar reference, or an invalid use of the expression marker.
func parseReference(s string) (ref reference, isExpr, valid bool) {
	if !strings.Contains(s, exprMarker) {
		return reference{}, false, false
	}
	m := exprRE.FindStringSubmatch(s)
	if m == nil {
		return reference{}, true, false
	}
	if m[1] != "" {
		return reference{input: m[1]}, true, true
	}
	return reference{step: m[2], output: m[3]}, true, true
}

// refSite describes where a string sits and which values it may reference.
type refSite struct {
	path string
	// consumer is the position of the referencing step.
	consumer int
	// allowed lists the accepted referenced types.
	allowed []ValueType
	// requireRef rejects a literal: the site only accepts a reference.
	requireRef bool
	// endpoints marks a site where a kodama/v1alpha2 service endpoint reference
	// is allowed. It has no effect on kodama/v1alpha1 documents.
	endpoints bool
}

func (s refSite) accepts(t ValueType) bool {
	for _, a := range s.allowed {
		if a == t {
			return true
		}
	}
	return false
}

var scalarTypes = []ValueType{TypeString, TypeBoolean, TypeInteger}

// resolver resolves references of one validation run.
type resolver struct {
	doc   Document
	graph *graph
	d     *diagnostics
	// v2 enables the kodama/v1alpha2 endpoint expressions. It stays false for
	// kodama/v1alpha1 documents, where the same text is an interpolation error.
	v2 bool
}

// noExpression rejects the expression marker in a string that is not an
// expression site.
func (r *resolver) noExpression(path, s string) {
	if strings.Contains(s, exprMarker) {
		r.d.add(CodeInterpolation, path, "expressions are not allowed here; only whole-scalar references in environment values, arguments and gate references are supported")
	}
}

// check validates a string at an expression site. A literal passes unless the
// site requires a reference.
func (r *resolver) check(site refSite, s string) {
	if r.v2 {
		if ep, ok := parseEndpoint(s); ok {
			r.checkEndpoint(site, ep)
			return
		}
	}
	ref, isExpr, valid := parseReference(s)
	if !isExpr {
		if site.requireRef {
			r.d.add(CodeInvalidValue, site.path, "a whole-scalar reference such as ${{ steps.ID.outputs.NAME }} is required")
		}
		return
	}
	if !valid {
		r.d.add(CodeInterpolation, site.path, "only a whole-scalar ${{ inputs.NAME }} or ${{ steps.ID.outputs.NAME }} reference is supported, never interpolation")
		return
	}
	typ, ok := r.resolve(site, ref)
	if !ok {
		return
	}
	if !site.accepts(typ) {
		r.d.add(CodeTypeMismatch, site.path, "the referenced value has type %s, which this field does not accept", typ)
	}
}

// resolve finds the referenced declaration and returns its type. It reports
// unknown inputs, unknown or nonancestor producers and undeclared outputs.
func (r *resolver) resolve(site refSite, ref reference) (ValueType, bool) {
	if ref.isInput() {
		in, ok := r.doc.Spec.Inputs[ref.input]
		if !ok {
			r.d.add(CodeUnknownInput, site.path, "the input %s is not declared", ref.input)
			return "", false
		}
		return in.Type, true
	}
	j, ok := r.graph.index[ref.step]
	if !ok {
		r.d.add(CodeUnknownStep, site.path, "the referenced step %s does not exist", ref.step)
		return "", false
	}
	if !r.graph.acyclic {
		return "", false // already reported as a cycle; ancestry is undefined
	}
	if !r.graph.isAncestor(site.consumer, j) {
		r.d.add(CodeNotAncestor, site.path, "the step %s is not a dependency of this step, so its outputs are not available", ref.step)
		return "", false
	}
	producer := r.doc.Spec.Steps[j]
	if out, ok := producer.Outputs[ref.output]; ok && producer.Executable() {
		return out.Type, true
	}
	if producer.IsWriter() && ref.output == ImplicitCommitOutput {
		return TypeString, true
	}
	r.d.add(CodeUnknownOutput, site.path, "the step %s declares no output %s", ref.step, ref.output)
	return "", false
}
