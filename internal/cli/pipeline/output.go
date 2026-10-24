package pipeline

import (
	"fmt"
	"strings"
)

func registeredText(m map[string]any, status int) string {
	verb := "registered"
	if status == 200 {
		verb = "already registered (identical content; nothing was created)"
	}
	return fmt.Sprintf("%s: %s  pipeline %q version %s\ndigest %s\n", verb, field(m, "id"), field(m, "name"), field(m, "version"), field(m, "digest"))
}

func definitionText(m map[string]any) string {
	return fmt.Sprintf("%s  pipeline %q version %s\ndigest %s\ncreated %s\n", field(m, "id"), field(m, "name"), field(m, "version"), field(m, "digest"), field(m, "createdAt"))
}

func listText(m map[string]any) string {
	var b strings.Builder
	for _, d := range items(m) {
		fmt.Fprintf(&b, "%s  %s v%s  %s\n", field(d, "id"), field(d, "name"), field(d, "version"), field(d, "digest"))
	}
	if len(items(m)) == 0 {
		b.WriteString("no pipeline definitions\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}
