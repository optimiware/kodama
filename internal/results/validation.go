package results

import (
	"errors"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Verdict is the outcome of validating a result against its pinned context and
// the artifact owner's evidence.
type Verdict struct {
	// Artifacts is the trusted metadata execution re-checks. A reference whose
	// kind, role or provenance is not trusted carries an empty kind, so
	// execution's own check classifies it as a wrong output.
	Artifacts map[domain.ArtifactID]runtimev1.ArtifactRef
	// Protect lists the verified artifacts the result consumes.
	Protect []domain.ArtifactID
	// OutputViolation is set when a completed result violates its declared
	// outputs. It is not a request error: execution records it as a durable
	// validation failure that exposes no success outputs.
	OutputViolation error
}

// outputArtifacts lists the artifact IDs a completed result uses as the values
// of declared artifact-typed outputs, independently of diagnostics.
func outputArtifacts(res runtimev1.Result, pinned runtimev1.Context) map[domain.ArtifactID]bool {
	seen := map[domain.ArtifactID]bool{}
	if res.Outcome != runtimev1.OutcomeCompleted {
		return seen
	}
	for _, o := range pinned.Outputs {
		if o.Type != runtimev1.OutputArtifact && o.Type != runtimev1.OutputOCIArtifact {
			continue
		}
		v, ok := res.Outputs[o.Name]
		if !ok || !v.IsString() {
			continue
		}
		if id := domain.ArtifactID(v.String()); id.Validate() == nil {
			seen[id] = true
		}
	}
	return seen
}

// ReferencedArtifacts lists, sorted and without duplicates, the artifact IDs a
// result can legitimately reference: values of declared artifact-typed outputs
// and diagnostic artifacts. A string output that is not artifact-typed is never
// an artifact ID, however much it looks like one.
func ReferencedArtifacts(res runtimev1.Result, pinned runtimev1.Context) []domain.ArtifactID {
	seen := map[domain.ArtifactID]bool{}
	for id := range outputArtifacts(res, pinned) {
		seen[id] = true
	}
	for _, id := range res.DiagnosticArtifactIDs {
		seen[id] = true
	}
	out := make([]domain.ArtifactID, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Validate applies the rules only this module can: the artifact owner's
// evidence against the attempt's pinned context. It returns, in this order of
// precedence:
//
//  1. ErrNotFound for any referenced artifact that is absent, foreign to the
//     attempt's tenant, project or attempt, or removed (indistinguishable);
//  2. ErrArtifactIncomplete for a referenced artifact that is not usable yet;
//  3. ErrOutputInvalid for a diagnostic that is not a file diagnostic, or for
//     any contract violation of a failed or needs-input result;
//
// and otherwise a Verdict. A completed result whose declared outputs are
// missing, mistyped or untrusted yields a Verdict with OutputViolation, so
// execution can record the durable failure. Diagnostics are verified
// independently of outputs: an early output violation never lets an
// unverified diagnostic reference reach execution.
func Validate(res runtimev1.Result, pinned runtimev1.Context, evidence map[domain.ArtifactID]ArtifactEvidence) (Verdict, error) {
	if err := res.Validate(); err != nil {
		return Verdict{}, contractError(err)
	}
	scope := pinned.Scope()
	own := func(id domain.ArtifactID) (ArtifactEvidence, error) {
		ev, ok := evidence[id]
		if !ok || ev.ID != id || ev.Scope != scope || ev.Attempt != pinned.AttemptID {
			return ArtifactEvidence{}, Fail(ErrNotFound, nil, "artifact not found")
		}
		return ev, nil
	}

	// Existence and completeness first, for every reference, so that a foreign
	// or unfinished reference is reported precisely whatever else is wrong.
	ids := ReferencedArtifacts(res, pinned)
	for _, id := range ids {
		ev, err := own(id)
		if err != nil {
			return Verdict{}, err
		}
		if !ev.Usable {
			return Verdict{}, Fail(ErrArtifactIncomplete, nil, "artifact upload is not complete")
		}
	}

	v := Verdict{Artifacts: map[domain.ArtifactID]runtimev1.ArtifactRef{}}
	isDiagnostic := map[domain.ArtifactID]bool{}
	for _, id := range res.DiagnosticArtifactIDs {
		isDiagnostic[id] = true
		ev, _ := own(id)
		if ev.Kind != runtimev1.KindFile || ev.Role != runtimev1.RoleDiagnostic {
			return Verdict{}, Fail(ErrOutputInvalid, nil, "a diagnostic must be a file artifact with the diagnostic role")
		}
	}
	isOutput := outputArtifacts(res, pinned)
	source := pinnedSource(pinned)
	for _, id := range ids {
		ev, _ := own(id)
		ref := runtimev1.ArtifactRef{ID: id, Scope: ev.Scope, Attempt: ev.Attempt, Kind: ev.Kind, Usable: ev.Usable}
		if isOutput[id] && !trustedOutput(ev, source) {
			// Not a trusted output, even when it is also a valid diagnostic:
			// execution's own kind check then rejects the output while the
			// diagnostic (which has no kind requirement) stays valid.
			ref.Kind = ""
			if v.OutputViolation == nil {
				v.OutputViolation = Fail(ErrOutputInvalid, nil, "an output artifact is not a trusted output of this attempt")
			}
		}
		v.Artifacts[id] = ref
	}

	if err := runtimev1.CheckResult(res, pinned, v.Artifacts); err != nil {
		var ce *runtimev1.ContractError
		if errors.As(err, &ce) && ce.Code == runtimev1.CodeOutputInvalid && res.Outcome == runtimev1.OutcomeCompleted {
			if v.OutputViolation == nil {
				v.OutputViolation = Fail(ErrOutputInvalid, err, "the result does not satisfy the declared outputs")
			}
		} else {
			return Verdict{}, contractError(err)
		}
	}
	for _, id := range ids {
		// Outputs of a violating result are never exposed, so they need no
		// protection; diagnostics are retained either way.
		if v.OutputViolation == nil || isDiagnostic[id] {
			v.Protect = append(v.Protect, id)
		}
	}
	return v, nil
}

// pinnedSource is the source the attempt's context pins, or the zero value for
// a step without a workspace. Provenance is compared with this input source,
// never with the commit a writer newly proposes.
func pinnedSource(c runtimev1.Context) Provenance {
	if c.Workspace.Source == nil {
		return Provenance{}
	}
	return Provenance{RepositoryURL: c.Workspace.Source.RepositoryURL, Commit: string(c.Workspace.Source.Commit)}
}

// trustedOutput reports whether the artifact has the output role and a
// producer provenance that is the attempt's pinned input source (or none, for
// a file produced without source).
func trustedOutput(ev ArtifactEvidence, pinned Provenance) bool {
	if ev.Role != runtimev1.RoleOutput {
		return false
	}
	if ev.Provenance.IsZero() {
		return ev.Kind == runtimev1.KindFile
	}
	return ev.Provenance == pinned
}

// FromContract maps a runtime contract violation (a *runtimev1.ContractError
// from strict decoding or validation) to this module's failure classes. The
// transport uses it so a hash mismatch or output violation found while decoding
// keeps its specific code.
func FromContract(err error) error { return contractError(err) }

// contractError maps a runtime contract violation to this module's classes.
func contractError(err error) error {
	var ce *runtimev1.ContractError
	if !errors.As(err, &ce) {
		return Fail(ErrInvalidRequest, err, "the request is not valid for this operation")
	}
	switch ce.Code {
	case domain.CodeNotFound:
		return Fail(ErrNotFound, err, "artifact not found")
	case runtimev1.CodeArtifactIncomplete:
		return Fail(ErrArtifactIncomplete, err, "artifact upload is not complete")
	case runtimev1.CodeOutputInvalid:
		return Fail(ErrOutputInvalid, err, "the result does not satisfy the declared outputs")
	case runtimev1.CodeResultHashMismatch:
		return Fail(ErrResultHashMismatch, err, "resultHash does not match the canonical result")
	}
	return Fail(ErrInvalidRequest, err, "the request is not valid for this operation")
}
