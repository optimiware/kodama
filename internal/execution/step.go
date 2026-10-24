package execution

import (
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// StepRun is the run-local progression of one pipeline step. Its Spec is the
// frozen step of the pinned definition.
type StepRun struct {
	RunID   domain.RunID
	ID      string
	Ordinal int
	Spec    pipeline.Step

	ActionKey string

	Status        StepStatus
	Reason        Reason
	NextAttemptAt *time.Time
	// GateRef names the gate record of a gate step (owned by the gate task).
	GateRef string

	AttemptsStarted int
	MaxAttempts     int
	BackoffSeconds  int64
	RetryOn         []pipeline.RetryClass

	// OutputCommit and Outputs are the finalized success outputs. They are
	// empty unless the step succeeded; a failed step exposes none.
	OutputCommit string
	Outputs      map[string]runtimev1.Value

	Version    domain.Version
	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt *time.Time

	dirty bool
}

// NewStepRun creates the pending run-local record of a definition step.
func NewStepRun(run domain.RunID, ordinal int, spec pipeline.Step, now time.Time) *StepRun {
	max := 1
	var backoff int64
	var on []pipeline.RetryClass
	if spec.Retry != nil {
		if spec.Retry.MaxAttempts > 0 {
			max = int(spec.Retry.MaxAttempts)
		}
		backoff = spec.Retry.BackoffSeconds
		on = append(on, spec.Retry.On...)
	}
	return &StepRun{
		RunID: run, ID: spec.ID, Ordinal: ordinal, Spec: spec, ActionKey: ActionKey(run, spec.ID),
		Status: StepPending, Reason: ReasonCreated, MaxAttempts: max, BackoffSeconds: backoff, RetryOn: on,
		Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
}

func (s *StepRun) touch(now time.Time) {
	if !s.dirty {
		s.Version++
		s.dirty = true
	}
	s.UpdatedAt = now.UTC()
}

// Dirty / MarkClean support persistence adapters.
func (s *StepRun) Dirty() bool { return s.dirty }
func (s *StepRun) MarkClean()  { s.dirty = false }

// Retryable reports whether the step's policy allows another attempt after a
// failure of the given class (budget and class only; replay safety is a
// separate, evidence-based question).
func (s *StepRun) Retryable(class FailureClass) bool {
	if s.AttemptsStarted >= s.MaxAttempts {
		return false
	}
	for _, c := range s.RetryOn {
		if string(c) == string(class) && (class == ClassInfrastructure || class == ClassTransient) {
			return true
		}
	}
	return false
}

// Due reports whether a pending step's backoff has elapsed.
func (s *StepRun) Due(now time.Time) bool {
	return s.NextAttemptAt == nil || !s.NextAttemptAt.After(now)
}

func (s *StepRun) start(now time.Time) error {
	if s.Status != StepPending {
		return transitionf("step %s is %s, not pending", s.ID, s.Status)
	}
	if !s.Due(now) {
		return transitionf("step %s is waiting for its backoff", s.ID)
	}
	if s.AttemptsStarted >= s.MaxAttempts {
		return transitionf("step %s has no attempts left", s.ID)
	}
	s.Status, s.Reason, s.NextAttemptAt = StepRunning, ReasonRunning, nil
	s.AttemptsStarted++
	s.touch(now)
	return nil
}

func (s *StepRun) succeed(commit string, outputs map[string]runtimev1.Value, now time.Time) error {
	if s.Status != StepRunning {
		return transitionf("step %s is %s, not running", s.ID, s.Status)
	}
	s.Status, s.Reason, s.OutputCommit = StepSucceeded, ReasonCompleted, commit
	s.Outputs = make(map[string]runtimev1.Value, len(outputs))
	for k, v := range outputs {
		s.Outputs[k] = v
	}
	t := now.UTC()
	s.FinishedAt = &t
	s.touch(now)
	return nil
}

func (s *StepRun) fail(reason Reason, now time.Time) error {
	if s.Status != StepRunning && s.Status != StepPending {
		return transitionf("step %s is %s and cannot fail", s.ID, s.Status)
	}
	s.Status, s.Reason, s.NextAttemptAt = StepFailed, reason, nil
	t := now.UTC()
	s.FinishedAt = &t
	s.touch(now)
	return nil
}

func (s *StepRun) skip(reason Reason, now time.Time) error {
	if s.Status != StepPending {
		return transitionf("step %s is %s, only a pending step is skipped", s.ID, s.Status)
	}
	s.Status, s.Reason = StepSkipped, reason
	t := now.UTC()
	s.FinishedAt = &t
	s.touch(now)
	return nil
}

func (s *StepRun) cancel(reason Reason, now time.Time) error {
	if s.Status != StepPending && s.Status != StepRunning {
		return transitionf("step %s is %s and cannot be cancelled", s.ID, s.Status)
	}
	s.Status, s.Reason, s.NextAttemptAt = StepCancelled, reason, nil
	t := now.UTC()
	s.FinishedAt = &t
	s.touch(now)
	return nil
}

// scheduleRetry returns a running step to pending with a persisted backoff.
func (s *StepRun) scheduleRetry(now time.Time) error {
	if s.Status != StepRunning {
		return transitionf("step %s is %s, not running", s.ID, s.Status)
	}
	if s.AttemptsStarted >= s.MaxAttempts {
		return transitionf("step %s has no attempts left", s.ID)
	}
	next := now.UTC().Add(time.Duration(s.BackoffSeconds) * time.Second)
	s.Status, s.Reason, s.NextAttemptAt = StepPending, ReasonBackoff, &next
	s.touch(now)
	return nil
}
