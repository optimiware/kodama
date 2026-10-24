package workspace

import (
	"fmt"
	"strings"
)

// workspaceText renders the authoritative workspace fields. The response has
// no clone URL: the text says so instead of inventing one.
func workspaceText(m map[string]any, status int) string {
	var b strings.Builder
	state := field(m, "state")
	fmt.Fprintf(&b, "%s  work item %s  %s\n", field(m, "id"), field(m, "workItemId"), state)
	fmt.Fprintf(&b, "repository: %s\n", field(m, "repositoryId"))
	fmt.Fprintf(&b, "branch: %s\n", field(m, "branch"))
	fmt.Fprintf(&b, "accepted commit: %s\n", field(m, "acceptedCommit"))
	fmt.Fprintf(&b, "writer attempt: %s\n", field(m, "writerAttemptId"))
	if p, ok := m["publication"].(map[string]any); ok {
		fmt.Fprintf(&b, "publication in flight: attempt %s, commit %s (expected %s), started %s\n",
			field(p, "attemptId"), field(p, "commit"), field(p, "expectedCommit"), field(p, "startedAt"))
	}
	if v := field(m, "archiveRequestedAt"); v != "-" {
		fmt.Fprintf(&b, "archive requested: %s\n", v)
	}
	if v := field(m, "lastError"); v != "-" {
		fmt.Fprintf(&b, "last error: %s\n", v)
	}
	fmt.Fprintf(&b, "resource version: %s (use with --if-version)\n", field(m, "version"))
	switch state {
	case "provisioning":
		fmt.Fprintf(&b, "The workspace is still provisioning (HTTP %d). Repeat `kodama workspace provision --work %s` to retry; wait until it is available before running work.\n", status, field(m, "workItemId"))
	case "archived":
		b.WriteString("The workspace is archived; `kodama workspace restore` reopens the same workspace and branch. Source history is not deleted.\n")
	default:
		b.WriteString("Clone or fetch the branch from your repository using your own authorized Git credentials; Kodama does not return a clone URL or publisher credentials.\n")
	}
	return b.String()
}
