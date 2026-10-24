package run

import (
	"fmt"
	"strings"
)

func runText(m map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s (%s)\n", field(m, "id"), field(m, "status"), field(m, "reason"))
	fmt.Fprintf(&b, "work item: %s  definition: %s\n", field(m, "workItemId"), field(m, "definitionId"))
	fmt.Fprintf(&b, "definition digest: %s\n", field(m, "definitionDigest"))
	fmt.Fprintf(&b, "content revision: %s  source commit: %s\n", field(m, "contentRevision"), field(m, "sourceCommit"))
	fmt.Fprintf(&b, "resource version: %s\n", field(m, "version"))
	fmt.Fprintf(&b, "created %s  started %s  finished %s\n", field(m, "createdAt"), field(m, "startedAt"), field(m, "finishedAt"))
	if v := field(m, "cancelRequestedAt"); v != "-" {
		fmt.Fprintf(&b, "cancellation requested: %s\n", v)
	}
	if v := field(m, "failureReason"); v != "-" {
		fmt.Fprintf(&b, "failure reason: %s\n", v)
	}
	return b.String()
}

func runListText(m map[string]any) string {
	var b strings.Builder
	for _, r := range items(m) {
		fmt.Fprintf(&b, "%s  %-9s work %s  v%s  %s\n", field(r, "id"), field(r, "status"), field(r, "workItemId"), field(r, "version"), field(r, "createdAt"))
	}
	if len(items(m)) == 0 {
		b.WriteString("no runs\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}

func stepsText(m map[string]any) string {
	var b strings.Builder
	for _, s := range items(m) {
		fmt.Fprintf(&b, "%s  %-9s %s (%s attempt(s) of at most %s)\n", field(s, "id"), field(s, "status"), field(s, "kind"), field(s, "attemptsStarted"), field(s, "maxAttempts"))
		attempts, _ := s["attempts"].([]any)
		for _, a := range attempts {
			am, _ := a.(map[string]any)
			fmt.Fprintf(&b, "    attempt %s #%s  %s (%s)  settled %s\n", field(am, "id"), field(am, "ordinal"), field(am, "status"), field(am, "reason"), field(am, "settled"))
		}
	}
	if len(items(m)) == 0 {
		b.WriteString("no steps\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}
