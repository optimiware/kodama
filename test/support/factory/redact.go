package factory

// Diagnostic redaction shared by every lifecycle failure dump.
//
// Diagnostic output is untrusted: a Kubernetes Secret rendered as YAML carries
// its base64 payload, a workload log may carry a mounted credential, and a
// failure path that prints either would leak a live bearer token into the test
// log. Every lifecycle dump therefore passes through SanitizeDiagnostic before
// it is logged, and a kubectl dump never asks a Secret kind for its payload at
// all (DiagnosticKubectlGetArgs).
//
// The rules are deliberately redundant:
//
//   - a Kubernetes Secret document, or a Secret item inside a List, never keeps
//     its data or stringData payload;
//   - every value the caller names as sensitive is replaced together with its
//     standard and URL-safe base64 encodings, so neither the raw token nor a
//     rendered Secret payload survives;
//   - a document with no Secret payload is returned byte-for-byte, so ordinary
//     diagnostics are not reformatted.

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DiagnosticRedacted is the fixed marker a diagnostic substitutes for sensitive
// material. It matches the product's own log redaction marker.
const DiagnosticRedacted = "[REDACTED]"

// SanitizeDiagnostic removes Kubernetes Secret payloads and every supplied
// sensitive value from diagnostic text. The text may be YAML, JSON or plain log
// output; a parse failure falls back to dropping data/stringData blocks by
// indentation, which over-redacts rather than leaking.
func SanitizeDiagnostic(text string, sensitive ...string) string {
	sanitized := scrubSecretDocuments(text)
	for _, value := range sensitive {
		if value == "" {
			continue
		}
		sanitized = strings.ReplaceAll(sanitized, value, DiagnosticRedacted)
		for _, encoding := range []*base64.Encoding{
			base64.StdEncoding,
			base64.RawStdEncoding,
			base64.URLEncoding,
			base64.RawURLEncoding,
		} {
			encoded := encoding.EncodeToString([]byte(value))
			if encoded == "" {
				continue
			}
			sanitized = strings.ReplaceAll(sanitized, encoded, DiagnosticRedacted)
		}
	}
	return sanitized
}

// DiagnosticKubectlGetArgs renders the complete kubectl argument list for one
// diagnostic get. A Secret resource is rendered as a name list: a diagnostic
// must never request data or stringData, because kubectl would print the live
// credential. Every other resource is rendered as YAML unless the caller
// already chose an output.
func DiagnosticKubectlGetArgs(args ...string) []string {
	full := append([]string{"get"}, args...)
	if len(args) > 0 && isSecretResource(args[0]) {
		return append(full, "-o", "name")
	}
	for _, argument := range args {
		if argument == "-o" || argument == "--output" ||
			strings.HasPrefix(argument, "-o=") || strings.HasPrefix(argument, "--output=") {
			return full
		}
	}
	return append(full, "-o", "yaml")
}

// isSecretResource reports whether one kubectl resource token names a Secret.
func isSecretResource(resource string) bool {
	switch strings.ToLower(strings.TrimSpace(resource)) {
	case "secret", "secrets":
		return true
	default:
		return false
	}
}

// scrubSecretDocuments decodes every YAML/JSON document, removes Secret
// payloads and re-encodes only when something changed.
func scrubSecretDocuments(text string) string {
	decoder := yaml.NewDecoder(strings.NewReader(text))
	var documents []any
	changed := false
	for {
		var document any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return scrubSecretPayloadLines(text)
		}
		if scrubSecretPayloads(document) {
			changed = true
		}
		documents = append(documents, document)
	}
	if !changed {
		return text
	}
	var builder strings.Builder
	for _, document := range documents {
		encoded, err := yaml.Marshal(document)
		if err != nil {
			return scrubSecretPayloadLines(text)
		}
		builder.Write(encoded)
	}
	return builder.String()
}

// scrubSecretPayloads recursively deletes data and stringData from every Secret
// object. It reports whether anything was removed.
func scrubSecretPayloads(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		if kind, _ := typed["kind"].(string); strings.EqualFold(kind, "Secret") {
			for _, key := range []string{"data", "stringData"} {
				if payload, present := typed[key]; present && payload != nil {
					delete(typed, key)
					changed = true
				}
			}
		}
		for _, child := range typed {
			if scrubSecretPayloads(child) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range typed {
			if scrubSecretPayloads(child) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

// scrubSecretPayloadLines is the fallback for text that is not valid YAML/JSON.
// It drops a data/stringData line and every more-indented line below it.
func scrubSecretPayloadLines(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	skippingIndent := -1
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		if skippingIndent >= 0 {
			if trimmed == "" || indent > skippingIndent {
				continue
			}
			skippingIndent = -1
		}
		if strings.HasPrefix(trimmed, "data:") || strings.HasPrefix(trimmed, "stringData:") {
			skippingIndent = indent
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
