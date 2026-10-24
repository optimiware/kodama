package root

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Format selects result rendering.
type Format string

const (
	FormatHuman Format = "human"
	FormatJSON  Format = "json"
)

// ParseFormat validates an --output value.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatHuman, FormatJSON:
		return Format(s), nil
	}
	return "", Usagef("--output must be human or json")
}

// Streams are the process streams, injected so commands never touch os.Std*.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Result is a successful command result. Data is the JSON document (nil is
// rendered as an empty object); Text is the human rendering.
type Result struct {
	Data any
	Text string
}

// EncodeJSON renders v as one compact, newline-terminated document. The output
// is deterministic: maps are key-sorted, structs use field order, and HTML
// characters are not escaped. It is fully buffered so failure emits nothing.
func EncodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// NormalizeJSON decodes a server JSON document so it re-encodes with sorted
// keys; numbers keep their exact spelling. Unknown (additive) fields survive.
func NormalizeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON document")
	}
	return v, nil
}

// WriteSuccess writes the result to stdout only.
func WriteSuccess(s Streams, f Format, r Result) error {
	if f == FormatJSON {
		data := r.Data
		if data == nil {
			data = map[string]any{}
		}
		b, err := EncodeJSON(data)
		if err != nil {
			return err
		}
		_, err = s.Out.Write(b)
		return err
	}
	text := r.Text
	if text == "" {
		return nil
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, err := io.WriteString(s.Out, text)
	return err
}

type errorBody struct {
	Code              string         `json:"code"`
	Message           string         `json:"message"`
	RequestID         string         `json:"requestId"`
	Details           map[string]any `json:"details"`
	Status            int            `json:"status,omitempty"`
	RetryAfterSeconds int64          `json:"retryAfterSeconds,omitempty"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// WriteFailure writes the failure to stderr only: one JSON envelope in JSON
// mode, one plain line otherwise.
func WriteFailure(s Streams, f Format, p Problem) {
	if f == FormatJSON {
		details := p.Details
		if details == nil {
			details = map[string]any{}
		}
		b, err := EncodeJSON(errorEnvelope{errorBody{
			Code: p.Code, Message: p.Message, RequestID: p.RequestID, Details: details,
			Status: p.Status, RetryAfterSeconds: p.RetryAfterSeconds,
		}})
		if err != nil { // details held an unencodable value: send a safe fallback.
			b, _ = EncodeJSON(errorEnvelope{errorBody{
				Code: p.Code, Message: p.Message, RequestID: p.RequestID, Details: map[string]any{},
			}})
		}
		_, _ = s.Err.Write(b)
		return
	}
	line := "error: " + p.Message
	var extra []string
	if p.Code != "" {
		extra = append(extra, "code "+p.Code)
	}
	if p.RequestID != "" {
		extra = append(extra, "request "+p.RequestID)
	}
	if p.RetryAfterSeconds > 0 {
		extra = append(extra, fmt.Sprintf("retry after %ds", p.RetryAfterSeconds))
	}
	if len(extra) > 0 {
		line += " (" + strings.Join(extra, ", ") + ")"
	}
	line += "\n"
	if p.OutcomeUncertain {
		line += "outcome unknown"
		if p.Recovery != "" {
			line += ": " + p.Recovery
		}
		line += "\n"
	}
	_, _ = io.WriteString(s.Err, line)
}
