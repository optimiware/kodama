package artifacts

import "testing"

func TestProvenanceWorkspaceState(t *testing.T) {
	base := Provenance{RepositoryURL: "https://git.example.test/acme/app.git", Commit: "1111111111111111111111111111111111111111"}
	ref := "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	for name, tc := range map[string]struct {
		mut      func(Provenance) Provenance
		ok       bool
		modified bool
	}{
		"private":          {func(p Provenance) Provenance { return p }, true, false},
		"clean":            {func(p Provenance) Provenance { p.WorkspaceState = WorkspaceClean; p.Lineage = ref; return p }, true, false},
		"modified":         {func(p Provenance) Provenance { p.WorkspaceState = WorkspaceModified; p.IgnoredPresent = true; return p }, true, true},
		"unknown state":    {func(p Provenance) Provenance { p.WorkspaceState = "dirty"; return p }, false, false},
		"ignored no state": {func(p Provenance) Provenance { p.IgnoredPresent = true; return p }, false, false},
		"lineage no state": {func(p Provenance) Provenance { p.Lineage = ref; return p }, false, false},
		"bad lineage":      {func(p Provenance) Provenance { p.WorkspaceState = WorkspaceClean; p.Lineage = "x"; return p }, false, false},
		"state no source":  {func(Provenance) Provenance { return Provenance{WorkspaceState: WorkspaceClean} }, false, false},
	} {
		p := tc.mut(base)
		if err := p.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
		if p.Modified() != tc.modified {
			t.Errorf("%s: modified=%v", name, p.Modified())
		}
	}
}
