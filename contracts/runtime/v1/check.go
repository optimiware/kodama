package runtimev1

import (
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// ArtifactRef is trusted artifact metadata the server (or a test) supplies to
// CheckResult. This package never reaches storage itself.
type ArtifactRef struct {
	ID      domain.ArtifactID
	Scope   domain.Scope
	Attempt domain.AttemptID
	Kind    ArtifactKind
	Digest  Digest
	Usable  bool // uploaded, verified and marked usable (or a verified OCI registration)
}

// CheckResult applies the rules a schema cannot: the result against the
// attempt's context and trusted artifact metadata. It checks structure, the
// writer/reader source rule, exact declared outputs and their types, and that
// every referenced artifact is in this attempt's scope and usable. A missing
// or foreign artifact is indistinguishable (not_found); an unfinished one is
// artifact_incomplete. It does not check current authorization, Git
// publication or storage state; those belong to the consuming module.
func CheckResult(r Result, ctx Context, artifacts map[domain.ArtifactID]ArtifactRef) error {
	if err := r.Validate(); err != nil {
		return &ContractError{Code: domain.CodeInvalidRequest, Message: err.Error()}
	}
	if r.Outcome == OutcomeCompleted {
		if (ctx.Workspace.Access == AccessWrite) != (r.SourceCommit != nil) {
			return &ContractError{Code: CodeOutputInvalid, Message: "a completed writer must report sourceCommit and a nonwriter must not"}
		}
		if err := checkOutputs(r, ctx, artifacts); err != nil {
			return err
		}
	}
	for _, id := range r.DiagnosticArtifactIDs {
		if err := checkArtifact(id, ctx, artifacts, nil); err != nil {
			return err
		}
	}
	return nil
}

func checkOutputs(r Result, ctx Context, artifacts map[domain.ArtifactID]ArtifactRef) error {
	declared := map[string]OutputType{}
	for _, o := range ctx.Outputs {
		declared[o.Name] = o.Type
	}
	for name := range r.Outputs {
		if _, ok := declared[name]; !ok {
			return outputError("undeclared output %q", name)
		}
	}
	for _, o := range ctx.Outputs {
		v, ok := r.Outputs[o.Name]
		if !ok {
			return outputError("missing declared output %q", o.Name)
		}
		var typeOK bool
		var want ArtifactKind
		switch o.Type {
		case OutputString:
			typeOK = v.IsString()
		case OutputBoolean:
			typeOK = v.IsBool()
		case OutputInteger:
			typeOK = v.IsInteger()
		case OutputArtifact:
			typeOK, want = v.IsString(), KindFile
		case OutputOCIArtifact:
			typeOK, want = v.IsString(), KindOCI
		}
		if !typeOK {
			return outputError("output %q must be of type %s", o.Name, o.Type)
		}
		if want != "" {
			id := domain.ArtifactID(v.String())
			if err := id.Validate(); err != nil {
				return outputError("output %q: %v", o.Name, err)
			}
			if err := checkArtifact(id, ctx, artifacts, &want); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkArtifact(id domain.ArtifactID, ctx Context, artifacts map[domain.ArtifactID]ArtifactRef, kind *ArtifactKind) error {
	ref, ok := artifacts[id]
	if !ok || ref.Scope != ctx.Scope() || ref.Attempt != ctx.AttemptID {
		return &ContractError{Code: domain.CodeNotFound, Message: "artifact not found"}
	}
	if kind != nil && ref.Kind != *kind {
		return outputError("artifact %q has the wrong kind", id)
	}
	if !ref.Usable {
		return &ContractError{Code: CodeArtifactIncomplete, Message: "artifact upload is not complete"}
	}
	return nil
}

func outputError(format string, args ...any) error {
	return &ContractError{Code: CodeOutputInvalid, Message: fmt.Sprintf(format, args...)}
}
