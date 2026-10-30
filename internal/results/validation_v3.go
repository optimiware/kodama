package results

import (
	"errors"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// projectV1 is the v1 view of a v3 result and its context the shared artifact
// rules apply to: the declared outputs, the diagnostics and the attempt's scope.
// It has no source (the source rules of the wire are applied separately).
func projectV1(res runtimev3.ResultV3, pinned runtimev3.ContextV3) (runtimev1.Result, runtimev1.Context) {
	return runtimev1.Result{
		Outcome: res.Outcome, ExitCode: res.ExitCode, Reason: res.Reason, Question: res.Question,
		Outputs: res.Outputs, DiagnosticArtifactIDs: res.DiagnosticArtifactIDs,
	}, runtimev1.Context{
		TenantID: pinned.TenantID, ProjectID: pinned.ProjectID, AttemptID: pinned.AttemptID, Outputs: pinned.Outputs,
		Workspace: runtimev1.Workspace{Access: runtimev1.AccessNone},
	}
}

// ReferencedArtifactsV3 lists the artifact IDs a v3 result can legitimately
// reference (see ReferencedArtifacts).
func ReferencedArtifactsV3(res runtimev3.ResultV3, pinned runtimev3.ContextV3) []domain.ArtifactID {
	r, c := projectV1(res, pinned)
	return ReferencedArtifacts(r, c)
}

// trustedOutputV3 reports whether the artifact has the output role and a producer
// provenance that is the attempt's pinned input: no source (a file), the digest
// of the pinned source set, or one repository of it at its pinned commit. The
// commits a writer newly pushed are never the pinned input.
func trustedOutputV3(ev ArtifactEvidence, pinned runtimev3.ContextV3) bool {
	if ev.Role != runtimev1.RoleOutput {
		return false
	}
	p := ev.Provenance
	if p.IsZero() {
		return ev.Kind == runtimev1.KindFile
	}
	if p.SourceSet != "" {
		return p.RepositoryURL == "" && p.Commit == "" && pinned.Workspace.SourceSetDigest != nil && p.SourceSet == string(*pinned.Workspace.SourceSetDigest)
	}
	for _, s := range pinned.Workspace.Sources {
		if p.RepositoryURL == s.RepositoryURL && p.Commit == s.Commit {
			return true
		}
	}
	return false
}

// ValidateV3 applies the rules only this module can on the source-set wire: the
// artifact owner's evidence against the attempt's pinned context (the same rules
// and precedence as Validate, with the pinned source set as the trusted input) and
// then the contract's check of the declared outputs and of every commit output
// against the grants the attempt may hold (runtimev3.CheckResult: the attempt's
// own grant as the context records it, or a runtime-issued grant from issued, the
// control plane's own records). A violation of the declared outputs or of the
// grant coverage is a Verdict with OutputViolation: execution records the durable
// failure and exposes no output. Whether each commit output's push record exists,
// was applied and was made by this attempt is CheckPush's, not this function's.
func ValidateV3(res runtimev3.ResultV3, pinned runtimev3.ContextV3, evidence map[domain.ArtifactID]ArtifactEvidence, issued []gitv1.GrantClaims) (Verdict, error) {
	if err := res.Validate(); err != nil {
		return Verdict{}, contractError(err)
	}
	r, c := projectV1(res, pinned)
	return validate(r, c, evidence, validator{
		trusted: func(ev ArtifactEvidence) bool { return trustedOutputV3(ev, pinned) },
		check: func(artifacts map[domain.ArtifactID]runtimev1.ArtifactRef) error {
			return runtimev3.CheckResult(res, pinned, artifacts, issued...)
		},
	})
}

// PushVerdict is what one push record says about a commit output.
type PushVerdict int

const (
	// PushApplied: the record is this attempt's, the entry for the output's ref
	// moved it to the output's commit and its outcome is applied. The output counts.
	PushApplied PushVerdict = iota
	// PushPending: the entry is authorized, committing or unknown. Its outcome is
	// not known yet (a lost confirmation waits for reconciliation, WS-A15): the
	// result is not accepted yet and the caller repeats it.
	PushPending
	// PushRefused: there is no such record, or it is another actor's or
	// repository's, or no entry moved the ref to the commit, or the entry was
	// refused. The output can never count.
	PushRefused
)

func (v PushVerdict) String() string {
	switch v {
	case PushApplied:
		return "applied"
	case PushPending:
		return "pending"
	}
	return "refused"
}

// CheckPush decides one commit output against the push record it names, for the
// attempt of a run. It is a pure function of the record: the record must be the
// one the output names (push ID and repository), made by this attempt's workload
// in this run (the actor the Git service recorded from the verified grant, never a
// person and never another attempt), and have exactly one entry for the output's
// ref whose new value is the output's commit; the entry's state then decides.
func CheckPush(out runtimev3.CommitOutput, rec gitv1.PushRecord, run domain.RunID, attempt domain.AttemptID) PushVerdict {
	a := rec.Actor
	if rec.PushID != out.PushID || rec.RepositoryID != out.RepositoryID {
		return PushRefused
	}
	if a.Type != gitv1.ActorWorkload || a.AttemptID == nil || *a.AttemptID != string(attempt) || a.RunID == nil || *a.RunID != string(run) {
		return PushRefused
	}
	if rec.OriginRunID != nil && *rec.OriginRunID != string(run) {
		return PushRefused
	}
	var entry *gitv1.PushEntry
	for i := range rec.Entries {
		if rec.Entries[i].Ref != out.Ref {
			continue
		}
		if entry != nil {
			return PushRefused // a record updates a ref once; two entries are not evidence
		}
		entry = &rec.Entries[i]
	}
	if entry == nil || entry.New != string(out.Commit) || gitv1.IsZeroObjectID(entry.New) {
		return PushRefused
	}
	switch entry.State {
	case gitv1.EntryApplied:
		return PushApplied
	case gitv1.EntryRefused:
		return PushRefused
	}
	return PushPending // authorized, committing, unknown (and any state this version does not know)
}

// VerifyCommits decides every commit output of a result against its push record
// (lookup returns the record the output names, ErrNotFound for none). It returns
// the first refusal as ErrOutputInvalid, else a retryable ErrUnavailable when any
// output is still pending, else nil. A refusal outranks a pending output: a result
// with an output that can never count is not made to wait for the others. A
// lookup that cannot answer is the lookup's own error (unavailable).
func VerifyCommits(res runtimev3.ResultV3, run domain.RunID, attempt domain.AttemptID, lookup func(runtimev3.CommitOutput) (gitv1.PushRecord, error)) error {
	pending := false
	for _, out := range res.Commits {
		rec, err := lookup(out)
		switch {
		case err == nil:
		case errors.Is(err, ErrNotFound):
			return Fail(ErrOutputInvalid, nil, "a commit output names no push record of this attempt")
		default:
			return err
		}
		switch CheckPush(out, rec, run, attempt) {
		case PushRefused:
			return Fail(ErrOutputInvalid, nil, "a commit output is not an applied push of this attempt")
		case PushPending:
			pending = true
		}
	}
	if pending {
		return Fail(ErrUnavailable, nil, "a push of the result is not settled yet; repeat the result")
	}
	return nil
}
