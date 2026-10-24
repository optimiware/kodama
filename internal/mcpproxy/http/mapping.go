package mcpproxyhttp

import (
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// serverDTO maps a registration to its wire form. It carries the credential
// reference (name and exact version) and never a value.
func serverDTO(r mcpproxy.Registration) McpServer {
	tools := make([]ToolName, len(r.Tools))
	copy(tools, r.Tools)
	return McpServer{
		Id: httpapi.ID(r.ID), Endpoint: r.Endpoint,
		Credential: CredentialRef{Name: r.Secret.Name, Version: httpapi.Version(r.Secret.Version)},
		Tools:      tools, Version: httpapi.Version(r.Version),
		CreatedBy: r.CreatedBy.ID, CreatedAt: r.CreatedAt.UTC(), UpdatedBy: r.UpdatedBy.ID, UpdatedAt: r.UpdatedAt.UTC(),
	}
}

func draftFrom(in McpServerDraft) mcpproxy.Draft {
	return mcpproxy.Draft{
		Endpoint: in.Endpoint,
		Secret:   mcpproxy.SecretRef{Name: in.Credential.Name, Version: int64(in.Credential.Version)},
		Tools:    append([]string{}, in.Tools...),
	}
}
