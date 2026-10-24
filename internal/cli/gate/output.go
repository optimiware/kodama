package gate

import (
	"fmt"
	"strings"
)

// gateText renders everything a reviewer needs before deciding: the version
// to pass as --if-version, the digest to pass as --subject-digest, the whole
// subject, each evidence ID/digest pair, the required evidence and conformance
// snapshot, and the recorded decision. Applicability is the server's
// observation from the gate record; it is not proof that current facts permit
// approval, which the server rechecks when a decision is recorded.
func gateText(m map[string]any, replayed bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  run %s step %s\n", field(m, "id"), field(m, "status"), field(m, "runId"), field(m, "stepId"))
	fmt.Fprintf(&b, "version: %s (use with --if-version)\n", field(m, "version"))
	fmt.Fprintf(&b, "subject digest: %s (use with --subject-digest)\n", field(m, "subjectDigest"))
	if s, ok := m["subject"].(map[string]any); ok {
		fmt.Fprintf(&b, "subject: work item %s, content revision %s, candidate %s, definition %s\n",
			field(s, "workItemId"), field(s, "contentRevision"), field(s, "candidateCommit"), field(s, "definitionId"))
		fmt.Fprintf(&b, "  tenant %s project %s\n", field(s, "tenantId"), field(s, "projectId"))
		fmt.Fprintf(&b, "  deployment: %s config digest %s\n", field(s, "deploymentId"), field(s, "deploymentConfigDigest"))
		ev, _ := s["evidence"].([]any)
		if len(ev) == 0 {
			b.WriteString("  evidence: none\n")
		}
		for _, e := range ev {
			if o, ok := e.(map[string]any); ok {
				fmt.Fprintf(&b, "  evidence: %s %s\n", field(o, "id"), field(o, "digest"))
			}
		}
	}
	if r, ok := m["required"].(map[string]any); ok {
		req, _ := r["evidence"].([]any)
		for _, e := range req {
			if o, ok := e.(map[string]any); ok {
				fmt.Fprintf(&b, "required evidence: %s %s %s produced by %s\n", field(o, "role"), field(o, "id"), field(o, "digest"), field(o, "producer"))
			}
		}
		if c, ok := r["conformance"].(map[string]any); ok {
			fmt.Fprintf(&b, "conformance: %s code %s step %s report %s %s\n", field(c, "status"), field(c, "code"),
				field(c, "producerStep"), field(c, "reportId"), field(c, "reportDigest"))
			fmt.Fprintf(&b, "  baseline %s checks %s\n", field(c, "baselineDigest"), field(c, "requiredChecks"))
		} else {
			b.WriteString("conformance: none required\n")
		}
	}
	fmt.Fprintf(&b, "requested: %s  deadline: %s\n", field(m, "requestedAt"), field(m, "deadline"))
	if inv, ok := m["invalidation"].(map[string]any); ok {
		fmt.Fprintf(&b, "invalidated: %s at %s (superseding commit %s, revision %s)\n", field(inv, "reason"), field(inv, "at"),
			field(inv, "supersedingCommit"), field(inv, "supersedingRevision"))
	}
	if a, ok := m["applicability"].(map[string]any); ok {
		fmt.Fprintf(&b, "applicability (server's record-derived observation, not proof current facts authorize progression): expired %s, applicable %s, reason %s\n",
			field(a, "expired"), field(a, "applicable"), field(a, "reason"))
	}
	if d, ok := m["decision"].(map[string]any); ok {
		by, _ := d["decidedBy"].(map[string]any)
		fmt.Fprintf(&b, "decision: %s by %s/%s at %s: %s\n", field(d, "decision"), field(by, "type"), field(by, "id"), field(d, "decidedAt"), field(d, "reason"))
	} else {
		b.WriteString("decision: none recorded\n")
	}
	if replayed {
		b.WriteString("replayed: the stored result of an earlier identical decision; nothing new was recorded\n")
	}
	return b.String()
}
