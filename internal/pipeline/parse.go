package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"go.yaml.in/yaml/v3"

	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	pipelinev1alpha3 "github.com/optimiware/kodama/contracts/pipeline/v1alpha3"
	"github.com/optimiware/kodama/internal/domain"
)

// Options bound the parser. They are library options with conservative local
// defaults, not installation settings.
type Options struct {
	// MaxBytes bounds the document size.
	MaxBytes int
	// MaxDepth bounds container nesting.
	MaxDepth int
	// MaxNodes bounds the number of values (scalars, sequences and mappings).
	MaxNodes int
}

// DefaultOptions returns the conservative parser bounds.
func DefaultOptions() Options {
	return Options{MaxBytes: 256 << 10, MaxDepth: 16, MaxNodes: 20000}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.MaxBytes <= 0 {
		o.MaxBytes = d.MaxBytes
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = d.MaxDepth
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = d.MaxNodes
	}
	return o
}

// ParseYAML parses exactly one YAML document into a structurally valid
// Document. It rejects duplicate keys, aliases, anchors, merge keys, explicit
// tags, null, floating point numbers, non-canonical integers and booleans,
// unknown fields and trailing documents. The result is not yet semantically
// validated; use a Validator.
func ParseYAML(data []byte, opts Options) (Document, error) {
	opts = opts.withDefaults()
	var d diagnostics
	if err := checkInput(data, opts, &d); err != nil {
		return Document{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			d.add(CodeInvalidDocument, "$", "the document is empty")
		} else {
			d.add(CodeInvalidDocument, yamlLine(err), "the document is not valid YAML")
		}
		return Document{}, d.err(StageSyntax)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		d.add(CodeMultiple, "$", "exactly one document is allowed")
		return Document{}, d.err(StageSyntax)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		d.add(CodeInvalidDocument, "$", "the document is empty")
		return Document{}, d.err(StageSyntax)
	}
	c := &converter{opts: opts, d: &d}
	tree := c.node(root.Content[0], "$", 1)
	if d.any() {
		return Document{}, d.err(StageSyntax)
	}
	return fromTree(tree)
}

var yamlLineRE = regexp.MustCompile(`line (\d+)`)

func yamlLine(err error) string {
	if m := yamlLineRE.FindStringSubmatch(err.Error()); m != nil {
		return "line " + m[1]
	}
	return "$"
}

func checkInput(data []byte, opts Options, d *diagnostics) error {
	if len(data) > opts.MaxBytes {
		d.add(CodeTooLarge, "$", "the document exceeds %d bytes", opts.MaxBytes)
		return d.err(StageSyntax)
	}
	if !utf8.Valid(data) {
		d.add(CodeInvalidDocument, "$", "the document is not valid UTF-8")
		return d.err(StageSyntax)
	}
	return nil
}

type converter struct {
	opts  Options
	d     *diagnostics
	nodes int
}

var yamlIntRE = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

