package pgtest

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// UnitStateMatrixBegin and UnitStateMatrixEnd delimit the generated matrix
// section in the support README.
const (
	UnitStateMatrixBegin = "<!-- unit-states:begin (generated from DerivedUnitStates; do not edit) -->"
	UnitStateMatrixEnd   = "<!-- unit-states:end -->"
)

// RenderUnitStateMatrix renders the deterministic README table for the observed
// unit×state registry.
func RenderUnitStateMatrix(states map[string][]string) string {
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	out.WriteString("| unit×state | changed tables |\n|---|---|\n")
	for _, key := range keys {
		tables := append([]string(nil), states[key]...)
		sort.Strings(tables)
		rendered := "none"
		if len(tables) > 0 {
			rendered = strings.Join(tables, ", ")
		}
		out.WriteString("| `" + key + "` | " + rendered + " |\n")
	}
	return out.String()
}

// UnitStateMatrixError compares the generated section in README with the
// observed registry, so the documented matrix cannot drift from what the suite
// actually proved.
func UnitStateMatrixError(path string) error {
	return unitStateMatrixError(path, DerivedUnitStates())
}

func unitStateMatrixError(path string, states map[string][]string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("unit×state matrix: read %s: %w", path, err)
	}
	text := string(body)
	start := strings.Index(text, UnitStateMatrixBegin)
	if start < 0 {
		return fmt.Errorf("unit×state matrix: %s has no begin marker", path)
	}
	start += len(UnitStateMatrixBegin)
	end := strings.Index(text[start:], UnitStateMatrixEnd)
	if end < 0 {
		return fmt.Errorf("unit×state matrix: %s has no end marker", path)
	}
	documented := strings.TrimSpace(text[start : start+end])
	generated := strings.TrimSpace(RenderUnitStateMatrix(states))
	if documented != generated {
		return fmt.Errorf("unit×state matrix: README section does not match the observed registry\n--- documented ---\n%s\n--- observed ---\n%s", documented, generated)
	}
	return nil
}

// WriteUnitStateMatrix replaces the generated README section with the observed
// registry. It is used only by the explicit matrix-update run.
func WriteUnitStateMatrix(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(body)
	start := strings.Index(text, UnitStateMatrixBegin)
	if start < 0 {
		return fmt.Errorf("unit×state matrix: %s has no begin marker", path)
	}
	start += len(UnitStateMatrixBegin)
	end := strings.Index(text[start:], UnitStateMatrixEnd)
	if end < 0 {
		return fmt.Errorf("unit×state matrix: %s has no end marker", path)
	}
	updated := text[:start] + "\n" + RenderUnitStateMatrix(DerivedUnitStates()) + text[start+end:]
	return os.WriteFile(path, []byte(updated), 0o644)
}
