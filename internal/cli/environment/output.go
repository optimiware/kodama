package environment

import (
	"fmt"
	"sort"
	"strings"
)

// Rendering shows references and explicit nonsecret configuration. No secret
// or kubeconfig value exists in any response, so none can be printed.

func bindingText(b map[string]any) string {
	return fmt.Sprintf("%s <- %s (%s)", field(b, "name"), field(b, "ref"), field(b, "delivery"))
}

func environmentText(m map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s\n", field(m, "id"), field(m, "name"), field(m, "kind"))
	fmt.Fprintf(&b, "target: %s  namespace: %s\n", field(m, "target"), field(m, "namespace"))
	if profile := field(m, "targetProfile"); profile != "-" {
		fmt.Fprintf(&b, "target profile: %s (%s, %s)\n", profile, field(m, "targetProvider"), field(m, "targetProfileDigest"))
	}
	if cfg, ok := m["targetConfig"].(map[string]any); ok && len(cfg) > 0 {
		keys := make([]string, 0, len(cfg))
		for k := range cfg {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "target config: %s=%s\n", k, field(cfg, k))
		}
	}
	if kc, ok := m["kubeconfig"].(map[string]any); ok {
		fmt.Fprintf(&b, "kubeconfig: %s\n", bindingText(kc))
	}
	secrets, _ := m["secrets"].([]any)
	if len(secrets) == 0 {
		b.WriteString("script credentials: none\n")
	}
	for _, s := range secrets {
		if o, ok := s.(map[string]any); ok {
			fmt.Fprintf(&b, "script credential: %s\n", bindingText(o))
		}
	}
	if cfg, ok := m["config"].(map[string]any); ok && len(cfg) > 0 {
		keys := make([]string, 0, len(cfg))
		for k := range cfg {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "config: %s=%s\n", k, field(cfg, k))
		}
	} else {
		b.WriteString("config: none\n")
	}
	if ttl := field(m, "previewTtlSeconds"); ttl != "-" {
		fmt.Fprintf(&b, "preview TTL: %s seconds\n", ttl)
	}
	fmt.Fprintf(&b, "version: %s (use with --if-version)\n", field(m, "version"))
	fmt.Fprintf(&b, "created: %s  updated: %s\n", field(m, "createdAt"), field(m, "updatedAt"))
	return b.String()
}

func listText(m map[string]any) string {
	var b strings.Builder
	for _, e := range items(m) {
		fmt.Fprintf(&b, "%s  %-24s %-8s ns %-20s target %-12s v%s\n", field(e, "id"), field(e, "name"), field(e, "kind"),
			field(e, "namespace"), field(e, "target"), field(e, "version"))
	}
	if len(items(m)) == 0 {
		b.WriteString("no environments\n")
	}
	b.WriteString(pageFooter(m) + "\n")
	return b.String()
}
