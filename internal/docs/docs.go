// Package docs embeds version-locked, agent-facing Kodema product
// documentation shared by the MCP edge (initialize instructions, resources,
// and prompts) and the kodema CLI (`kodema docs`).
//
// This package is neutral and standard-library only so both Shell transport
// and Userland CLI packages can consume the exact same documentation bundle.
package docs

import (
	_ "embed"
	"strings"
)

// URIScheme is the MCP resource URI prefix for embedded Kodema documentation.
const URIScheme = "kodema://docs/"

// MIMETypeMarkdown is the media type of every embedded documentation topic.
const MIMETypeMarkdown = "text/markdown"

// SkillDescription is the portable Agent Skill description for Kodema's
// embedded product documentation skill.
const SkillDescription = "Work with Kodema pipelines, container workloads, approvals, task handoffs, Git rules, and shared context repositories."

// Topic is one embedded Kodema documentation document.
type Topic struct {
	Slug        string `json:"slug"`
	URI         string `json:"uri"`
	Title       string `json:"title"`
	Description string `json:"description"`
	MIMEType    string `json:"mime_type"`
	Content     string `json:"content"`
}

var (
	//go:embed content/overview.md
	overviewMD string

	//go:embed content/pipelines.md
	pipelinesMD string

	//go:embed content/workload.md
	workloadMD string

	//go:embed content/git-rules.md
	gitRulesMD string

	//go:embed content/workflows.md
	workflowsMD string

	//go:embed content/context.md
	contextMD string
)

const coreModelSummary = `Kodema is an API-first, multi-tenant software factory running container pipelines and coding agents on Kubernetes or Podman.

Core model:
- A pipeline is an ordered list of actions (container or request); an execution runs a pipeline once with an immutable admission snapshot; a task is a conversation that starts executions.
- Pipelines are managed only through the API (pipeline_apply / pipeline_start); there are no when-conditions, loops, parallel actions, or Git auto-sync.
- Mutations require an idempotency_key (identical retries converge) and versioned updates require expected_version (re-read on version_conflict).
- Planned human/service gates use request actions (role/<role> or service/<id>) and retain /workspace; unplanned questions use task_ask / task_reply across fresh executions after saving durable commits or digests.
- Approval-guarded Git refs (option approve.<ref> in admin/conf/git.conf) require a passed request: role/<role> in the pushing execution matching exact operation, resource, ref, expected_base, and candidate.`

type topicSpec struct {
	slug        string
	title       string
	description string
	content     string
}

var catalog = buildCatalog([]topicSpec{
	{
		slug:        "overview",
		title:       "Kodema Overview",
		description: "Core model (tenants, projects, pipelines, executions, tasks), roles, and API/MCP conventions.",
		content:     overviewMD,
	},
	{
		slug:        "pipelines",
		title:       "Kodema Pipelines",
		description: "Pipeline YAML schema, container and request actions, subject lookups, and on.push triggers.",
		content:     pipelinesMD,
	},
	{
		slug:        "workload",
		title:       "Container Workload Contract",
		description: "Container action contract: /workspace, /kodema/* files, env vars, LLM proxy, and toolbox.",
		content:     workloadMD,
	},
	{
		slug:        "git-rules",
		title:       "Git Service and Access Rules",
		description: "Git companion conventions, admin/conf/git.conf permissions, and approval-guarded refs.",
		content:     gitRulesMD,
	},
	{
		slug:        "workflows",
		title:       "Kodema Workflows",
		description: "Planned approvals, task handoffs (ask/reply), digest-pinned delivery, and failure recovery.",
		content:     workflowsMD,
	},
	{
		slug:        "context",
		title:       "Shared Company and Project Context",
		description: "Company/project Git context repositories, Markdown skills, and kodema context pull.",
		content:     contextMD,
	},
})

func buildCatalog(specs []topicSpec) []Topic {
	topics := make([]Topic, len(specs))
	for i, spec := range specs {
		topics[i] = Topic{
			Slug:        spec.slug,
			URI:         URIScheme + spec.slug,
			Title:       spec.title,
			Description: spec.description,
			MIMEType:    MIMETypeMarkdown,
			Content:     spec.content,
		}
	}
	return topics
}

var mcpInstructions = buildMCPInstructions(catalog)

func buildMCPInstructions(topics []Topic) string {
	var builder strings.Builder
	builder.WriteString(coreModelSummary)
	builder.WriteString("\n\nDocumentation:\n")
	builder.WriteString("- Read full guides via MCP resources (resources/list, resources/read) or MCP prompts (prompts/list, prompts/get):\n")
	for _, topic := range topics {
		builder.WriteString("  - ")
		builder.WriteString(topic.URI)
		builder.WriteString(" — ")
		builder.WriteString(topic.Description)
		builder.WriteByte('\n')
	}
	builder.WriteString(`- From a shell or container step, run "kodema docs [topic]" or "kodema call --list".`)
	return builder.String()
}

// MCPInstructions returns the concise server instructions advertised during
// the MCP initialize handshake.
func MCPInstructions() string {
	return mcpInstructions
}

// Topics returns a copy of all embedded documentation topics in deterministic
// declaration order.
func Topics() []Topic {
	out := make([]Topic, len(catalog))
	copy(out, catalog)
	return out
}

// Lookup returns the embedded topic matching slug (case-insensitive, trimmed).
func Lookup(slug string) (Topic, bool) {
	normalized := strings.ToLower(strings.TrimSpace(slug))
	for _, topic := range catalog {
		if topic.Slug == normalized {
			return topic, true
		}
	}
	return Topic{}, false
}

// LookupByURI returns the embedded topic matching an exact kodema://docs/<slug>
// URI.
func LookupByURI(uri string) (Topic, bool) {
	for _, topic := range catalog {
		if topic.URI == uri {
			return topic, true
		}
	}
	return Topic{}, false
}

// AllMarkdown concatenates every topic in declaration order separated by
// horizontal rules.
func AllMarkdown() string {
	var builder strings.Builder
	for index, topic := range catalog {
		if index > 0 {
			builder.WriteString("\n---\n\n")
		}
		builder.WriteString(strings.TrimSpace(topic.Content))
		builder.WriteByte('\n')
	}
	return builder.String()
}

// SkillDocument returns a portable Agent Skill SKILL.md document with YAML
// frontmatter that summarizes Kodema and indexes its reference guides under
// references/<slug>.md.
func SkillDocument() string {
	var builder strings.Builder
	builder.WriteString("---\n")
	builder.WriteString("name: kodema\n")
	builder.WriteString("description: ")
	builder.WriteString(SkillDescription)
	builder.WriteString("\n---\n\n")
	builder.WriteString("# Kodema\n\n")
	builder.WriteString(coreModelSummary)
	builder.WriteString("\n\n## Reference guides\n\n")
	builder.WriteString("Read the bundled Markdown guides in this skill directory (or run `kodema docs [topic]` / `kodema call --list`):\n\n")
	for _, topic := range catalog {
		builder.WriteString("- `references/")
		builder.WriteString(topic.Slug)
		builder.WriteString(".md` — ")
		builder.WriteString(topic.Title)
		builder.WriteString(": ")
		builder.WriteString(topic.Description)
		builder.WriteByte('\n')
	}
	return builder.String()
}
