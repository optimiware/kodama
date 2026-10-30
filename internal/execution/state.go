package execution

import (
	"context"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
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

// resolveArtifactFiles pins the verified file descriptors a v1alpha3 step
// consumes. An ancestor output is resolved from the producing step's recorded
// descriptor, never from a mutable path or a caller-supplied digest. Run-input
// artifacts are not yet represented in the execution snapshot and fail closed
// until admission pins their descriptors.
func resolveArtifactFiles(a *Aggregate, step *StepRun, snap Snapshot) ([]runtimev1.ArtifactInput, error) {
	if len(step.Spec.ArtifactFiles) == 0 {
		return nil, nil
	}
	out := make([]runtimev1.ArtifactInput, 0, len(step.Spec.ArtifactFiles))
	for _, alias := range sortedKeys(step.Spec.ArtifactFiles) {
		input, producer, output, ok := pipeline.ResolveReference(step.Spec.ArtifactFiles[alias])
		if !ok {
			return nil, Refusedf("artifact file %s is not a whole-scalar input or step-output reference", alias)
		}
		if input != "" {
			descriptor, ok := snap.InputArtifact(input)
			if !ok {
				return nil, Refusedf("artifact file %s references an unadmitted input %s", alias, input)
			}
			descriptor.Name = alias
			if err := descriptor.Validate(); err != nil {
				return nil, Refusedf("artifact file %s has an invalid descriptor", alias)
			}
			out = append(out, descriptor)
			continue
		}
		src := a.Step(producer)
		if src == nil || src.Status != StepSucceeded {
			return nil, Refusedf("artifact file %s depends on %s, which has not succeeded", alias, producer)
		}
		descriptor, ok := src.ArtifactRefs[output]
		if !ok {
			return nil, Refusedf("artifact file %s references an unavailable artifact output %s.%s", alias, producer, output)
		}
		descriptor.Name = alias
		if err := descriptor.Validate(); err != nil {
			return nil, Refusedf("artifact file %s has an invalid descriptor", alias)
		}
		out = append(out, descriptor)
	}
	return out, nil
}

func sortedKeys(in map[string]string) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// resolveInputArtifacts pins the verified file descriptor of each declared
// artifact input. Scalar inputs remain ordinary values; an artifact input is a
// run-level reference and is checked against current project ownership.
func (s *Service) resolveInputArtifacts(ctx context.Context, scope domain.Scope, doc pipeline.Document, snap Snapshot) (Snapshot, error) {
	for _, name := range sortedInputNames(doc) {
		decl := doc.Spec.Inputs[name]
		if decl.Type != pipeline.TypeArtifact {
			continue
		}
		value, ok := snap.Values()[name]
		if !ok || !value.IsString() || value.String() == "" {
			return Snapshot{}, Refusedf("artifact input %s is missing", name)
		}
		if s.d.Artifacts == nil {
			return Snapshot{}, Unavailablef(nil, "artifact input resolution is not configured")
		}
		ref, err := s.d.Artifacts.ResolveArtifact(ctx, scope, domain.ArtifactID(value.String()))
		if err != nil {
			return Snapshot{}, mapLookup(err, "artifact input")
		}
		if ref.Scope != scope || !ref.Usable || ref.Kind != runtimev1.KindFile || ref.Digest == "" {
			return Snapshot{}, Refusedf("artifact input %s is not a usable file artifact of this project", name)
		}
		input := runtimev1.ArtifactInput{Name: name, ArtifactID: string(ref.ID), Digest: ref.Digest}
		if err := input.Validate(); err != nil {
			return Snapshot{}, Refusedf("artifact input %s has an invalid descriptor", name)
		}
		snap.InputArtifacts = append(snap.InputArtifacts, input)
	}
	return snap, nil
}

func sortedInputNames(doc pipeline.Document) []string {
	out := make([]string, 0, len(doc.Spec.Inputs))
	for name := range doc.Spec.Inputs {
		out = append(out, name)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// authorizeStateBindings checks the caller's current project authority before a
// declared state binding is pinned. The fixed role model maps write authority
// to run management; a future evidence-read-only binding can narrow this seam
// without changing the stored snapshot.
func (s *Service) authorizeStateBindings(ctx context.Context, scope domain.Scope, actor Actor, doc pipeline.Document) error {
	if !doc.IsV1Alpha3() {
		return nil
	}
	for _, step := range doc.Spec.Steps {
		if len(step.State) == 0 {
			continue
		}
		if err := s.d.Authority.Authorize(ctx, actor, scope, PermRunManage); err != nil {
			return normalizeAuth(err)
		}
		return nil
	}
	return nil
}
