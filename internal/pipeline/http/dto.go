package pipelinehttp

import (
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
)

// definitionDTO maps a registered definition to its wire form. The definition
// field is the canonical JSON copied from the immutable record, so integers
// keep their precision and the digest can be recomputed from it.
func definitionDTO(d pipeline.Definition) PipelineDefinition {
	return PipelineDefinition{
		Id:         httpapi.ID(d.ID),
		Name:       d.Name,
		Version:    httpapi.Version(d.Version),
		Digest:     string(d.Digest),
		Definition: d.Content.Canonical(),
		CreatedBy:  Actor{Id: d.CreatedBy.ID, Type: ActorType(d.CreatedBy.Type)},
		CreatedAt:  d.CreatedAt.UTC(),
	}
}

func microTime(us int64) time.Time { return time.UnixMicro(us).UTC() }
