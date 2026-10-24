package runtimev1

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// maxDepth bounds nesting so hostile payloads cannot exhaust the stack.
const maxDepth = 32

// parseStrict parses data into a generic tree and rejects everything the
// runtime contract forbids: duplicate keys, non-integer numbers, integers
// outside +/-MaxSafeInteger, "-0", invalid UTF-8, unpaired surrogate escapes,
// control characters in strings, excessive nesting and trailing data.
// Objects become map[string]any, arrays []any, numbers int64.
func parseStrict(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("invalid UTF-8")
	}
	p := &parser{b: data}
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, p.fail("trailing data")
	}
	return v, nil
}

type parser struct {
	b     []byte
	i     int
	depth int
}

func (p *parser) fail(msg string) error { return fmt.Errorf("json offset %d: %s", p.i, msg) }

func (p *parser) ws() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\t' || p.b[p.i] == '\n' || p.b[p.i] == '\r') {
		p.i++
	}
}

func (p *parser) value() (any, error) {
	p.ws()
	if p.i >= len(p.b) {
		return nil, p.fail("unexpected end of input")
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == 't':
		return p.literal("true", true)
	case c == 'f':
		return p.literal("false", false)
	case c == 'n':
		return p.literal("null", nil)
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, p.fail("unexpected character")
}

func (p *parser) literal(word string, v any) (any, error) {
	if !bytes.HasPrefix(p.b[p.i:], []byte(word)) {
		return nil, p.fail("invalid literal")
	}
	p.i += len(word)
	return v, nil
}

func (p *parser) number() (any, error) {
	start := p.i
	if p.b[p.i] == '-' {
		p.i++
	}
	digits := p.i
	for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
		p.i++
	}
	if p.i == digits || (p.b[digits] == '0' && p.i-digits > 1) {
		return nil, p.fail("invalid number")
	}
	if p.i < len(p.b) && (p.b[p.i] == '.' || p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		return nil, p.fail("numbers must be integers")
	}
	text := string(p.b[start:p.i])
	if text == "-0" {
		return nil, p.fail("negative zero is not allowed")
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n > domain.MaxSafeInteger || n < -domain.MaxSafeInteger {
		return nil, p.fail("integer outside +/-(2^53-1)")
	}
	return n, nil
}

func (p *parser) str() (string, error) {
	p.i++ // opening quote
	var out []byte
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return string(out), nil
		case c < 0x20:
			return "", p.fail("control character in string")
		case c != '\\':
			out = append(out, c)
			p.i++
			continue
		}
		p.i++
		if p.i >= len(p.b) {
			break
		}
		esc := p.b[p.i]
		p.i++
		switch esc {
		case '"', '\\', '/':
			out = append(out, esc)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, err := p.unicode()
			if err != nil {
				return "", err
			}
			out = utf8.AppendRune(out, r)
		default:
			return "", p.fail("invalid escape")
		}
	}
	return "", p.fail("unterminated string")
}

func (p *parser) hex4() (rune, error) {
	if p.i+4 > len(p.b) {
		return 0, p.fail("short unicode escape")
	}
	n, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, p.fail("invalid unicode escape")
	}
	p.i += 4
	return rune(n), nil
}

func (p *parser) unicode() (rune, error) {
	r, err := p.hex4()
	if err != nil {
		return 0, err
	}
	switch {
	case utf16.IsSurrogate(r) && r >= 0xDC00:
		return 0, p.fail("unpaired surrogate")
	case utf16.IsSurrogate(r):
		if !bytes.HasPrefix(p.b[p.i:], []byte(`\u`)) {
			return 0, p.fail("unpaired surrogate")
		}
		p.i += 2
		low, err := p.hex4()
		if err != nil {
			return 0, err
		}
		if low < 0xDC00 || low > 0xDFFF {
			return 0, p.fail("unpaired surrogate")
		}
		return utf16.DecodeRune(r, low), nil
	}
	return r, nil
}

func (p *parser) enter() error {
	p.depth++
	if p.depth > maxDepth {
		return p.fail("nesting too deep")
	}
	return nil
}

func (p *parser) object() (any, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	p.i++
	obj := map[string]any{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return obj, nil
	}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return nil, p.fail("object key expected")
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		if _, dup := obj[key]; dup {
			return nil, p.fail("duplicate key " + strconv.Quote(key))
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, p.fail("':' expected")
		}
		p.i++
		if obj[key], err = p.value(); err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.b) {
			return nil, p.fail("unterminated object")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return obj, nil
		default:
			return nil, p.fail("',' or '}' expected")
		}
	}
}

func (p *parser) array() (any, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	p.i++
	arr := []any{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return arr, nil
	}
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.ws()
		if p.i >= len(p.b) {
			return nil, p.fail("unterminated array")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return arr, nil
		default:
			return nil, p.fail("',' or ']' expected")
		}
	}
}

// Canonicalize validates data with the strict parser and returns its canonical
// form: UTF-8 JSON without insignificant whitespace, object keys sorted by
// code point (equivalently, by UTF-8 bytes), array order preserved, integers
// in plain base-10, and strings escaped as specified by writeString. The
// canonical form is the only input to result hashing.
func Canonicalize(data []byte) ([]byte, error) {
	tree, err := parseStrict(data)
	if err != nil {
		return nil, err
	}
	return encodeCanonical(tree), nil
}

func encodeCanonical(tree any) []byte {
	var buf bytes.Buffer
	writeCanonical(&buf, tree)
	return buf.Bytes()
}

func writeCanonical(buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case string:
		writeString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, e)
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // Go compares strings bytewise: UTF-8 order == code point order
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			writeCanonical(buf, x[k])
		}
		buf.WriteByte('}')
	default:
		panic(fmt.Sprintf("canonical: unsupported %T", v)) // unreachable: parseStrict emits only the types above
	}
}

// writeString escapes only what JSON requires: '"' and '\\', the short forms
// \b \f \n \r \t, and every other control character below U+0020 as a
// lowercase \u00xx escape. All other characters, including '/', '<', '>',
// '&', U+007F, U+2028 and non-ASCII text, are written as literal UTF-8.
// No Unicode normalization is applied.
func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, c)
			} else {
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
}
