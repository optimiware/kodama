// Package apispec assembles the public OpenAPI document of the core slice from
// the module-owned fragments in api/. The fragments stay the source of truth;
// the aggregate (api/openapi.yaml) is generated deterministically from the
// listed subset, so a route cannot be published without its owning fragment and
// the client generated from it cannot drift from the fragments.
//
// Assembly rules: paths are merged (a repeated path or operation ID is an
// error); components are merged by name, an identical definition is shared, a
// differing one is renamed with its fragment's prefix (Runs + Scalar) and every
// reference inside that fragment follows; in the fragments of typeNamespaced a
// component whose name a generated Go type of another section already uses (a
// schema EventType beside a parameter EventType) is renamed the same way; the
// shared security scheme is taken from the common fragment; references to common.yaml become local references;
// x-go-type hints (which name server packages) are dropped.
package apispec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// CoreFragments is the supported subset of the core slice, in assembly order.
// common.yaml comes first: its component names are never renamed.
//
// T-072 adds the manual agent, gate and delivery slice: agent profiles, human
// gates, environments and deployment records. Their fragments also describe
// runtime (attempt) routes, which another audience calls; those are served on
// the runtime API and are never part of this document (see audience).
//
// T-039 adds the M2 management surface: project model settings (gateway) and MCP
// server registrations (mcp). The gateway's chat-completions stream, the MCP
// proxy endpoint and the Git receive hook are workload or signature-authenticated
// interfaces, served beside the API, not part of this document.
//
// T-075 adds the business-loop management surface: the business dependency
// graph (work-dependencies) and the ChatOps destinations and delivery history
// (chatops). The SQL flow summaries (work-summaries) declare no route: they
// contribute query options and a member to the work list, which the list handler
// does not serve yet, so the fragment stays out until it does (a client must not
// grow types for an interface it cannot call). ChatOps' outbound webhook payload
// is a receiver contract, not a call a client makes, and is pruned like the
// workload schemas.
//
// T-170 adds the kernel Git hosting surface (kernel-git): the repository resource
// of a tenant and of a project, and the Git credential exchange. It describes the
// routes the kernel serves; the fuller drafts (repositories, git-credentials,
// git-pushes, git-triggers, git-webhooks) stay unpublished until their routes are
// served. The /git/ smart-HTTP transport is not JSON and is described in prose in
// the document's introduction, never as an operation.
var CoreFragments = []string{
	"common", "identity", "work", "configuration", "pipeline", "runs", "workspace", "artifacts", "observation", "logs",
	"agents", "gates", "delivery",
	"gateway", "mcp",
	"work-dependencies", "chatops",
	"kernel-git",
}

// audience reports whether a fragment path belongs to the public (human)
// audience. A fragment may also describe the runtime and workload routes of its
// module, which are authenticated by attempt credentials and mounted outside
// the public API prefix; the public document leaves them out.
func audience(path string) bool {
	for _, outside := range []string{"/attempts/", "/gateway/", "/internal/", "/runtime/", "/git-hooks/"} {
		if strings.HasPrefix(path, outside) {
			return false
		}
	}
	return true
}

// publicPaths returns the fragment's paths of the public audience.
func publicPaths(doc map[string]any) map[string]any {
	all, _ := doc["paths"].(map[string]any)
	out := make(map[string]any, len(all))
	for p, item := range all {
		if audience(p) {
			out[p] = item
		}
	}
	return out
}

// Info is the aggregate document's identity.
type Info struct {
	Title       string
	Version     string
	Description string
	ServerURL   string
}

