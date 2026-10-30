package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ReserveWriterInput identifies one writer admission.
type ReserveWriterInput struct {
	Scope       domain.Scope
	WorkspaceID ID
	AttemptID   domain.AttemptID
	// InputCommit is the commit the attempt checks out; empty selects the
	// accepted commit.
	InputCommit Commit
	// ExpectedTip is the freshly observed branch tip (the accepted commit).
	ExpectedTip Commit
	Actor       Actor
}

// ReserveWriterMutation returns the pure reservation mutation. It is shared by
// the service and the PostgreSQL transaction-bound entry point used when the
// reservation joins attempt creation, so both apply identical rules.
func ReserveWriterMutation(in ReserveWriterInput, now func() time.Time) Mutation {
	return func(a *Aggregate) (Effects, error) {
		if err := a.Workspace.requireExternal("the publisher writer reservation"); err != nil {
			return Effects{}, err
		}
		input := in.InputCommit
		if input == "" {
			input = in.ExpectedTip
		}
		had := a.Workspace.Writer != nil
		if err := a.Workspace.ReserveWriter(in.AttemptID, input, in.ExpectedTip, now()); err != nil {
			return Effects{}, err
		}
		if had { // idempotent repeat
			return Effects{}, nil
		}
		return Effects{Audit: []Audit{audit(in.Actor, "workspace.writer_reserved", "", map[string]any{
			"attemptId": string(in.AttemptID), "inputCommit": string(input), "expectedTip": string(in.ExpectedTip)})}}, nil
	}
}

// SettleWriterInput identifies the end of a writer attempt.
type SettleWriterInput struct {
	Scope       domain.Scope
	WorkspaceID ID
	AttemptID   domain.AttemptID
	Outcome     WriterOutcome
	// SourceCommit is the commit the attempt reported; it is verified for a
	// successful outcome.
	SourceCommit Commit
	Actor        Actor
}

// SettleWriterMutation releases the slot with the attempt's outcome. Load must
// request the attempt's receipt.
func SettleWriterMutation(in SettleWriterInput, now func() time.Time) Mutation {
	return func(a *Aggregate) (Effects, error) {
		// An internally hosted writer's commits are accepted one by one from
		// their push records (AcceptCheckpoint), so there is no single published
		// commit to verify here.
		if in.Outcome == OutcomeSucceeded && in.SourceCommit != "" && !a.Workspace.Internal() {
			if err := a.Workspace.VerifyWriterSource(in.AttemptID, in.SourceCommit, a.Receipt); err != nil {
				return Effects{}, err
			}
		}
		if err := a.Workspace.SettleWriter(in.AttemptID, in.Outcome, a.Receipt, now()); err != nil {
			return Effects{}, err
		}
		fx := Effects{Audit: []Audit{audit(in.Actor, "workspace.writer_settled", string(in.Outcome), map[string]any{"attemptId": string(in.AttemptID)})}}
		if a.Receipt != nil {
			fx.Receipt = a.Receipt
		}
		return fx, nil
	}
}

// ReserveWriter admits one writer: it refreshes the remote tip, then reserves
// the single slot against the current workspace version and the attempt's
// current authority. A human push after this point is protected by the
// expected-tip compare-and-swap at publication.
func (s *Service) ReserveWriter(ctx context.Context, actor Actor, in ReserveWriterInput) (*Workspace, error) {
	if err := firstErr(in.WorkspaceID.Validate(), in.AttemptID.Validate()); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionAdmit, in.Scope, Resource{"workspace", string(in.WorkspaceID)}); err != nil {
		return nil, err
	}
	if err := s.checkWriterAuthority(ctx, in.Scope, in.WorkspaceID, in.AttemptID); err != nil {
		return nil, err
	}
	in.Actor = actor
	var out *Workspace
	err := retry(3, func() error {
		w, err := s.observe(ctx, actor, in.Scope, in.WorkspaceID, 0)
		if err != nil {
			if w == nil {
				return err
			}
			return fmt.Errorf("refusing admission: %w", err)
		}
		in.ExpectedTip = w.AcceptedCommit
		agg, err := s.d.Store.Update(ctx, in.Scope, in.WorkspaceID, Load{}, ReserveWriterMutation(in, s.now))
		if err != nil {
			return err
		}
		out = agg.Workspace
		return nil
	})
	return out, err
}

func (s *Service) checkWriterAuthority(ctx context.Context, scope domain.Scope, ws ID, attempt domain.AttemptID) error {
	auth, err := s.d.Attempts.Authority(ctx, scope, attempt)
	switch {
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	case err != nil:
		return unavailable("attempt authority", err)
	case auth.WorkspaceID != ws:
		return ErrNotFound
	case !auth.Live:
		return ErrAttemptRevoked
	case auth.Access != AccessWrite:
		return ErrNotWriter
	}
	return nil
}

// SettleWriter ends a writer attempt after its publication settled and its
// workload/result settled. A published commit of an attempt that did not
// succeed is retained as history but never becomes a successful output.
func (s *Service) SettleWriter(ctx context.Context, in SettleWriterInput) (*Workspace, error) {
	in.Actor = system
	agg, err := s.d.Store.Update(ctx, in.Scope, in.WorkspaceID, Load{Receipt: in.AttemptID}, SettleWriterMutation(in, s.now))
	if err != nil {
		return nil, err
	}
	if agg.Receipt != nil {
		_ = s.d.Bundles.Settled(ctx, in.Scope, agg.Receipt.ArtifactID)
	}
	return agg.Workspace, nil
}
