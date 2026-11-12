package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/optimiware/kodama/api"
)

// renderResult writes one operation result in the configured format. JSON writes
// the exact body bytes (or "{}" when absent) followed by a newline; table writes
// the operation line and one compact-JSON field per top-level body key in
// sorted order.
func renderResult(out io.Writer, format OutputFormat, result api.Result) error {
	if format == OutputTable {
		return renderTable(out, result)
	}
	if len(result.Body) == 0 {
		_, err := io.WriteString(out, "{}\n")
		return err
	}
	if _, err := out.Write(result.Body); err != nil {
		return err
	}
	_, err := io.WriteString(out, "\n")
	return err
}

// renderTable writes the deterministic table representation of one result.
func renderTable(out io.Writer, result api.Result) error {
	if _, err := fmt.Fprintf(out, "operation\t%s\n", result.Operation); err != nil {
		return err
	}
	if len(result.Body) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result.Body, &fields); err != nil {
		return fmt.Errorf("result body is not a JSON object: %w", err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var compact bytes.Buffer
		if err := json.Compact(&compact, fields[key]); err != nil {
			return fmt.Errorf("result field %q is not valid JSON: %w", key, err)
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\n", key, compact.String()); err != nil {
			return err
		}
	}
	return nil
}
