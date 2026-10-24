package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
	"github.com/optimiware/kodama/internal/agents"
)

// mcpServer is one allowed registration and the tools the profile pins for it.
type mcpServer struct {
	// Registration is the proxy registration ID (the segment before "/").
	Registration string
	// Name is the OpenCode server name: the registration ID normalized the way
	// OpenCode normalizes names (letters, digits, "_" and "-" survive).
	Name  string
	Tools []string
}

var (
	normalizeRE   = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	registrationR = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	toolNameR     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

func normalize(s string) string { return normalizeRE.ReplaceAllString(s, "_") }

// servers maps the profile's qualified tool allow-list (<registrationID>/<tool>)
// onto OpenCode servers. It fails closed on anything it cannot map without
// ambiguity: a malformed entry, two registrations that normalize to one server
// name, or two tools that normalize to one `<server>_<tool>` permission name
// (OpenCode's own permission and tool name). A bare name or wildcard grants
// nothing and is refused.
func servers(tools []string) ([]mcpServer, error) {
	byReg := map[string]*mcpServer{}
	byName := map[string]string{}
	full := map[string]string{}
	for _, q := range tools {
		reg, tool, ok := strings.Cut(q, "/")
		if !ok || !registrationR.MatchString(reg) || !toolNameR.MatchString(tool) || strings.ContainsAny(q, "*?") {
			return nil, fmt.Errorf("tool %q is not a qualified <registration>/<tool> name", q)
		}
		name := normalize(reg)
		if other, dup := byName[name]; dup && other != reg {
			return nil, fmt.Errorf("registrations %q and %q normalize to the same OpenCode server name", other, reg)
		}
		byName[name] = reg
		perm := name + "_" + normalize(tool)
		if other, dup := full[perm]; dup && other != q {
			return nil, fmt.Errorf("tools %q and %q normalize to the same OpenCode tool name %q", other, q, perm)
		}
		full[perm] = q
		s := byReg[reg]
		if s == nil {
			s = &mcpServer{Registration: reg, Name: name}
			byReg[reg] = s
		}
		if !contains(s.Tools, tool) {
			s.Tools = append(s.Tools, tool)
		}
	}
	out := make([]mcpServer, 0, len(byReg))
	for _, s := range byReg {
		sort.Strings(s.Tools)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Registration < out[j].Registration })
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type rule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// permissions is the ordered rule list (the last matching rule wins). It starts
// from deny-everything and then allows only: workspace-internal reading,
// searching and editing, shell (the trusted-engineer execution profile of the
// agent runtime specification), and exactly the pinned MCP tools. Nothing is
// left to "ask": measurement shows `opencode run` auto-rejects an ask when there
// is no human, and explicit rules keep that from being the mechanism.
func permissions(srv []mcpServer) []rule {
	r := []rule{{"*", "*", "deny"}}
	for _, a := range []string{"read", "glob", "grep", "edit", "shell"} {
		r = append(r, rule{a, "*", "allow"})
	}
	r = append(r, rule{"external_directory", "*", "deny"})
	for _, s := range srv {
		for _, t := range s.Tools {
			r = append(r, rule{s.Name + "_" + normalize(t), "*", "allow"})
		}
	}
	return r
}

// openCodeConfig renders the user-level configuration the wrapper writes into
// the private OpenCode home. The static security policy lives in the managed
// configuration; this file carries what depends on the pinned profile: the
// model, its output cap, the explicit permission rules and the MCP servers,
// which point at the loopback proxy and carry no credential.
func openCodeConfig(cfg agents.WrapperConfig, srv []mcpServer, outputTokens int64) ([]byte, error) {
	model := cfg.Content.Model.Name
	if model == "" || strings.Contains(model, "/") || strings.Contains(model, "#") {
		return nil, fmt.Errorf("model name %q cannot be used as an OpenCode model key", model)
	}
	servers := map[string]any{}
	for _, s := range srv {
		servers[s.Name] = map[string]any{
			"type":     "remote",
			"url":      fmt.Sprintf("http://%s:%d%s%s", spec.ProxyHost, spec.ProxyPort, spec.MCPPath, s.Registration),
			"oauth":    false,
			"codemode": false,
			"protocol": "legacy",
		}
	}
	// The provider endpoint and policies repeat the managed configuration (which
	// wins when present), so a run without the baked file is still confined.
	provider := spec.ProviderDefinition()
	provider["models"] = map[string]any{
		model: map[string]any{"name": model, "limit": map[string]any{"context": 128000, "output": outputTokens}},
	}
	doc := map[string]any{
		"$schema":     "https://opencode.ai/config.json",
		"model":       spec.ProviderID + "/" + model,
		"small_model": spec.ProviderID + "/" + model,
		"update":      "disable",
		"permissions": permissions(srv),
		"policies":    spec.Policies(),
		"providers":   map[string]any{spec.ProviderID: provider},
	}
	if len(servers) > 0 {
		doc["mcp"] = map[string]any{"servers": servers}
	}
	return json.MarshalIndent(doc, "", "  ")
}
