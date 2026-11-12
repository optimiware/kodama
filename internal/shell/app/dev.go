package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// devRegistry is the explicitly named development image resolver. It never
// contacts a registry: an already digest-pinned reference is returned as-is and
// any other reference is pinned to a deterministic local digest so admission can
// be exercised without network access. It is refused in production.
type devRegistry struct{}

var _ pipeline.Resolver = devRegistry{}

func (devRegistry) Resolve(ctx context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	if err := ctx.Err(); err != nil {
		return pipeline.ResolvedImage{}, err
	}
	reference := strings.TrimSpace(request.Reference)
	if reference == "" {
		return pipeline.ResolvedImage{}, fault.New(fault.InvalidInput, "image reference is empty")
	}
	if index := strings.LastIndex(reference, "@sha256:"); index > 0 {
		return pipeline.ResolvedImage{
			Reference: reference[:index],
			Digest:    reference[index+1:],
		}, nil
	}
	sum := sha256.Sum256([]byte(reference))
	return pipeline.ResolvedImage{
		Reference: reference,
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}
