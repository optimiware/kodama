package configuration

import (
	"fmt"
	"strings"
)

// Rendering shows management metadata only. A secret's value, its ciphertext
// and its encoded form are never part of any response, so none can be printed.

func variableText(m map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s = %s\n", field(m, "id"), field(m, "name"), field(m, "type"), field(m, "value"))
	fmt.Fprintf(&b, "value version: %s (pinned by runs)  resource version: %s (use with --if-version)\n", field(m, "valueVersion"), field(m, "version"))
	if d := field(m, "description"); d != "-" {
		fmt.Fprintf(&b, "description: %s\n", d)
	}
	if d := field(m, "deletedAt"); d != "-" {
		fmt.Fprintf(&b, "deleted: %s (the name stays reserved)\n", d)
	}
	return b.String()
}

func variableListText(m map[string]any) string {
	var b strings.Builder
	for _, v := range items(m) {
		fmt.Fprintf(&b, "%s  %-24s %-8s %s  (value v%s, resource v%s)\n", field(v, "id"), field(v, "name"), field(v, "type"), field(v, "value"), field(v, "valueVersion"), field(v, "version"))
	}
	if len(items(m)) == 0 {
		b.WriteString("no variables\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}

func secretText(m map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  kind %s  %s\n", field(m, "id"), field(m, "name"), field(m, "kind"), field(m, "ref"))
	fmt.Fprintf(&b, "current value version: %s  resource version: %s (use with --if-version)\n", field(m, "currentVersion"), field(m, "version"))
	if d := field(m, "deletedAt"); d != "-" {
		fmt.Fprintf(&b, "deleted: %s (the name stays reserved)\n", d)
	}
	if r, ok := m["retention"].(map[string]any); ok {
		b.WriteString(retentionText(r))
	}
	return b.String()
}

func retentionText(r map[string]any) string {
	return fmt.Sprintf("retention: %s pinned version(s), %s active pin(s), %s consumer action(s) pending, backups %s\n"+
		"Deletion purges nothing; pinned versions, ciphertext and the audit trail are retained.\n",
		field(r, "pinnedVersions"), field(r, "activePins"), field(r, "pendingConsumerHandling"), field(r, "backupRetention"))
}

func secretListText(m map[string]any) string {
	var b strings.Builder
	for _, s := range items(m) {
		fmt.Fprintf(&b, "%s  %-24s %-10s value v%s  resource v%s\n", field(s, "id"), field(s, "name"), field(s, "kind"), field(s, "currentVersion"), field(s, "version"))
	}
	if len(items(m)) == 0 {
		b.WriteString("no secrets\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}

func versionsText(m map[string]any) string {
	var b strings.Builder
	for _, v := range items(m) {
		revoked := "active"
		if r := field(v, "revokedAt"); r != "-" {
			revoked = "revoked " + r
		}
		fmt.Fprintf(&b, "v%s  created %s by %s  %s\n", field(v, "version"), field(v, "createdAt"), field(v, "createdBy"), revoked)
	}
	if len(items(m)) == 0 {
		b.WriteString("no versions\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}

func rotatedText(m map[string]any) string {
	v, _ := m["version"].(map[string]any)
	s, _ := m["secret"].(map[string]any)
	return fmt.Sprintf("added value version %s to %s (%s); resource version now %s\n", field(v, "version"), field(s, "name"), field(s, "id"), field(s, "version"))
}

func revokedText(m map[string]any) string {
	v, _ := m["version"].(map[string]any)
	s, _ := m["secret"].(map[string]any)
	ch, _ := m["consumerHandling"].(map[string]any)
	return fmt.Sprintf("revoked value version %s of %s at %s\n"+
		"Revocation blocks further resolution; it cannot recall values already delivered. %s of %s consumer action(s) are pending until their owners acknowledge them.\n",
		field(v, "version"), field(s, "name"), field(v, "revokedAt"), field(ch, "pending"), field(ch, "total"))
}

func deletedSecretText(m map[string]any) string {
	s, _ := m["secret"].(map[string]any)
	r, _ := m["retention"].(map[string]any)
	return fmt.Sprintf("deleted %s (%s); the name stays reserved\n%s", field(s, "name"), field(s, "id"), retentionText(r))
}
