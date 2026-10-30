package results

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/diag"
	"github.com/optimiware/kodama/internal/domain"
)

// Dependencies are the consumer-owned ports of a Service. Every one is
// required: a missing port fails construction rather than loosening a check.
type Dependencies struct {
	Authority Authority
	Contexts  Contexts
	Evidence  Evidence
	Finalizer Finalizer
	Source    Source
	// V3 enables the source-set result route (SubmitV3). Without it SubmitV3 is
	// unavailable; the v1 and v2 routes are unchanged either way.
	V3 *DependenciesV3
}

// Options configure a Service.
type Options struct {
	// Logger receives only operation names, safe identifiers and classified
	// outcomes. It defaults to a discard logger.
	Logger *slog.Logger
}

// Service is the runtime result and source use-case layer. It owns no durable
// state: execution owns attempt and result transitions, the workspace owns
// publication receipts, and the artifact module owns bytes and provenance.
type Service struct {
	d Dependencies
	l *slog.Logger
}

// NewService validates the dependencies.
func NewService(d Dependencies, o Options) (*Service, error) {
	switch {
	case d.Authority == nil:
		return nil, errors.New("results: an authority is required (no allow-all fallback)")
	case d.Contexts == nil:
		return nil, errors.New("results: a pinned-context reader is required")
	case d.Evidence == nil:
		return nil, errors.New("results: an artifact evidence reader is required")
	case d.Finalizer == nil:
		return nil, errors.New("results: a result finalizer is required")
	case d.Source == nil:
		return nil, errors.New("results: a source coordinator is required")
	}
	if d.V3 != nil && (d.V3.Contexts == nil || d.V3.Pushes == nil || d.V3.Issued == nil || d.V3.Finalizer == nil) {
		return nil, errors.New("results: the source-set route needs pinned contexts, push records, issued grants and a finalizer (no allow-all fallback)")
	}
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{d: d, l: l}, nil
}

// SubmitCommand is one POST /result call.
type SubmitCommand struct {
	// Attempt is the attempt named by the route.
	Attempt domain.AttemptID
	Bearer  string
	Request runtimev1.ResultRequest
}

// Submit accepts a final result once. The order is frozen:
//
//  1. verify the bearer and the route binding;
//  2. validate the normalized request and recompute the canonical hash;
//  3. let execution resolve an already accepted hash first (identical:
//     the original acknowledgment; different: result_conflict), so an artifact
//     outage or retention never replaces a receipt decision;
//  4. for a new result, validate artifact evidence against the pinned context;
//  5. protect consumed evidence, then commit through execution, which repeats
//     the session check under its run lock and enforces the deadline, terminal
//     state, publication and writer settlement.
//
// Success is never inferred from process exit or logs. An unknown commit or
// acknowledgment-read outcome is ErrUnavailable; the retry observes the same
// receipt.
func (s *Service) Submit(ctx context.Context, cmd SubmitCommand) (resp runtimev1.ResultResponse, err error) {
	defer func() { err = Classify(err); s.log(ctx, "submitResult", cmd.Attempt, err) }()
	p, err := s.authenticate(ctx, cmd.Attempt, cmd.Bearer, ActionResultSubmit)
	if err != nil {
		return resp, err
	}
	if ctx, err = s.d.Authority.Bind(ctx, p); err != nil {
		return resp, err
	}
	if err := cmd.Request.Validate(); err != nil {
		return resp, contractError(err)
	}

	commit := Commit{Scope: p.Scope, AttemptID: p.AttemptID, Request: cmd.Request}
	// An already accepted attempt (same or different hash) goes straight to
	// execution, which answers the original acknowledgment or result_conflict
	// without mutation; only a new result needs artifact evidence.
	_, found, err := s.d.Finalizer.Accepted(ctx, p.Scope, p.AttemptID)
	if err != nil {
		return resp, err
	}
	if !found {
		verdict, err := s.validate(ctx, p, cmd.Request.Result)
		if err != nil {
			return resp, err
		}
		commit.Artifacts = verdict.Artifacts
		if err := s.d.Evidence.Protect(ctx, p.Scope, p.AttemptID, verdict.Protect); err != nil {
			return resp, err
		}
	}
	ack, err := s.d.Finalizer.Commit(ctx, commit)
	if err != nil {
		return resp, err
	}
	if ack.AttemptID != p.AttemptID || ack.ResultHash != cmd.Request.ResultHash {
		return runtimev1.ResultResponse{}, Fail(ErrUnavailable, nil, "the acknowledgment does not match the request")
	}
	resp = runtimev1.ResultResponse{
		AttemptID: ack.AttemptID, ResultHash: ack.ResultHash, AttemptStatus: ack.Status,
		AcceptedAt: runtimev1.Timestamp(ack.AcceptedAt.UTC().Format(time.RFC3339Nano)),
	}
	if err := resp.Validate(); err != nil {
		// A response that violates the contract is never delivered.
		return runtimev1.ResultResponse{}, Fail(ErrUnavailable, err, "the acknowledgment is not valid")
	}
	return resp, nil
}