// CoreInfo is the identity of the core subset document.
var CoreInfo = Info{
	Title:   "Kodama public API (core assembly subset)",
	Version: "0.4.0",
	Description: "The supported subset of the Kodama HTTP API served by the core, execution, manual delivery, M2 and business-loop assemblies (T-070, T-071, T-072, T-039, T-075): identity, work items, " +
		"variables and secrets, pipeline definitions, run admission/reads/cancellation, workspaces, artifact metadata, event observation and " +
		"attempt logs, the manual agent, gate and delivery slice (T-072): agent profiles, human gates, environments and deployment records, and the M2 model and tool " +
		"management routes (T-039): project model settings and MCP server registrations, and the business-loop routes (T-075): business dependency links between work items and ChatOps " +
		"notification destinations and delivery history, and the kernel Git hosting routes (T-170): the repository resource of a tenant and of a project and the Git credential exchange; " +
		"the Git smart-HTTP endpoint (/git/<tenant>/<project>/<repository>.git) is a non-JSON transport for the git client, authenticated with the exchanged credential as the HTTP Basic password, and is not an API operation. Generated from the module fragments in api/ by internal/app/apispec; do not edit by hand. Runtime (attempt) routes, " +
		"the gateway's chat-completions stream, the MCP proxy endpoint, the Git receive hook, the outbound ChatOps webhook payload and the flow-summary list options are not part of this subset. Paths are relative to the server URL, which must include the /api/v1 prefix.",
	ServerURL: "/api/v1",
}

type component struct{ section, name string }

// typedSections are the component sections a client generator turns into Go
// types: one name may be used by only one of them.
var typedSections = map[string]bool{"schemas": true, "parameters": true, "responses": true, "requestBodies": true}

// typeNamespaced are the fragments whose components are renamed when their name
// is already used by another typed section (a schema EventType beside the
// observation parameter EventType; the generator cannot disambiguate them).
// Fragments published before the business-loop composition are exempt so their
// generated type names stay exactly as released (AC-10); only fragments added
// since are checked.
var typeNamespaced = map[string]bool{"work-dependencies": true, "chatops": true, "kernel-git": true}

// prunedFragments are the fragments whose components are limited to those the
// public paths reach. The runtime-audience routes of a fragment (the gateway's
// chat-completions stream, the MCP proxy endpoint) never reach the public
// document, and neither may their schemas: a public client must not grow
// types for an interface it cannot call. Fragments published before the M2
// composition are exempt so their generated types stay exactly as released
// (AC-10); only fragments added since are pruned.
var prunedFragments = map[string]bool{"gateway": true, "mcp": true, "work-dependencies": true, "chatops": true}

