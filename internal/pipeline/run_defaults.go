package pipeline

// Source-dependent defaults of the run_shared model. Whether a run has source
// is a property of the run, not the definition, so none of this is part of a
// definition's canonical form or digest: run admission resolves the effective
// access of every step with EffectiveAccess and records it in the run snapshot.

// EffectiveAccess resolves the workspace access of one step of a kodama/v1alpha2
// document for a run, applying the source-backed or source-free table:
//
//	workspace absent         read (source-backed)  | none (source-free)
//	workspace {access: none} refused               | none
//	workspace {read|write}   as declared           | refused
//	agent step               as above              | refused
//
// A gate has no workspace and always resolves to none. refusal is empty when the
// step is allowed and otherwise one of the Admission* codes; it never carries
// document content.
func EffectiveAccess(s Step, sourceBacked bool) (access Access, refusal string) {
	if s.Kind == KindGate {
		return AccessNone, ""
	}
	if s.Kind == KindAgent && !sourceBacked {
		return "", AdmissionAgentSourceFree
	}
	if s.Workspace == nil {
		if sourceBacked {
			return AccessRead, ""
		}
		return AccessNone, ""
	}
	switch s.Workspace.Access {
	case AccessNone:
		if sourceBacked {
			return "", AdmissionWorkspaceNoneSourceBacked
		}
		return AccessNone, ""
	case AccessRead, AccessWrite:
		if !sourceBacked {
			return "", AdmissionWorkspaceAccessSourceFree
		}
		return s.Workspace.Access, ""
	}
	return "", AdmissionWorkspaceAccessSourceFree // unreachable for a validated document
}

// EffectiveArgv applies the contract's argv matrix to the pinned image's
// defaults. A nil command is "absent" (meaningful); a nil or empty args is
// "absent" (identical meaning):
//
//	command absent,  args absent/[]  -> image ENTRYPOINT then image CMD
//	command absent,  args non-empty  -> image ENTRYPOINT then args (CMD replaced)
//	command present, args absent/[]  -> command only
//	command present, args non-empty  -> command then args
//
// The result may be empty when the image declares neither ENTRYPOINT nor CMD
// and the document no command; execution then fails startup, not validation,
// because validation never reads images.
func EffectiveArgv(imageEntrypoint, imageCmd, command, args []string) []string {
	out := []string{}
	switch {
	case command == nil && len(args) == 0:
		out = append(out, imageEntrypoint...)
		out = append(out, imageCmd...)
	case command == nil:
		out = append(out, imageEntrypoint...)
		out = append(out, args...)
	default:
		out = append(out, command...)
		out = append(out, args...)
	}
	return out
}
