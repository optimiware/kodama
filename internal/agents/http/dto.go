package agentshttp

import (
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/httpapi"
)

// profileDTO maps a registered version to its wire form. The profile field is
// the canonical JSON copied from the immutable record, so integers keep their
// precision and the digest can be recomputed from it.
func profileDTO(p agents.ProfileVersion) AgentProfile {
	return AgentProfile{
		Id:        httpapi.ID(p.ID),
		Name:      p.Name,
		Version:   httpapi.Version(p.Version),
		Reference: p.Ref(),
		Digest:    string(p.Digest),
		Profile:   p.Content.Canonical(),
		CreatedBy: Actor{Id: p.CreatedBy.ID, Type: ActorType(p.CreatedBy.Type)},
		CreatedAt: p.CreatedAt.UTC(),
	}
}

func microTime(us int64) time.Time { return time.UnixMicro(us).UTC() }
