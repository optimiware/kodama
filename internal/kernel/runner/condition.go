package runner

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// The frozen condition grammar of the pipeline format (contracts/pipeline-format.md,
// "Conditions"):
//
//	expr    := and ( "||" and )*
//	and     := not ( "&&" not )*
//	not     := "!" not | "(" expr ")" | cmp | operand
//	cmp     := operand ( "==" | "!=" | "<" | "<=" | ">" | ">=" ) operand
//	         | operand "in" "[" literal ( "," literal )* "]"
//	operand := reference | literal
//	literal := '"' string '"' | integer | "true" | "false"
//
// A condition reads only declared inputs, the status of prerequisite steps and the
// outcome, answer and outputs of settled ones. It executes no code, calls no
// function and reads neither environment, time, shell nor task. Comparison
// operands have the same type and are never coerced; a missing value makes every
// comparison, including != and in, false; a bare missing boolean is false.

// Bounds of the grammar.
const (
	maxConditionComparisons = 8
	maxConditionDepth       = 4
	maxConditionString      = 64
	maxConditionInList      = 8
)

// ErrCondition reports a condition the grammar refuses: a syntax error, a bound
// exceeded, an undeclared or invisible reference, or operands of different types.
// A missing value is never this error: it is a runtime outcome.
var ErrCondition = errors.New("runner: invalid condition")

func conditionErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCondition, fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------
// References
// ---------------------------------------------------------------------------

var (
	refNameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	refOutputRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	stepRefRE   = regexp.MustCompile(domain.IDPattern)
)

type refKind int

const (
	refInput refKind = iota
	refStatus
	refOutcome
	refAnswer
	refOutput
)

type reference struct {
	text string
	kind refKind
	step domain.StepID
	name string
}

// parseReference parses
//
//	reference := "inputs." name | step "." ( "status" | "outcome" | "answer" | "outputs." name )
func parseReference(text string) (reference, error) {
	parts := strings.Split(text, ".")
	r := reference{text: text}
	switch {
	case len(parts) == 2 && parts[0] == "inputs":
		if !refNameRE.MatchString(parts[1]) {
			return r, conditionErr("input name %q", parts[1])
		}
		r.kind, r.name = refInput, parts[1]
		return r, nil
	case len(parts) == 2 && stepRefRE.MatchString(parts[0]):
		r.step = domain.StepID(parts[0])
		switch parts[1] {
		case "status":
			r.kind = refStatus
		case "outcome":
			r.kind = refOutcome
		case "answer":
			r.kind = refAnswer
		default:
			return r, conditionErr("reference %q", text)
		}
		return r, nil
	case len(parts) == 3 && stepRefRE.MatchString(parts[0]) && parts[1] == "outputs":
		if !refOutputRE.MatchString(parts[2]) {
			return r, conditionErr("output name %q", parts[2])
		}
		r.kind, r.step, r.name = refOutput, domain.StepID(parts[0]), parts[2]
		return r, nil
	}
	return r, conditionErr("reference %q", text)
}

// ReferenceReader returns the typed value of a reference and whether it is present. A
// missing value still carries its type.
type ReferenceReader func(ref string) (runtime.InputValue, bool, error)

// prerequisites returns the transitive depends_on closure of a step: the only
// steps a reference of that step may name.
func (d *drive) prerequisites(ps runtime.PlanStep) map[domain.StepID]bool {
	seen := map[domain.StepID]bool{}
	var walk func(deps []domain.StepID)
	walk = func(deps []domain.StepID) {
		for _, k := range deps {
			if seen[k] {
				continue
			}
			seen[k] = true
			walk(d.planStep(k).DependsOn)
		}
	}
	walk(ps.DependsOn)
	return seen
}

