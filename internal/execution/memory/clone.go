package memory

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
)

// memdb stores and returns values as given and never deep-copies, so every row
// crosses the adapter boundary through one of the functions below, in both
// directions: what is stored shares nothing with what a caller passed in, and
// what a caller reads shares nothing with what is stored. A mutation can then
// change only its own copy: a failed or abandoned update leaves the committed
// pins, outputs and versions untouched, as a database does.

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneRun(r execution.Run) execution.Run {
	if r.Specification != nil {
		s := *r.Specification
		s.AcceptanceCriteria = slices.Clone(s.AcceptanceCriteria)
		r.Specification = &s
	}
	r.Snapshot.Inputs = slices.Clone(r.Snapshot.Inputs)
	r.Snapshot.Secrets = slices.Clone(r.Snapshot.Secrets)
	r.Snapshot.Environments = slices.Clone(r.Snapshot.Environments)
	if r.Snapshot.Runtime != nil {
		b := *r.Snapshot.Runtime
		r.Snapshot.Runtime = &b
	}
	r.CancelRequestedAt, r.FailureIntentAt = cloneTime(r.CancelRequestedAt), cloneTime(r.FailureIntentAt)
	r.StartedAt, r.FinishedAt = cloneTime(r.StartedAt), cloneTime(r.FinishedAt)
	return r
}

// cloneStep copies a step run. The frozen step definition is copied through
// its stored wire form, exactly as the PostgreSQL adapter persists it.
func cloneStep(s execution.StepRun) (execution.StepRun, error) {
	b, err := execution.EncodeStep(s.Spec)
	if err != nil {
		return execution.StepRun{}, err
	}
	if s.Spec, err = execution.DecodeStep(b); err != nil {
		return execution.StepRun{}, err
	}
	s.NextAttemptAt, s.FinishedAt = cloneTime(s.NextAttemptAt), cloneTime(s.FinishedAt)
	s.RetryOn = slices.Clone(s.RetryOn)
	s.Outputs = maps.Clone(s.Outputs)
	return s, nil
}

func cloneResult(r *runtimev1.Result) *runtimev1.Result {
	if r == nil {
		return nil
	}
	c := *r
	if r.ExitCode != nil {
		v := *r.ExitCode
		c.ExitCode = &v
	}
	if r.Reason != nil {
		v := *r.Reason
		c.Reason = &v
	}
	if r.Question != nil {
		v := *r.Question
		c.Question = &v
	}
	if r.SourceCommit != nil {
		v := *r.SourceCommit
		c.SourceCommit = &v
	}
	c.Outputs = maps.Clone(r.Outputs)
	c.DiagnosticArtifactIDs = slices.Clone(r.DiagnosticArtifactIDs)
	return &c
}

func cloneAttempt(a execution.Attempt) execution.Attempt {
	a.Context = slices.Clone(a.Context)
	a.ExecutionDeadline, a.StartedAt, a.FinishedAt = cloneTime(a.ExecutionDeadline), cloneTime(a.StartedAt), cloneTime(a.FinishedAt)
	a.Result = cloneResult(a.Result)
	return a
}

func cloneControl(c execution.Control) execution.Control {
	c.DispatchConfirmedAt, c.ClaimUntil, c.RevokedAt = cloneTime(c.DispatchConfirmedAt), cloneTime(c.ClaimUntil), cloneTime(c.RevokedAt)
	c.TerminationRequestedAt, c.TerminationConfirmedAt = cloneTime(c.TerminationRequestedAt), cloneTime(c.TerminationConfirmedAt)
	c.ReconciledAt, c.SettledAt, c.StepResolvedAt = cloneTime(c.ReconciledAt), cloneTime(c.SettledAt), cloneTime(c.StepResolvedAt)
	return c
}

// cloneDetail deep-copies a JSON-compatible evidence or audit payload through
// its serialized form, the same boundary the PostgreSQL adapter stores it
// across (jsonb): nested maps and slices are never shared, and a value that
// cannot be serialized is refused on write as it would be there. Numbers read
// back as float64, as they do from jsonb.
func cloneDetail(d map[string]any) (map[string]any, error) {
	if len(d) == 0 {
		return map[string]any{}, nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("%w: the payload is not serializable", execution.ErrInvalid)
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%w: the payload is not an object", execution.ErrInvalid)
	}
	return out, nil
}