// pruneUnreachable returns doc with the components no public path reaches
// removed. Security schemes are dropped altogether: operations name the shared
// scheme of the common fragment.
func pruneUnreachable(doc map[string]any) map[string]any {
	comps, _ := doc["components"].(map[string]any)
	if comps == nil {
		return doc
	}
	reach := map[component]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if ref, ok := child.(string); ok && k == "$ref" && strings.HasPrefix(ref, "#/components/") {
					parts := strings.SplitN(strings.TrimPrefix(ref, "#/components/"), "/", 3)
					if len(parts) >= 2 {
						c := component{parts[0], parts[1]}
						if !reach[c] {
							reach[c] = true
							sec, _ := comps[c.section].(map[string]any)
							walk(sec[c.name])
						}
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(publicPaths(doc))
	kept := map[string]any{}
	for sec, items := range comps {
		if sec == "securitySchemes" {
			continue
		}
		m, _ := items.(map[string]any)
		out := map[string]any{}
		for n, def := range m {
			if reach[component{sec, n}] {
				out[n] = def
			}
		}
		if len(out) > 0 {
			kept[sec] = out
		}
	}
	pruned := make(map[string]any, len(doc))
	for k, v := range doc {
		pruned[k] = v
	}
	pruned["components"] = kept
	return pruned
}

// Bundle assembles the aggregate from the fragment files in dir.
func Bundle(dir string, fragments []string, info Info) ([]byte, error) {
	docs := make(map[string]map[string]any, len(fragments))
	for _, name := range fragments {
		data, err := os.ReadFile(filepath.Join(dir, name+".yaml"))
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("%s.yaml: %w", name, err)
		}
		docs[name] = doc
	}
	return bundle(fragments, docs, info)
}

func bundle(order []string, docs map[string]map[string]any, info Info) ([]byte, error) {
	merged := map[string]map[string]any{} // section -> name -> definition
	owner := map[component]string{}       // who defined (section, name) first
	renames := map[string]map[component]string{}
	typedOwner := map[string]string{} // type name -> the typed section that holds it
	paths := map[string]any{}
	opIDs := map[string]string{}

	for i, frag := range order {
		if prunedFragments[frag] {
			docs[frag] = pruneUnreachable(docs[frag])
		}
		doc := docs[frag]
		renames[frag] = map[component]string{}
		comps, _ := doc["components"].(map[string]any)
		secs := make([]string, 0, len(comps))
		for s := range comps {
			secs = append(secs, s)
		}
		sort.Strings(secs)
		for _, sec := range secs {
			items, _ := comps[sec].(map[string]any)
			names := make([]string, 0, len(items))
			for n := range items {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				key := component{sec, n}
				if merged[sec] == nil {
					merged[sec] = map[string]any{}
				}
				prev, exists := merged[sec][n]
				if typeNamespaced[frag] && typedSections[sec] && !exists {
					if other, taken := typedOwner[n]; taken && other != sec {
						renamed := pascal(frag) + n
						if _, dup := typedOwner[renamed]; dup {
							return nil, fmt.Errorf("component %s/%s of %s collides with %s/%s even after renaming to %s", sec, n, frag, other, n, renamed)
						}
						if merged[sec][renamed] != nil {
							return nil, fmt.Errorf("component %s/%s of %s collides even after renaming to %s", sec, n, frag, renamed)
						}
						merged[sec][renamed] = items[n]
						owner[component{sec, renamed}] = frag
						renames[frag][key] = renamed
						typedOwner[renamed] = sec
						continue
					}
				}
				if typedSections[sec] && typedOwner[n] == "" {
					typedOwner[n] = sec
				}
				switch {
				case !exists:
					merged[sec][n], owner[key] = items[n], frag
				case sec == "securitySchemes" && schemeCompatible(prev, items[n]):
					// the one bearer scheme of the API
				case i > 0 && reflect.DeepEqual(prev, items[n]):
					// identical: shared (references stay as they are)
				default:
					renamed := pascal(frag) + n
					if _, dup := merged[sec][renamed]; dup {
						return nil, fmt.Errorf("component %s/%s of %s collides even after renaming to %s", sec, n, frag, renamed)
					}
					merged[sec][renamed] = items[n]
					owner[component{sec, renamed}] = frag
					renames[frag][key] = renamed
					if typedSections[sec] {
						typedOwner[renamed] = sec
					}
				}
			}
		}
	}

	for _, frag := range order {
		doc := docs[frag]
		ps := publicPaths(doc)
		for p, item := range ps {
			if _, dup := paths[p]; dup {
				return nil, fmt.Errorf("path %s is defined by more than one fragment", p)
			}
			paths[p] = item
		}
		for sec, items := range merged {
			for n, def := range items {
				if owner[component{sec, n}] == frag {
					rewritten, err := rewrite(def, frag, renames[frag])
					if err != nil {
						return nil, fmt.Errorf("%s: component %s/%s: %w", frag, sec, n, err)
					}
					items[n] = rewritten
				}
			}
		}
		for p, item := range ps {
			rewritten, err := rewrite(item, frag, renames[frag])
			if err != nil {
				return nil, fmt.Errorf("%s: path %s: %w", frag, p, err)
			}
			paths[p] = rewritten
			if err := collectOperationIDs(frag, p, rewritten, opIDs); err != nil {
				return nil, err
			}
		}
	}

	components := map[string]any{}
	for sec, items := range merged {
		components[sec] = items
	}
	out := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title": info.Title, "version": info.Version, "description": info.Description,
		},
		"servers":            []any{map[string]any{"url": info.ServerURL}},
		"security":           []any{map[string]any{"bearerAuth": []any{}}},
		"paths":              paths,
		"components":         components,
		"x-kodama-fragments": toAny(order),
	}
	if err := checkReferences(out); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("# GENERATED by internal/app/apispec from the api/*.yaml fragments listed in x-kodama-fragments.\n# Do not edit: run `make api-generate` and commit the result.\n")
	// Top-level members in a readable fixed order; everything below is sorted.
	root := &yaml.Node{Kind: yaml.MappingNode}
	for _, key := range []string{"openapi", "info", "servers", "security", "x-kodama-fragments", "paths", "components"} {
		var v yaml.Node
		if err := v.Encode(out[key]); err != nil {
			return nil, err
		}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &v)
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func schemeCompatible(a, b any) bool {
	am, _ := a.(map[string]any)
	bm, _ := b.(map[string]any)
	return am != nil && bm != nil && am["type"] == bm["type"] && am["scheme"] == bm["scheme"]
}