// reader reads references for ps from persisted values only: the admitted
// inputs, recorded step states, recorded wait decisions and the outputs port. A
// reference to anything but a declared input or a transitive prerequisite is
// refused, whatever the format validation said.
func (d *drive) reader(ps runtime.PlanStep) ReferenceReader {
	visible := d.prerequisites(ps)
	str := func(v string, present bool) (runtime.InputValue, bool, error) {
		if !present {
			return runtime.StringInput(""), false, nil
		}
		return runtime.StringInput(v), true, nil
	}
	return func(text string) (runtime.InputValue, bool, error) {
		r, err := parseReference(text)
		if err != nil {
			return runtime.InputValue{}, false, err
		}
		if r.kind == refInput {
			v, ok := d.rec.Plan.Inputs[r.name]
			if !ok {
				return runtime.InputValue{}, false, conditionErr("input %q is not declared", r.name)
			}
			return v, true, nil
		}
		if !visible[r.step] {
			return runtime.InputValue{}, false, conditionErr("%q is not a prerequisite of %s", r.step, ps.Key)
		}
		s, target := d.step(r.step), d.planStep(r.step)
		if !s.State.Terminal() {
			return runtime.InputValue{}, false, conditionErr("prerequisite %s is %s", r.step, s.State)
		}
		switch r.kind {
		case refStatus:
			return runtime.StringInput(string(s.State)), true, nil
		case refOutcome, refAnswer:
			if target.Kind != runtime.StepWait {
				return runtime.InputValue{}, false, conditionErr("%q applies to wait steps only", text)
			}
			w, ok := d.wait(r.step)
			if !ok || s.State != runtime.StepSucceeded {
				return str("", false)
			}
			if r.kind == refOutcome {
				return str(w.Decision.Outcome, w.Decision.Outcome != "")
			}
			return str(w.Decision.Answer, w.Decision.Answer != "")
		}
		if target.Kind != runtime.StepContainer || !slices.Contains(target.Outputs, r.name) {
			return runtime.InputValue{}, false, conditionErr("%q is not a declared output", text)
		}
		// Only a succeeded step has outputs; those of any other outcome were never
		// recorded. They are read from the run record, whichever engine loaded it.
		if s.State != runtime.StepSucceeded {
			return str("", false)
		}
		v, ok := d.rec.Output(r.step, r.name)
		return str(v, ok)
	}
}

// evaluateWhen evaluates a step's predicate once over persisted values and
// returns the evaluation to record: its result and a digest of every value read.
func (d *drive) evaluateWhen(ps runtime.PlanStep) (runtime.PredicateEvaluation, error) {
	c, err := ParseCondition(ps.When)
	if err != nil {
		return runtime.PredicateEvaluation{}, err
	}
	res, reads, err := c.Evaluate(d.reader(ps))
	if err != nil {
		return runtime.PredicateEvaluation{}, err
	}
	digest, err := runtime.DigestPredicateReads(reads)
	if err != nil {
		return runtime.PredicateEvaluation{}, err
	}
	return runtime.PredicateEvaluation{Evaluated: true, Result: res, InputDigest: digest, At: d.now()}, nil
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

// Condition is a parsed when predicate.
type Condition struct {
	root node
	refs []string
}

type node interface{}

type (
	orNode  struct{ l, r node }
	andNode struct{ l, r node }
	notNode struct{ x node }
	cmpNode struct {
		op   string
		l, r operand
	}
	inNode struct {
		x    operand
		list []runtime.InputValue
	}
	bareNode struct{ x operand }
)

// operand is a reference or a literal.
type operand struct {
	ref *reference
	lit *runtime.InputValue
}

type token struct {
	kind string // "ref", "str", "int", "bool", "in", or the punctuation itself
	text string
	val  runtime.InputValue
}

func lex(src string) ([]token, error) {
	if len(src) > runtime.MaxWhenBytes {
		return nil, conditionErr("expression is longer than %d bytes", runtime.MaxWhenBytes)
	}
	var out []token
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == '[' || c == ']' || c == ',':
			out = append(out, token{kind: string(c)})
			i++
		case strings.HasPrefix(src[i:], "&&"), strings.HasPrefix(src[i:], "||"), strings.HasPrefix(src[i:], "=="),
			strings.HasPrefix(src[i:], "!="), strings.HasPrefix(src[i:], "<="), strings.HasPrefix(src[i:], ">="):
			out = append(out, token{kind: src[i : i+2]})
			i += 2
		case c == '!' || c == '<' || c == '>':
			out = append(out, token{kind: string(c)})
			i++
		case c == '"':
			end := strings.IndexByte(src[i+1:], '"')
			if end < 0 {
				return nil, conditionErr("unterminated string")
			}
			s := src[i+1 : i+1+end]
			if len(s) > maxConditionString || !utf8.ValidString(s) {
				return nil, conditionErr("a string literal is at most %d bytes of UTF-8", maxConditionString)
			}
			out = append(out, token{kind: "lit", val: runtime.StringInput(s)})
			i += end + 2
		case c >= '0' && c <= '9' || (c == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i + 1
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			n, err := strconv.ParseInt(src[i:j], 10, 64)
			if err != nil {
				return nil, conditionErr("integer %q is outside 64 bits", src[i:j])
			}
			out = append(out, token{kind: "lit", val: runtime.IntegerInput(n)})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(src) && (src[j] == '_' || src[j] == '.' || src[j] == '-' || src[j] >= '0' && src[j] <= '9' ||
				src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z') {
				j++
			}
			word := src[i:j]
			switch word {
			case "true", "false":
				out = append(out, token{kind: "lit", val: runtime.BooleanInput(word == "true")})
			case "in":
				out = append(out, token{kind: "in"})
			default:
				out = append(out, token{kind: "ref", text: word})
			}
			i = j
		default:
			return nil, conditionErr("unexpected %q", string(c))
		}
	}
	return out, nil
}

