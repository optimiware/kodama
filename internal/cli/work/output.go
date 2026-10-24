package work

import (
	"fmt"
	"strings"
)

// itemText renders a work item. The resource version (the optimistic-lock
// value to pass as --if-version) and the content revision (what review and
// completion bind to) are separate numbers and are labelled as such.
func itemText(m map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s\n", field(m, "id"), field(m, "kind"), field(m, "state"))
	fmt.Fprintf(&b, "title: %s\n", field(m, "title"))
	fmt.Fprintf(&b, "resource version: %s (use with --if-version)\n", field(m, "version"))
	fmt.Fprintf(&b, "content revision: %s\n", field(m, "contentRevision"))
	fmt.Fprintf(&b, "assignee: %s  parent: %s\n", field(m, "assigneeId"), field(m, "parentId"))
	if v := field(m, "blockedReason"); v != "-" {
		fmt.Fprintf(&b, "blocked: %s\n", v)
	}
	if rs, ok := m["reviewSubject"].(map[string]any); ok {
		fmt.Fprintf(&b, "review subject: revision %s, candidate %s\n", field(rs, "contentRevision"), field(rs, "candidateCommit"))
	}
	if c, ok := m["completion"].(map[string]any); ok {
		fmt.Fprintf(&b, "completed: revision %s, candidate %s\n", field(c, "contentRevision"), field(c, "candidateCommit"))
	}
	return b.String()
}

func listText(m map[string]any) string {
	var b strings.Builder
	for _, it := range items(m) {
		fmt.Fprintf(&b, "%s  %-8s %-9s v%s r%s  %s\n", field(it, "id"), field(it, "kind"), field(it, "state"),
			field(it, "version"), field(it, "contentRevision"), field(it, "title"))
	}
	if len(items(m)) == 0 {
		b.WriteString("no work items\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}

func historyText(m map[string]any) string {
	var b strings.Builder
	for _, it := range items(m) {
		fmt.Fprintf(&b, "v%s r%s  %s  by %s/%s at %s", field(it, "version"), field(it, "contentRevision"),
			field(it, "kind"), field(actorOf(it), "type"), field(actorOf(it), "id"), field(it, "occurredAt"))
		if from, to := field(it, "from"), field(it, "to"); to != "-" {
			fmt.Fprintf(&b, "  %s -> %s", from, to)
		}
		if fs, ok := it["changedFields"].([]any); ok && len(fs) > 0 {
			names := make([]string, 0, len(fs))
			for _, f := range fs {
				names = append(names, fmt.Sprint(f))
			}
			fmt.Fprintf(&b, "  changed: %s", strings.Join(names, ","))
		}
		b.WriteString("\n")
	}
	if len(items(m)) == 0 {
		b.WriteString("no history entries\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}

func actorOf(m map[string]any) map[string]any {
	a, _ := m["actor"].(map[string]any)
	return a
}
