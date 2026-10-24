package agent

import (
	"fmt"
	"strings"
)

// profileText renders one immutable profile version: the reference a pipeline
// agent step pins, its version and digest, and the pinned defaults a run will
// inherit.
func profileText(m map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", field(m, "reference"), field(m, "id"))
	fmt.Fprintf(&b, "version: %s  digest: %s\n", field(m, "version"), field(m, "digest"))
	p, _ := m["profile"].(map[string]any)
	if p != nil {
		fmt.Fprintf(&b, "image: %s\n", field(p, "image"))
		if w, ok := p["wrapper"].(map[string]any); ok {
			h, _ := w["harness"].(map[string]any)
			fmt.Fprintf(&b, "wrapper: %s %s  harness: %s %s\n", field(w, "id"), field(w, "version"), field(h, "id"), field(h, "version"))
		}
		if mo, ok := p["model"].(map[string]any); ok {
			fmt.Fprintf(&b, "model: %s  endpoint: %s\n", field(mo, "name"), field(mo, "endpoint"))
		}
		fmt.Fprintf(&b, "tools: %s\n", field(p, "tools"))
		fmt.Fprintf(&b, "limits: %s\n", field(p, "limits"))
		if d, ok := p["designBaseline"].(map[string]any); ok {
			fmt.Fprintf(&b, "design baseline: %s\n", field(d, "digest"))
		}
	}
	if by, ok := m["createdBy"].(map[string]any); ok {
		fmt.Fprintf(&b, "created by: %s/%s at %s\n", field(by, "type"), field(by, "id"), field(m, "createdAt"))
	}
	return b.String()
}

// registeredText states whether registration created a version (201) or
// converged on an identical existing one (200).
func registeredText(m map[string]any, status int) string {
	head := "registered a new profile version"
	if status == 200 {
		head = "an identical profile was already registered; nothing was created"
	}
	return head + "\n" + profileText(m)
}

func listText(m map[string]any) string {
	var b strings.Builder
	for _, it := range items(m) {
		fmt.Fprintf(&b, "%s  v%s  %s  %s\n", field(it, "reference"), field(it, "version"), field(it, "digest"), field(it, "id"))
	}
	if len(items(m)) == 0 {
		b.WriteString("no agent profiles\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}
