package deployment

import (
	"fmt"
	"sort"
	"strings"
)

// Rendering shows recorded identities, structured outcome and cleanup state.
// Nothing here is an approval: reading a deployment changes nothing, and the
// annotation values are shown only so a script can put them on its objects.

func deploymentText(m map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s (%s)  environment %s  namespace %s\n", field(m, "id"), field(m, "status"), field(m, "reason"),
		field(m, "environmentId"), field(m, "namespace"))
	fmt.Fprintf(&b, "run %s step %s action %s attempt %s\n", field(m, "runId"), field(m, "stepId"), field(m, "actionKey"), field(m, "attemptId"))
	if s, ok := m["source"].(map[string]any); ok {
		fmt.Fprintf(&b, "source: %s @ %s\n", field(s, "repositoryUrl"), field(s, "commit"))
	}
	if i, ok := m["image"].(map[string]any); ok {
		fmt.Fprintf(&b, "image: artifact %s %s\n", field(i, "artifactId"), field(i, "digest"))
	}
	fmt.Fprintf(&b, "configuration digest: %s\n", field(m, "configurationDigest"))
	if m["outcomeUnknown"] == true {
		b.WriteString("OUTCOME UNKNOWN: the apply may have taken effect; do not apply again until the outcome is settled by observation\n")
	}
	objs, _ := m["objects"].([]any)
	if len(objs) == 0 {
		b.WriteString("objects: none recorded\n")
	}
	for _, o := range objs {
		if obj, ok := o.(map[string]any); ok {
			fmt.Fprintf(&b, "object: %s %s/%s uid %s ready %s\n", field(obj, "kind"), field(obj, "namespace"), field(obj, "name"), field(obj, "uid"), field(obj, "ready"))
		}
	}
	if e := field(m, "expiresAt"); e != "-" {
		fmt.Fprintf(&b, "expires: %s\n", e)
	}
	if c, ok := m["cleanup"].(map[string]any); ok {
		fmt.Fprintf(&b, "cleanup: %s", field(c, "state"))
		if r := field(c, "reason"); r != "-" {
			fmt.Fprintf(&b, " (%s)", r)
		}
		if t := field(c, "requestedAt"); t != "-" {
			fmt.Fprintf(&b, " requested %s", t)
		}
		b.WriteString("\n")
	}
	if d := field(m, "deletedAt"); d != "-" {
		fmt.Fprintf(&b, "deleted: %s (the record, outcome and history are retained)\n", d)
	}
	fmt.Fprintf(&b, "version: %s (use with --if-version)\n", field(m, "version"))
	return b.String()
}

func receiptText(m map[string]any, replayed bool) string {
	var b strings.Builder
	d, _ := m["deployment"].(map[string]any)
	b.WriteString(deploymentText(d))
	switch {
	case replayed:
		b.WriteString("replayed: the stored result of an earlier identical request; nothing new was recorded\n")
	case m["created"] == true:
		b.WriteString("created: this request recorded the pre-apply deployment\n")
	default:
		b.WriteString("created: false - this logical action already had the deployment above; it is returned unchanged\n")
	}
	if ann, ok := m["annotations"].(map[string]any); ok && len(ann) > 0 {
		keys := make([]string, 0, len(ann))
		for k := range ann {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("annotations to put on every applied object:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s=%s\n", k, field(ann, k))
		}
	}
	return b.String()
}

func cleanupText(m map[string]any) string {
	return deploymentText(m) + "cleanup request recorded; this performs no deletion itself - a cleanup run deletes the owned objects and reports the result (check `kodama deployment get`)\n"
}

func listText(m map[string]any) string {
	var b strings.Builder
	for _, d := range items(m) {
		c, _ := d["cleanup"].(map[string]any)
		fmt.Fprintf(&b, "%s  %-9s %-18s env %s  run %s  cleanup %s  v%s\n", field(d, "id"), field(d, "status"), field(d, "reason"),
			field(d, "environmentId"), field(d, "runId"), field(c, "state"), field(d, "version"))
	}
	if len(items(m)) == 0 {
		b.WriteString("no deployments\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}
