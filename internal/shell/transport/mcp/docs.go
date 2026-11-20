package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/docs"
)

const promptDocsIndexName = "kodema-docs"

// ResourceCatalog returns the deterministic list of embedded Kodema
// documentation resources exposed through MCP resources/list.
func ResourceCatalog() []api.MCPResource {
	topics := docs.Topics()
	resources := make([]api.MCPResource, len(topics))
	for index, topic := range topics {
		resources[index] = api.MCPResource{
			URI:         topic.URI,
			Name:        topic.Slug,
			Title:       topic.Title,
			Description: topic.Description,
			MIMEType:    topic.MIMEType,
		}
	}
	return resources
}

// PromptCatalog returns the deterministic list of embedded Kodema
// documentation prompts exposed through MCP prompts/list.
func PromptCatalog() []api.MCPPrompt {
	topics := docs.Topics()
	slugs := make([]string, len(topics))
	for index, topic := range topics {
		slugs[index] = topic.Slug
	}
	prompts := make([]api.MCPPrompt, 0, len(topics)+1)
	prompts = append(prompts, api.MCPPrompt{
		Name:        promptDocsIndexName,
		Title:       "Kodema Documentation",
		Description: "Load embedded Kodema documentation into context (defaults to overview; pass topic=all for every guide).",
		Arguments: []api.MCPPromptArgument{{
			Name:        "topic",
			Description: fmt.Sprintf("Documentation topic slug (%s, or all).", strings.Join(slugs, ", ")),
			Required:    false,
		}},
	})
	for _, topic := range topics {
		prompts = append(prompts, api.MCPPrompt{
			Name:        "kodema-" + topic.Slug,
			Title:       topic.Title,
			Description: topic.Description,
		})
	}
	return prompts
}

func readDocResource(uri string) (api.MCPResourcesReadResult, bool) {
	topic, ok := docs.LookupByURI(uri)
	if !ok {
		return api.MCPResourcesReadResult{}, false
	}
	return api.MCPResourcesReadResult{
		Contents: []api.MCPResourceContents{{
			URI:      topic.URI,
			MIMEType: topic.MIMEType,
			Text:     topic.Content,
		}},
	}, true
}

func getDocPrompt(name string, arguments map[string]string) (api.MCPPromptsGetResult, string, string) {
	if name == promptDocsIndexName {
		for key := range arguments {
			if key != "topic" {
				return api.MCPPromptsGetResult{}, api.CodeInvalidInput,
					fmt.Sprintf("prompt %q does not accept argument %q", name, key)
			}
		}
		rawTopic := strings.ToLower(strings.TrimSpace(arguments["topic"]))
		if rawTopic == "" {
			rawTopic = "overview"
		}
		if rawTopic == "all" {
			return api.MCPPromptsGetResult{
				Description: "Complete embedded Kodema documentation.",
				Messages: []api.MCPPromptMessage{{
					Role: "user",
					Content: api.MCPContentBlock{
						Type: "text",
						Text: docs.AllMarkdown(),
					},
				}},
			}, "", ""
		}
		topic, ok := docs.Lookup(rawTopic)
		if !ok {
			return api.MCPPromptsGetResult{}, api.CodeInvalidInput,
				fmt.Sprintf("unknown documentation topic %q; valid topics: %s, all", rawTopic, validDocTopicSlugs())
		}
		return promptResultForTopic(topic), "", ""
	}

	if slug, ok := strings.CutPrefix(name, "kodema-"); ok {
		topic, found := docs.Lookup(slug)
		if found && topic.Slug == slug {
			if len(arguments) > 0 {
				return api.MCPPromptsGetResult{}, api.CodeInvalidInput,
					fmt.Sprintf("prompt %q takes no arguments", name)
			}
			return promptResultForTopic(topic), "", ""
		}
	}

	return api.MCPPromptsGetResult{}, api.CodeNotFound, fmt.Sprintf("unknown MCP prompt %q", name)
}

func promptResultForTopic(topic docs.Topic) api.MCPPromptsGetResult {
	return api.MCPPromptsGetResult{
		Description: topic.Description,
		Messages: []api.MCPPromptMessage{{
			Role: "user",
			Content: api.MCPContentBlock{
				Type: "text",
				Text: topic.Content,
			},
		}},
	}
}

func validDocTopicSlugs() string {
	topics := docs.Topics()
	slugs := make([]string, len(topics))
	for index, topic := range topics {
		slugs[index] = topic.Slug
	}
	return strings.Join(slugs, ", ")
}

func decodeOptionalCursor(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	var params api.MCPListParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", err
	}
	return strings.TrimSpace(params.Cursor), nil
}
