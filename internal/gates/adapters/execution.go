package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/pipeline"
)

// ConformanceMapping identifies the design conformance evidence of an
// agent-produced candidate from immutable run data. It does not guess a file
// name and never lets an agent select the required checks:
//
//   - the expected baseline (and with it the required checks) is the one pinned
//     in the immutable context of the successful producing attempt, built by the
//     control plane from the pinned profile and specification;
//   - the report is the artifact output the producer's frozen step declares under
//     ReportOutput (type artifact) and its accepted result carries;
//   - the published commit is the producer's accepted result, which binds the
//     report to the candidate it assessed.
//
// An absent declaration, output or baseline yields a zero or partial
// expectation, which blocks approval; nothing is substituted.
type ConformanceMapping struct {
	// ReportOutput is the declared output name of the report artifact. It is
	// installation configuration agreed with T-031; empty identifies nothing.
	ReportOutput string
}

var _ gates.Conformance = ConformanceMapping{}

// Expected implements gates.Conformance. It resolves exactly the one successful
// attempt of the candidate producer and takes everything from that attempt:
// its immutable pinned context (verified against its digest, scope and
// identity), its accepted publication and its declared report output. The
// report's own claims and the latest profile are never substituted, and an
// ambiguous or inconsistent record is an error that blocks approval.
func (m ConformanceMapping) Expected(_ context.Context, agg *execution.Aggregate, cand execution.GateCandidate) (gates.ConformanceExpectation, error) {
	var exp gates.ConformanceExpectation
	producer := agg.Step(cand.Producer)
	if producer == nil {
		return exp, errors.New("the candidate producer is not a step of the run")
	}
	accepted, err := gates.AcceptedAttempt(agg, cand.Producer)
	if err != nil {
		return exp, err
	}
	if accepted.Result == nil || accepted.Result.SourceCommit == nil {
		return exp, errors.New("the producer has no accepted publication")
	}
	exp.ProducerAttempt = accepted.ID
	exp.PublishedCommit = string(*accepted.Result.SourceCommit)

	sum := sha256.Sum256(accepted.Context)
	if accepted.ContextDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		return exp, errors.New("the pinned context does not match its digest")
	}
	var ctx runtimev1.Context
	if err := json.Unmarshal(accepted.Context, &ctx); err != nil {
		return exp, errors.New("the pinned context is unreadable")
	}
	if ctx.Scope() != agg.Run.Scope || ctx.RunID != accepted.RunID || ctx.StepID != accepted.StepID || ctx.AttemptID != accepted.ID {
		return exp, errors.New("the pinned context belongs to another attempt")
	}
	if ctx.DesignBaseline != nil {
		b := *ctx.DesignBaseline
		exp.Baseline = &b
	}
	if m.ReportOutput != "" {
		if out, ok := producer.Spec.Outputs[m.ReportOutput]; ok && out.Type == pipeline.TypeArtifact {
			v, ok := producer.Outputs[m.ReportOutput]
			acc, accOK := accepted.Result.Outputs[m.ReportOutput]
			if ok && v.IsString() && accOK && acc.IsString() && v.String() == acc.String() {
				exp.ReportID = domain.ArtifactID(v.String())
			}
		}
	}
	return exp, nil
}
