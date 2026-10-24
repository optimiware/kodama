package dependencieshttp

import (
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
)

func ts(t time.Time) time.Time { return t.UTC() }

func actorDTO(a work.Actor) DependencyActor {
	return DependencyActor{Id: a.ID, Type: DependencyActorType(a.Type)}
}

// dependencyDTO is a total mapping: it has no failure path, so a committed read
// can always be rendered.
func dependencyDTO(p dependencies.Prerequisite) Dependency {
	return Dependency{
		DependentId: httpapi.ID(p.Link.DependentID), DependencyId: httpapi.ID(p.Link.PrerequisiteID),
		CreatedAt: ts(p.CreatedAt), CreatedBy: actorDTO(p.CreatedBy),
	}
}

func changeDTO(o dependencies.Outcome) DependencyChange {
	return DependencyChange{
		DependentId: httpapi.ID(o.Link.DependentID), DependencyId: httpapi.ID(o.Link.PrerequisiteID),
		Changed: o.Changed, Version: httpapi.Version(o.Owner.Version),
	}
}