// node converts a YAML node into a generic tree of string, bool, int64, []any
// and map[string]any, reporting everything that cannot be represented without
// loss. It keeps going after most findings so one run reports several.
func (c *converter) node(n *yaml.Node, path string, depth int) any {
	c.nodes++
	if c.nodes > c.opts.MaxNodes {
		if c.nodes == c.opts.MaxNodes+1 {
			c.d.add(CodeTooManyNodes, "$", "the document has more than %d values", c.opts.MaxNodes)
		}
		return nil
	}
	if depth > c.opts.MaxDepth {
		c.d.add(CodeTooDeep, path, "nesting is deeper than %d levels", c.opts.MaxDepth)
		return nil
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		c.d.add(CodeUnsupportedYAML, path, "aliases and anchors are not supported")
		return nil
	}
	if n.Style&yaml.TaggedStyle != 0 {
		c.d.add(CodeUnsupportedYAML, path, "explicit tags are not supported")
		return nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" || k.Style&yaml.TaggedStyle != 0 || k.Anchor != "" {
				if k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge" {
					c.d.add(CodeUnsupportedYAML, path, "merge keys are not supported")
				} else {
					c.d.add(CodeInvalidType, path, "object keys must be strings")
				}
				continue
			}
			child := childPath(path, k.Value)
			if _, dup := out[k.Value]; dup {
				c.d.add(CodeDuplicateKey, child, "the key appears more than once")
				continue
			}
			out[k.Value] = c.node(v, child, depth+1)
		}
		return out
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, e := range n.Content {
			out = append(out, c.node(e, path+"["+strconv.Itoa(i)+"]", depth+1))
		}
		return out
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str":
			return n.Value
		case "!!bool":
			if n.Value == "true" {
				return true
			}
			if n.Value == "false" {
				return false
			}
			c.d.add(CodeInvalidFormat, path, "booleans must be written true or false")
		case "!!int":
			if !yamlIntRE.MatchString(n.Value) {
				c.d.add(CodeInvalidFormat, path, "integers must be plain decimal numbers")
				return nil
			}
			v, err := strconv.ParseInt(n.Value, 10, 64)
			if err != nil || v < -domain.MaxSafeInteger || v > domain.MaxSafeInteger {
				c.d.add(CodeInvalidValue, path, "integers must be within +/-%d", int64(domain.MaxSafeInteger))
				return nil
			}
			return v
		case "!!float":
			c.d.add(CodeInvalidType, path, "numbers must be integers")
		case "!!null":
			c.d.add(CodeInvalidType, path, "null is not a valid value; omit the field")
		default:
			c.d.add(CodeInvalidType, path, "the scalar is not a string, boolean or integer; quote it to make it a string")
		}
		return nil
	}
	c.d.add(CodeInvalidDocument, path, "unsupported YAML construct")
	return nil
}

var plainKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// childPath appends a map key to a path, escaping and bounding unusual keys so
// a hostile document cannot inflate or forge a diagnostic path.
func childPath(path, key string) string {
	if plainKeyRE.MatchString(key) {
		return path + "." + key
	}
	if len(key) > 64 {
		key = key[:64]
		for !utf8.ValidString(key) {
			key = key[:len(key)-1]
		}
	}
	return path + "[" + strconv.Quote(key) + "]"
}

// ParseJSON parses exactly one JSON document with the same strictness as
// ParseYAML: duplicate keys, unknown fields, fractional or exponent numbers,
// lone surrogates, control characters and trailing data are rejected, and
// integers keep their precision.
func ParseJSON(data []byte, opts Options) (Document, error) {
	opts = opts.withDefaults()
	var d diagnostics
	if err := checkInput(data, opts, &d); err != nil {
		return Document{}, err
	}
	p := &jsonParser{b: data, opts: opts, d: &d}
	p.ws()
	if p.i >= len(p.b) {
		d.add(CodeInvalidDocument, "$", "the document is empty")
		return Document{}, d.err(StageSyntax)
	}
	tree, ok := p.value("$", 1)
	if ok {
		p.ws()
		if p.i != len(p.b) {
			ok = false
			d.add(CodeMultiple, "$", "exactly one document is allowed")
		}
	}
	if !ok || d.any() {
		if !d.any() {
			d.add(CodeInvalidDocument, "$", "the document is not valid JSON")
		}
		return Document{}, d.err(StageSyntax)
	}
	return fromTree(tree)
}

type jsonParser struct {
	b     []byte
	i     int
	opts  Options
	d     *diagnostics
	nodes int
}

func (p *jsonParser) fail(path, format string, args ...any) (any, bool) {
	p.d.add(CodeInvalidDocument, path, format, args...)
	return nil, false
}

func (p *jsonParser) ws() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\t' || p.b[p.i] == '\n' || p.b[p.i] == '\r') {
		p.i++
	}
}