type parser struct {
	toks  []token
	pos   int
	cmps  int
	depth int
	refs  map[string]bool
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos].kind
	}
	return ""
}

func (p *parser) take(kind string) bool {
	if p.peek() == kind {
		p.pos++
		return true
	}
	return false
}

// ParseCondition parses and bounds-checks a when predicate without evaluating it.
func ParseCondition(src string) (*Condition, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, conditionErr("an empty condition")
	}
	p := &parser{toks: toks, refs: map[string]bool{}}
	root, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, conditionErr("unexpected %q", p.toks[p.pos].kind)
	}
	refs := make([]string, 0, len(p.refs))
	for r := range p.refs {
		refs = append(refs, r)
	}
	slices.Sort(refs)
	return &Condition{root: root, refs: refs}, nil
}

// References returns the distinct references the condition reads, sorted.
func (c *Condition) References() []string { return slices.Clone(c.refs) }

func (p *parser) expr() (node, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.take("||") {
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = orNode{l, r}
	}
	return l, nil
}

func (p *parser) and() (node, error) {
	l, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.take("&&") {
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		l = andNode{l, r}
	}
	return l, nil
}

func (p *parser) enter() error {
	if p.depth++; p.depth > maxConditionDepth {
		return conditionErr("nesting is deeper than %d", maxConditionDepth)
	}
	return nil
}

func (p *parser) not() (node, error) {
	switch {
	case p.take("!"):
		if err := p.enter(); err != nil {
			return nil, err
		}
		x, err := p.not()
		p.depth--
		return notNode{x}, err
	case p.take("("):
		if err := p.enter(); err != nil {
			return nil, err
		}
		x, err := p.expr()
		p.depth--
		if err != nil {
			return nil, err
		}
		if !p.take(")") {
			return nil, conditionErr("missing )")
		}
		return x, nil
	}
	l, err := p.operand()
	if err != nil {
		return nil, err
	}
	switch k := p.peek(); k {
	case "==", "!=", "<", "<=", ">", ">=":
		p.pos++
		r, err := p.operand()
		if err != nil {
			return nil, err
		}
		if err := p.comparison(); err != nil {
			return nil, err
		}
		return cmpNode{op: k, l: l, r: r}, nil
	case "in":
		p.pos++
		if !p.take("[") {
			return nil, conditionErr("in needs a list")
		}
		var list []runtime.InputValue
		for {
			if p.peek() != "lit" {
				return nil, conditionErr("a list holds literals")
			}
			list = append(list, p.toks[p.pos].val)
			p.pos++
			if len(list) > maxConditionInList {
				return nil, conditionErr("an in list holds at most %d literals", maxConditionInList)
			}
			if p.take(",") {
				continue
			}
			break
		}
		if !p.take("]") {
			return nil, conditionErr("missing ]")
		}
		if err := p.comparison(); err != nil {
			return nil, err
		}
		return inNode{x: l, list: list}, nil
	}
	return bareNode{l}, nil
}