func pascal(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' {
			up = true
			continue
		}
		if up {
			b.WriteString(strings.ToUpper(string(r)))
			up = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rewrite returns a copy of v with every $ref redirected: references to
// common.yaml become local, local references follow this fragment's renames.
func rewrite(v any, frag string, renames map[component]string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			if strings.HasPrefix(k, "x-go-type") {
				// Server-side code generation hints name Kodama packages; the
				// public document must generate a standalone client from the
				// schema alone.
				continue
			}
			if k == "$ref" {
				ref, ok := child.(string)
				if !ok {
					return nil, errors.New("a $ref is not a string")
				}
				r, err := rewriteRef(ref, frag, renames)
				if err != nil {
					return nil, err
				}
				out[k] = r
				continue
			}
			c, err := rewrite(child, frag, renames)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			c, err := rewrite(child, frag, renames)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return v, nil
}

func rewriteRef(ref, frag string, renames map[component]string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "common.yaml#/"):
		return strings.TrimPrefix(ref, "common.yaml"), nil
	case strings.HasPrefix(ref, "#/components/"):
		parts := strings.SplitN(strings.TrimPrefix(ref, "#/components/"), "/", 3)
		if len(parts) < 2 {
			return "", fmt.Errorf("malformed reference %q", ref)
		}
		if renamed, ok := renames[component{parts[0], parts[1]}]; ok {
			parts[1] = renamed
			return "#/components/" + strings.Join(parts, "/"), nil
		}
		return ref, nil
	}
	return "", fmt.Errorf("%s: unsupported reference %q (only local and common.yaml references are bundled)", frag, ref)
}

var methods = map[string]bool{"get": true, "put": true, "post": true, "delete": true, "patch": true, "head": true, "options": true}

func collectOperationIDs(frag, path string, item any, seen map[string]string) error {
	m, _ := item.(map[string]any)
	for method, op := range m {
		if !methods[method] {
			continue
		}
		o, _ := op.(map[string]any)
		id, _ := o["operationId"].(string)
		if id == "" {
			return fmt.Errorf("%s: %s %s has no operationId", frag, strings.ToUpper(method), path)
		}
		if prev, dup := seen[id]; dup {
			return fmt.Errorf("operationId %s is defined by %s and %s", id, prev, frag)
		}
		seen[id] = frag
	}
	return nil
}

// checkReferences verifies that every local reference of the aggregate
// resolves, so a dangling reference is a generation error, not a client one.
func checkReferences(doc map[string]any) error {
	var bad []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				if k == "$ref" {
					if ref, ok := c.(string); ok && !resolves(doc, ref) {
						bad = append(bad, ref)
					}
					continue
				}
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(doc)
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("unresolved references: %s", strings.Join(bad, ", "))
	}
	return nil
}

func resolves(doc map[string]any, ref string) bool {
	if !strings.HasPrefix(ref, "#/") {
		return false
	}
	var cur any = doc
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg = strings.NewReplacer("~1", "/", "~0", "~").Replace(seg)
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[seg]; !ok {
			return false
		}
	}
	return true
}
