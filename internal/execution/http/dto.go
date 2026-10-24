package executionhttp

import (
	"sort"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// ScalarValue is the wire form of a string, boolean or integer: the runtime
// contract's strict scalar (integers never pass through floating point).
type ScalarValue = runtimev1.Value

func tp(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func sp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// runDTO renders a run with its pinned snapshot. Secrets appear as references and
// exact versions only.
func runDTO(r *execution.Run) Run {
	inputs := make([]PinnedInput, 0, len(r.Snapshot.Inputs))
	for _, in := range r.Snapshot.Inputs {
		p := PinnedInput{Name: in.Name, Type: PinnedInputType(in.Type), Value: in.Value, Source: PinnedInputSource(in.Source), VariableId: sp(in.VariableID)}
		if in.VariableVersion > 0 {
			v := in.VariableVersion
			p.VariableVersion = &v
		}
		inputs = append(inputs, p)
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Name < inputs[j].Name })
	secrets := make([]PinnedSecret, 0, len(r.Snapshot.Secrets))
	for _, s := range r.Snapshot.Secrets {
		secrets = append(secrets, PinnedSecret{Step: s.Step, Ref: s.Binding.Ref, Version: s.Binding.Version, Name: s.Binding.Name, Delivery: PinnedSecretDelivery(s.Binding.Delivery)})
	}
	out := Run{
		Id: httpID(r.ID), Purpose: RunPurpose(r.Purpose), DefinitionId: httpID(r.DefinitionID), DefinitionDigest: r.DefinitionDigest,
		SourceCommit: sp(r.SourceCommit), WorkItemId: sp(string(r.WorkItemID)), Status: RunStatus(r.Status), Reason: string(r.Reason),
		CancelRequestedAt: tp(r.CancelRequestedAt), InputSnapshot: InputSnapshot{Digest: r.SnapshotDigest, Inputs: inputs, Secrets: secrets},
		RequestedBy: Actor{Id: r.RequestedBy.ID, Type: ActorType(r.RequestedBy.Type)}, SourceRetention: RunSourceRetention(r.SourceRetention),
		Version: int64(r.Version), CreatedAt: r.CreatedAt.UTC(), StartedAt: tp(r.StartedAt), FinishedAt: tp(r.FinishedAt),
	}
	if r.ContentRevision != 0 {
		v := int64(r.ContentRevision)
		out.ContentRevision = &v
	}
	if r.FailureIntentAt != nil {
		reason := string(r.FailureReason)
		out.FailureReason = &reason
	}
	return out
}

func httpID[T ~string](v T) string { return string(v) }

// stepDTO renders a step with its attempts. A step exposes success outputs only
// when it succeeded; a failed attempt exposes its diagnostic artifact references.
func stepDTO(agg *execution.Aggregate, s *execution.StepRun) Step {
	attempts := make([]Attempt, 0)
	for _, at := range agg.AttemptsOf(s.ID) {
		attempts = append(attempts, attemptDTO(agg, at))
	}
	out := Step{
		Id: s.ID, Kind: StepKind(s.Spec.Kind), Status: StepStatus(s.Status), Reason: string(s.Reason), NextAttemptAt: tp(s.NextAttemptAt),
		ActionKey: s.ActionKey, AttemptsStarted: s.AttemptsStarted, MaxAttempts: s.MaxAttempts, Attempts: attempts,
	}
	if s.Status == execution.StepSucceeded {
		m := make(map[string]ScalarValue, len(s.Outputs))
		for k, v := range s.Outputs {
			m[k] = v
		}
		out.Outputs = &m
		out.SourceCommit = sp(s.OutputCommit)
	}
	return out
}

func attemptDTO(agg *execution.Aggregate, at *execution.Attempt) Attempt {
	c := agg.Control(at.ID)
	diag := []string{}
	if at.Result != nil {
		for _, id := range at.Result.DiagnosticArtifactIDs {
			diag = append(diag, string(id))
		}
	}
	a := Attempt{
		Id: string(at.ID), Ordinal: at.Ordinal, Status: AttemptStatus(at.Status), Reason: string(at.Reason), JobName: at.WorkloadName,
		ObservedJobUid: sp(at.WorkloadUID), Access: AttemptAccess(at.Access), InputCommit: sp(at.InputCommit),
		StartupDeadlineAt: at.StartupDeadline.UTC(), ExecutionDeadlineAt: tp(at.ExecutionDeadline), StartedAt: tp(at.StartedAt),
		FinishedAt: tp(at.FinishedAt), ResultHash: sp(at.ResultHash), DiagnosticArtifactIds: diag, Settled: c.Settled(), CreatedAt: at.CreatedAt.UTC(),
	}
	if at.Class != "" {
		fc := AttemptFailureClass(at.Class)
		a.FailureClass = &fc
	}
	return a
}

var _ = domain.RunID("")
