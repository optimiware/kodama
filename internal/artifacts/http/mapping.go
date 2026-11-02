package artifactshttp

import (
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/httpapi"
)

// toDTO renders the safe public representation: metadata and the optional
// download instruction, never a storage key, bucket or credential.
func toDTO(v artifacts.View) Artifact {
	a := v.Artifact
	out := Artifact{
		Id: httpapi.ID(a.ID()), AttemptId: httpapi.ID(a.Attempt()), RunId: httpapi.ID(a.Reservation().Run), StepId: a.Reservation().Step, Name: a.Name(),
		Kind: ArtifactKind(a.Kind()), Role: ArtifactRole(a.Role()), State: ArtifactState(a.State()),
		Availability: availability(a.Disposition()), Digest: string(a.Digest()), Version: httpapi.Version(a.Version()), CreatedAt: a.CreatedAt().UTC(),
	}
	if a.MediaType() != "" {
		mt := a.MediaType()
		out.MediaType = &mt
	}
	if size, ok := a.Size(); ok {
		out.SizeBytes = &size
	}
	if a.Registry() != "" {
		reg := a.Registry()
		out.Registry = &reg
	}
	if p := a.Provenance(); !p.SourceSet.IsZero() {
		set := &ArtifactSourceSet{Digest: p.SourceSet.Digest}
		for _, c := range p.SourceSet.Commits() {
			set.Commits = append(set.Commits, ArtifactSourceCommit{RepositoryId: httpapi.ID(c.RepositoryID), Commit: c.Commit})
		}
		out.SourceSet = set
	} else if !p.IsZero() {
		src := &SourceProvenance{RepositoryUrl: p.RepositoryURL, Commit: p.Commit}
		if p.WorkspaceState != "" {
			// A run_shared producer: what the server recorded at its command begin,
			// so a modified workspace is never mistaken for the commit (DQ-16).
			state, ignored := p.WorkspaceState, p.IgnoredPresent
			src.WorkspaceState, src.IgnoredPresent = &state, &ignored
			if p.Lineage != "" {
				lineage := p.Lineage
				src.Lineage = &lineage
			}
		}
		out.Source = src
	}
	if t := a.VerifiedAt(); t != nil {
		vt := t.UTC()
		out.VerifiedAt = &vt
	}
	if v.Download != nil {
		out.Download = &ArtifactDownload{Method: ArtifactDownloadMethod(v.Download.Method), Url: v.Download.URL, ExpiresAt: v.Download.ExpiresAt.UTC()}
	}
	return out
}

func availability(d artifacts.Disposition) ArtifactAvailability {
	switch d {
	case artifacts.DispositionAbandoned:
		return Abandoned
	case artifacts.DispositionRemoved:
		return Removed
	}
	return Available
}