func (s *Service) validate(ctx context.Context, p Principal, res runtimev1.Result) (Verdict, error) {
	pinned, err := s.d.Contexts.Pinned(ctx, p.Scope, p.AttemptID)
	if err != nil {
		return Verdict{}, err
	}
	if pinned.Scope() != p.Scope || pinned.AttemptID != p.AttemptID {
		return Verdict{}, Fail(ErrUnavailable, nil, "the pinned context does not belong to the attempt")
	}
	var evidence map[domain.ArtifactID]ArtifactEvidence
	if ids := ReferencedArtifacts(res, pinned); len(ids) > 0 {
		if evidence, err = s.d.Evidence.References(ctx, p.Scope, p.AttemptID, ids); err != nil {
			return Verdict{}, err
		}
	}
	return Validate(res, pinned, evidence)
}

// ProposeCommand is one POST /source call.
type ProposeCommand struct {
	Attempt domain.AttemptID
	Bearer  string
	Request runtimev1.SourceProposalRequest
}

// ProposeSource coordinates one source proposal. It authenticates source.propose
// and hands the proposal, scoped by the grant, to the workspace owner, which
// persists the pending publication before any Git call and keeps ownership of
// an uncertain publication. A pending outcome (202) must settle before the
// attempt can succeed; a repeat of the identical proposal observes the same
// publication. Publication credentials never reach the workload.
func (s *Service) ProposeSource(ctx context.Context, cmd ProposeCommand) (out SourceOutcome, err error) {
	defer func() { err = Classify(err); s.log(ctx, "proposeSource", cmd.Attempt, err) }()
	p, err := s.authenticate(ctx, cmd.Attempt, cmd.Bearer, ActionSourcePropose)
	if err != nil {
		return out, err
	}
	if ctx, err = s.d.Authority.Bind(ctx, p); err != nil {
		return out, err
	}
	if err := cmd.Request.Validate(); err != nil {
		return out, Fail(ErrInvalidRequest, err, "the request is not valid for this operation")
	}
	// The pending bundle is protected before the intent is recorded, and stays
	// protected (an unknown outcome included) until the publication settles.
	if err := s.d.Evidence.Protect(ctx, p.Scope, p.AttemptID, []domain.ArtifactID{cmd.Request.BundleArtifactID}); err != nil {
		return SourceOutcome{}, err
	}
	out, err = s.d.Source.Propose(ctx, p.Scope, p.AttemptID, cmd.Request)
	if err != nil {
		return SourceOutcome{}, err
	}
	if err := out.Response.Validate(); err != nil || (out.HTTPStatus != 200 && out.HTTPStatus != 202) {
		return SourceOutcome{}, Fail(ErrUnavailable, err, "the publication outcome is not known; repeat the proposal")
	}
	return out, nil
}

func (s *Service) authenticate(ctx context.Context, attempt domain.AttemptID, bearer string, action Action) (Principal, error) {
	if err := attempt.Validate(); err != nil {
		return Principal{}, Fail(ErrUnauthenticated, err, "the token is not valid for this attempt")
	}
	p, err := s.d.Authority.Authenticate(ctx, AuthRequest{Attempt: attempt, Bearer: bearer, Action: action})
	if err != nil {
		return Principal{}, err
	}
	// The grant, not the request, names the attempt: a principal for another
	// attempt or without a complete scope is never used.
	if p.AttemptID != attempt || p.Action != action || p.Scope.Validate() != nil {
		return Principal{}, Fail(ErrUnauthenticated, nil, "the token is not valid for this attempt")
	}
	return p, nil
}

func (s *Service) log(ctx context.Context, op string, attempt domain.AttemptID, err error) {
	level, outcome := slog.LevelInfo, "ok"
	if err != nil {
		outcome = "denied"
		if errors.Is(err, ErrUnavailable) {
			level, outcome = slog.LevelWarn, "unavailable"
		}
	}
	attrs := []slog.Attr{slog.String("op", op), slog.String("outcome", outcome)}
	if detail := diag.Describe(err); detail != "" {
		attrs = append(attrs, slog.String("detail", detail))
	}
	if attempt.Validate() == nil {
		attrs = append(attrs, slog.String("attemptId", string(attempt)))
	}
	s.l.LogAttrs(ctx, level, "runtime results", attrs...)
}
