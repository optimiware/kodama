package observationhttp

import "github.com/optimiware/kodama/internal/observation"

func snapshotDTO(cursor string, snap observation.Snapshot) Snapshot {
	out := Snapshot{Cursor: cursor, Resources: make([]ObservedResource, 0, len(snap.Resources))}
	for _, r := range snap.Resources {
		d := ObservedResource{Type: ObservedResourceType(r.Type), Id: r.ID, Version: r.Version, State: r.State}
		if r.Kind != "" {
			k := r.Kind
			d.Kind = &k
		}
		if r.RunID != "" {
			v := r.RunID
			d.RunId = &v
		}
		if r.StepID != "" {
			v := r.StepID
			d.StepId = &v
		}
		out.Resources = append(out.Resources, d)
	}
	return out
}