func (p *jsonParser) value(path string, depth int) (any, bool) {
	p.nodes++
	if p.nodes > p.opts.MaxNodes {
		p.d.add(CodeTooManyNodes, "$", "the document has more than %d values", p.opts.MaxNodes)
		return nil, false
	}
	if depth > p.opts.MaxDepth {
		p.d.add(CodeTooDeep, path, "nesting is deeper than %d levels", p.opts.MaxDepth)
		return nil, false
	}
	p.ws()
	if p.i >= len(p.b) {
		return p.fail(path, "unexpected end of the document")
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object(path, depth)
	case c == '[':
		return p.array(path, depth)
	case c == '"':
		s, ok := p.str(path)
		return s, ok
	case c == 't' && bytes.HasPrefix(p.b[p.i:], []byte("true")):
		p.i += 4
		return true, true
	case c == 'f' && bytes.HasPrefix(p.b[p.i:], []byte("false")):
		p.i += 5
		return false, true
	case c == 'n' && bytes.HasPrefix(p.b[p.i:], []byte("null")):
		p.d.add(CodeInvalidType, path, "null is not a valid value; omit the field")
		p.i += 4
		return nil, false
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number(path)
	}
	return p.fail(path, "unexpected character")
}

func (p *jsonParser) object(path string, depth int) (any, bool) {
	p.i++ // {
	out := map[string]any{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return out, true
	}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return p.fail(path, "an object key must be a string")
		}
		key, ok := p.str(path)
		if !ok {
			return nil, false
		}
		child := childPath(path, key.(string))
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return p.fail(child, "expected ':' after the key")
		}
		p.i++
		if _, dup := out[key.(string)]; dup {
			p.d.add(CodeDuplicateKey, child, "the key appears more than once")
			return nil, false
		}
		v, ok := p.value(child, depth+1)
		if !ok {
			return nil, false
		}
		out[key.(string)] = v
		p.ws()
		if p.i >= len(p.b) {
			return p.fail(path, "unexpected end of the document")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return out, true
		default:
			return p.fail(path, "expected ',' or '}'")
		}
	}
}

func (p *jsonParser) array(path string, depth int) (any, bool) {
	p.i++ // [
	out := []any{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return out, true
	}
	for {
		v, ok := p.value(path+"["+strconv.Itoa(len(out))+"]", depth+1)
		if !ok {
			return nil, false
		}
		out = append(out, v)
		p.ws()
		if p.i >= len(p.b) {
			return p.fail(path, "unexpected end of the document")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, true
		default:
			return p.fail(path, "expected ',' or ']'")
		}
	}
}

func (p *jsonParser) str(path string) (any, bool) {
	p.i++ // opening quote
	var sb strings.Builder
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return sb.String(), true
		case c < 0x20:
			return p.fail(path, "control characters in strings must be escaped")
		case c == '\\':
			p.i++
			if p.i >= len(p.b) {
				return p.fail(path, "unterminated escape")
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"', '\\', '/':
				sb.WriteByte(e)
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				r, ok := p.hex4()
				if !ok {
					return p.fail(path, "invalid unicode escape")
				}
				if r >= 0xD800 && r < 0xDC00 { // high surrogate needs its pair
					if p.i+1 < len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
						p.i += 2
						lo, ok := p.hex4()
						if !ok || lo < 0xDC00 || lo > 0xDFFF {
							return p.fail(path, "invalid surrogate pair")
						}
						r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
					} else {
						return p.fail(path, "invalid surrogate pair")
					}
				} else if r >= 0xDC00 && r <= 0xDFFF {
					return p.fail(path, "invalid surrogate pair")
				}
				sb.WriteRune(rune(r))
			default:
				return p.fail(path, "invalid escape")
			}
		default:
			sb.WriteByte(c)
			p.i++
		}
	}
	return p.fail(path, "unterminated string")
}

func (p *jsonParser) hex4() (int, bool) {
	if p.i+4 > len(p.b) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	p.i += 4
	return int(v), true
}

var jsonIntRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func (p *jsonParser) number(path string) (any, bool) {
	start := p.i
	for p.i < len(p.b) && strings.IndexByte("+-0123456789.eE", p.b[p.i]) >= 0 {
		p.i++
	}
	lit := string(p.b[start:p.i])
	if !jsonIntRE.MatchString(lit) {
		if strings.ContainsAny(lit, ".eE") {
			p.d.add(CodeInvalidType, path, "numbers must be integers")
		} else {
			p.d.add(CodeInvalidFormat, path, "integers must be plain decimal numbers")
		}
		return nil, false
	}
	v, err := strconv.ParseInt(lit, 10, 64)
	if err != nil || v < -domain.MaxSafeInteger || v > domain.MaxSafeInteger || lit == "-0" {
		p.d.add(CodeInvalidValue, path, "integers must be within +/-%d", int64(domain.MaxSafeInteger))
		return nil, false
	}
	return v, true
}

// fromTree validates a generic tree against the contract schema and decodes it
// into a typed Document.
func fromTree(tree any) (Document, error) {
	// The schema is selected by the document's own apiVersion. Reading a
	// shared-format document is independent of whether an installation enabled
	// the format: stored definitions stay readable, and enabling is a
	// registration decision of the Validator.
	v2 := isV1Alpha2Tree(tree)
	v3 := isV1Alpha3Tree(tree)
	schema := pipelinev1.Schema
	if v2 {
		schema = pipelinev1alpha2.Schema
	} else if v3 {
		schema = pipelinev1alpha3.Schema
	}
	sch, err := schema()
	if err != nil {
		return Document{}, fmt.Errorf("pipeline: %w", err)
	}
	// The schema validator reads json.Number, never int64 or float64, so the
	// tree is bridged through JSON text, which preserves integer precision.
	raw, err := json.Marshal(tree)
	if err != nil {
		return Document{}, fmt.Errorf("pipeline: encode tree: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return Document{}, fmt.Errorf("pipeline: schema instance: %w", err)
	}
	if verr := sch.Validate(inst); verr != nil {
		var ve *jsonschema.ValidationError
		if errors.As(verr, &ve) {
			var d diagnostics
			mapSchemaError(&d, ve, tree)
			if d.any() {
				return Document{}, d.err(StageSyntax)
			}
		}
		var d diagnostics
		d.add(CodeInvalidDocument, "$", "the document does not match the pipeline schema")
		return Document{}, d.err(StageSyntax)
	}
	doc, err := decodeDocument(raw)
	if err != nil {
		var d diagnostics
		d.add(CodeInvalidDocument, "$", "the document does not match the pipeline schema")
		return Document{}, d.err(StageSyntax)
	}
	if v2 || v3 {
		// Cross-structure rules the schema cannot state (port and edge
		// references, overlaps, probe ranges) are still part of the document's
		// shape and fail at the syntax stage.
		var d diagnostics
		checkShapeV2(normalize(doc), &d)
		if d.any() {
			return Document{}, d.err(StageSyntax)
		}
	}
	return doc, nil
}

// gateForbidden are the executable-only fields a gate must not carry.
var gateForbidden = map[string]bool{
	"when": true, "workspace": true, "environment": true, "image": true, "command": true, "args": true,
	"env": true, "secretFiles": true, "timeoutSeconds": true, "retry": true, "outputs": true, "agent": true,
}

// mapSchemaError turns the schema's error tree into bounded diagnostics with
// stable codes. Messages are fixed text; the offending values are never echoed.
func mapSchemaError(d *diagnostics, ve *jsonschema.ValidationError, tree any) {
	type finding struct{ code, path, msg string }
	v2 := isV1Alpha2Tree(tree) || isV1Alpha3Tree(tree)
	var found []finding
	seen := map[string]bool{}
	add := func(code, path, msg string) {
		key := code + "\x00" + path
		if !seen[key] {
			seen[key] = true
			found = append(found, finding{code, path, msg})
		}
	}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		path := schemaPath(tree, e.InstanceLocation)
		kw := ""
		if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
			kw = kp[len(kp)-1]
		}
		switch k := e.ErrorKind.(type) {
		case *kind.AdditionalProperties:
			parent := lookup(tree, e.InstanceLocation)
			for _, prop := range k.Properties {
				code, msg := CodeUnknownField, "the field is not part of the pipeline format"
				if m, ok := parent.(map[string]any); ok && m["kind"] == "gate" && (gateForbidden[prop] || (v2 && prop == "egress")) {
					code, msg = CodeGateField, "gates have no executable fields"
					if prop == "when" {
						code, msg = CodeConditionalGate, "gates are unconditional and cannot have a when condition"
					}
				}
				if v2 && reNameKeyed.MatchString(path) {
					// The kodama/v1alpha2 schema states key syntax with
					// patternProperties, so a badly spelled name surfaces as an
					// additional property of its map.
					code, msg = CodeInvalidFormat, "the name does not have the required format"
				}
				if v2 && prop == "from" && strings.HasSuffix(path, ".workspace") {
					code, msg = CodeWorkspaceFromRemoved, "workspace.from is not part of this pipeline format"
				}
				add(code, childPath(path, prop), msg)
			}
			return
		case *kind.Required:
			for _, prop := range k.Missing {
				add(CodeMissingField, childPath(path, prop), "the field is required")
			}
			return
		}
		if _, isNot := e.ErrorKind.(*kind.Not); isNot {
			switch {
			case strings.HasSuffix(path, ".workspace"):
				add(CodeInvalidSource, path, "workspace access none cannot name a source")
			case strings.Contains(path, ".outputs"):
				add(CodeInvalidOutput, path, "only artifact outputs have a path")
			default:
				add(CodeInvalidValue, path, "the value is not allowed here")
			}
			return
		}
		if kw == "anyOf" || kw == "oneOf" {
			if code, ok := v2SchemaCode(v2, path, kw); ok {
				add(code, path, v2Message(code))
				return
			}
			add(CodeInvalidType, path, "the value has an unsupported type")
			return
		}
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		if code, ok := v2SchemaCode(v2, path, kw); ok {
			add(code, path, v2Message(code))
			return
		}
		switch kw {
		case "type":
			add(CodeInvalidType, path, "the value has the wrong type")
		case "enum", "const":
			add(CodeInvalidValue, path, "the value is not one of the allowed values")
		case "pattern":
			if strings.HasSuffix(path, ".image") {
				add(CodeMutableImage, path, "images must be pinned by sha256 digest")
			} else {
				add(CodeInvalidFormat, path, "the value does not have the required format")
			}
		case "minItems", "maxItems", "minimum", "maximum", "maxLength", "minLength", "uniqueItems":
			add(CodeInvalidValue, path, "the value is outside its allowed range")
		default:
			add(CodeInvalidValue, path, "the value is not valid here")
		}
	}
	walk(ve)
	sortFindings := func() {
		for i := 1; i < len(found); i++ {
			for j := i; j > 0 && (found[j].path < found[j-1].path || (found[j].path == found[j-1].path && found[j].code < found[j-1].code)); j-- {
				found[j], found[j-1] = found[j-1], found[j]
			}
		}
	}
	sortFindings()
	for _, f := range found {
		d.add(f.code, f.path, "%s", f.msg)
	}
}

// lookup walks tree along schema instance-location segments.
func lookup(tree any, loc []string) any {
	cur := tree
	for _, seg := range loc {
		switch v := cur.(type) {
		case map[string]any:
			cur = v[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(v) {
				return nil
			}
			cur = v[i]
		default:
			return nil
		}
	}
	return cur
}

// schemaPath renders an instance location, using the tree to tell array
// indexes from object keys.
func schemaPath(tree any, loc []string) string {
	path := "$"
	cur := tree
	for _, seg := range loc {
		switch v := cur.(type) {
		case []any:
			path += "[" + seg + "]"
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(v) {
				cur = nil
			} else {
				cur = v[i]
			}
		case map[string]any:
			path = childPath(path, seg)
			cur = v[seg]
		default:
			path = childPath(path, seg)
			cur = nil
		}
	}
	return path
}