func (p *parser) comparison() error {
	if p.cmps++; p.cmps > maxConditionComparisons {
		return conditionErr("more than %d comparisons", maxConditionComparisons)
	}
	return nil
}

func (p *parser) operand() (operand, error) {
	switch p.peek() {
	case "lit":
		v := p.toks[p.pos].val
		p.pos++
		return operand{lit: &v}, nil
	case "ref":
		r, err := parseReference(p.toks[p.pos].text)
		if err != nil {
			return operand{}, err
		}
		p.pos++
		p.refs[r.text] = true
		return operand{ref: &r}, nil
	}
	return operand{}, conditionErr("an operand was expected")
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

// value is an operand's typed value; present is false for a missing one.
type value struct {
	v       runtime.InputValue
	present bool
}

// Evaluate evaluates the condition over a reader. It reads every distinct
// reference exactly once, before any comparison, so the values read, and their
// digest, do not depend on short-circuiting. It returns the result and what was
// read.
func (c *Condition) Evaluate(read ReferenceReader) (bool, []runtime.PredicateRead, error) {
	vals := map[string]value{}
	reads := make([]runtime.PredicateRead, 0, len(c.refs))
	for _, ref := range c.refs {
		v, ok, err := read(ref)
		if err != nil {
			return false, nil, err
		}
		vals[ref] = value{v, ok}
		pr := runtime.PredicateRead{Ref: ref, Type: v.Type, Present: ok}
		if ok {
			pr.Value = v.Text()
		}
		reads = append(reads, pr)
	}
	res, err := eval(c.root, vals)
	return res, reads, err
}

func (o operand) get(vals map[string]value) value {
	if o.lit != nil {
		return value{*o.lit, true}
	}
	return vals[o.ref.text]
}

func eval(n node, vals map[string]value) (bool, error) {
	switch x := n.(type) {
	case orNode:
		l, err := eval(x.l, vals)
		if err != nil {
			return false, err
		}
		r, err := eval(x.r, vals)
		return l || r, err
	case andNode:
		l, err := eval(x.l, vals)
		if err != nil {
			return false, err
		}
		r, err := eval(x.r, vals)
		return l && r, err
	case notNode:
		v, err := eval(x.x, vals)
		return !v, err
	case bareNode:
		v := x.x.get(vals)
		if v.v.Type != runtime.InputBoolean {
			return false, conditionErr("a bare operand must be boolean, not %s", v.v.Type)
		}
		return v.present && v.v.Boolean, nil
	case inNode:
		v := x.x.get(vals)
		for _, lit := range x.list {
			if lit.Type != v.v.Type {
				return false, conditionErr("in compares %s with %s", v.v.Type, lit.Type)
			}
		}
		if !v.present {
			return false, nil
		}
		return slices.ContainsFunc(x.list, func(l runtime.InputValue) bool { return l == v.v }), nil
	case cmpNode:
		l, r := x.l.get(vals), x.r.get(vals)
		if l.v.Type != r.v.Type {
			return false, conditionErr("%s compares %s with %s: there is no coercion", x.op, l.v.Type, r.v.Type)
		}
		ordered := x.op != "==" && x.op != "!="
		if ordered && l.v.Type != runtime.InputInteger {
			return false, conditionErr("%s applies to integers only", x.op)
		}
		if !l.present || !r.present {
			return false, nil
		}
		switch x.op {
		case "==":
			return l.v == r.v, nil
		case "!=":
			return l.v != r.v, nil
		case "<":
			return l.v.Integer < r.v.Integer, nil
		case "<=":
			return l.v.Integer <= r.v.Integer, nil
		case ">":
			return l.v.Integer > r.v.Integer, nil
		}
		return l.v.Integer >= r.v.Integer, nil
	}
	return false, conditionErr("unknown expression")
}
