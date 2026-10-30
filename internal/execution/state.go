package execution

import (
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/pipeline"
)

// resolveStateBindings pins the state declarations of a v1alpha3 document.
// The server resolves namespaces/prefixes against current project authority at
// a later integration checkpoint; this function only freezes the declaration
// into the run snapshot so a workload cannot change it.
func (s *Service) resolveStateBindings(doc pipeline.Document, snap Snapshot) (Snapshot, error) {
	if !doc.IsV1Alpha3() {
		return snap, nil
	}
	if len(snap.States) != 0 || len(snap.ArtifactFiles) != 0 {
		return Snapshot{}, Unavailablef(nil, "configuration supplied undeclared state or artifact bindings")
	}
	for _, step := range doc.Spec.Steps {
		if !step.Executable() {
			continue
		}
		if len(step.State) > 0 {
			bindings := make([]runtimev1.StateBinding, 0, len(step.State))
			for _, name := range sortedStateNames(step.State) {
				b := step.State[name]
				bindings = append(bindings, runtimev1.StateBinding{
					Name: name, Namespace: b.Namespace, Prefix: b.Prefix,
					Write: b.Access == pipeline.StateReadWrite,
				})
			}
			snap.States = append(snap.States, StepState{Step: step.ID, Bindings: bindings})
		}
	}
	return snap, nil
}

func sortedStateNames(in map[string]pipeline.StateBinding) []string {
	out := make([]string, 0, len(in))
	for name := range in {
		out = append(out, name)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
